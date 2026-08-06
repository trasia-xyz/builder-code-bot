# 架构说明

本文描述 Hyperliquid Builder Code Bot 的资金流程、恢复模型和安全不变量。部署与故障处置见
[运维指南](operations.md)。

## 1. 两阶段日任务

每天的工作按 UTC 日期拆成两个阶段。

### UTC 00:00：Claim 与资金归集

零点任务不连接 MySQL，也不读取或修改 payout `current` 状态：

1. 查询 canonical USDC。
2. 并行查询所有 builder 的 canonical-USDC claimable reward；仅重试失败项。
3. 将零点 claimable 以 `claimables_captured` 原子写入当日文件；落盘成功后才继续。
4. 连续两次查询符合条件的 builder 的 claim 前 `Total/Available` 稳定基线；基线无法取得时不提交
   claim。
5. 并行提交一次 `claimRewards`。结果不确定时不换 nonce 重发，只观察动作关联状态。
6. Claimable 已捕获时，将其按 token 精度截断为最低增量，要求 claim 后 `Total` 不低于基线加
   该增量；同一完整余额连续两次满足才视为已刷新。明确接受的响应作为动作证据；不确定提交还
   必须先在提交后看到完整的 claim 前余额并看到 `ClaimableUSDC` 明确下降。Claimable 缺失时，
   Total-only 不能解除不确定状态。
7. 每笔 sweep 前重新连续读取稳定的 builder `Total/Available`，按最新 available 提交。明确接受
   后直接完成该 builder，不立即查询余额。结果不确定时等待后重读稳定的当前 Available，并用新
   nonce 最多提交三次；该 builder 最终失败也不阻断其他 builder。
8. 将 claim/sweep 结果写回当日文件，状态更新为 `completed`。

零点 claimable 是与数据库 payout 对账的唯一数值基准；claim 接受状态是独立的执行结果。
某个 builder 的 claimable 查询全部失败时标记为数据缺失，在取得余额基线后仍会尝试一次 claim，
但当日不能得出完整数值对账结论。Claim 在有限观察后仍不确定时保留 warning，不会改写已落盘的零点 claimable，
也不阻止其他已捕获数据参与合计。

### UTC 01:00：数据库 Payout

一点任务只处理数据库批次与最终付款：

1. 从 MySQL 冻结全部 `status = 0` records。
2. 计算精确 raw total 和六位向上取整的 payout total。
3. 连续读取稳定的 settlement `Total/Available` 与 recipient `Total` 基线；余额充足时向固定 recipient
   发送唯一一笔 payout。
4. payout 确认后将冻结 records 标记完成。
5. 读取同一 UTC 日期的零点归集记录，生成最终报告。

一点任务不查询 builder claimable、不 claim、不 sweep。Settlement 不足属于普通错误，调度器
有限次重试，且重试仍只执行 payout 阶段。

## 2. 内部归集规则

Claimable 只取 referral info 中 canonical USDC 对应项的 `unclaimedRewards`，不使用外层多币种
折算汇总。成功读取且金额严格超过 1 USDC 才正常 claim；等于或低于阈值则跳过。若 claimable
查询最终失败，因无法判断阈值，为避免漏领会执行一次受相同上限保护的 claim，同时将对账数据
标记为不完整。

同一轮 builder claim 并行且每个 builder 只提交一次。明确接受证明 claim 动作成功，程序再等待
相对 `Total` 下界连续成立；后续新 Reward 立即回补不会把已接受动作误报为失败。不确定结果还要求
先看到提交后的 claim 前完整余额与 Claimable 下降，不再换 nonce。观察耗尽时将 Claim 本身持久化
为 warning，并按最后可见余额做 best-effort 归集，不能冒充 Reward 已完整可见。

Sweep 金额使用提交前重新稳定读取的 `Available`。明确接受的响应直接确认本次内部归集，不再执行
容易读到旧快照的提交后余额查询。Builder 与 settlement 都是运营方账户，因此不确定 sweep 可以
有限重试：每次先等待，再重读连续稳定的 Builder 余额，按最新正 Available 和新 nonce 提交。若重读
得到低于上一笔不确定 amount 的余额，为避免历史低值快照造成部分归集或漏归集，会至少使用上一笔
不确定请求的 amount 再试一次；若此前已成功，该请求只会因余额不足被拒绝，不会产生对外资金风险。

首次明确拒绝不重试；出现过不确定响应后，后续拒绝不能反证此前请求未执行，因此在剩余次数内继续。
三次后仍无明确接受则保留 warning，且不虚构 SweepAmount。若后续重试明确接受，也保留 warning：
SweepAmount 只记录该次 accepted 金额，此前不确定请求是否入账及累计归集金额仍无法确认。任一 builder
的失败都不阻断其他 builder。Claim 已确认的 builder `Total` 仅作为首次 sweep 查询的下界；进入
不确定重试后允许余额下降。

零点任务与 `--consolidate-only` 共用同一套 claim 可见性边界和内部 sweep 有限重试。Payout 不使用
这套重试规则，仍严格禁止对不确定请求重发。

## 3. `--consolidate-only` 隔离

`--consolidate-only` 执行一次内部归集后退出。它不连接 MySQL；不读写 funding records、
`current`、backup、history 或 `daily-consolidations`；不发送 recipient payout 或最终报告；
只解密 builder 私钥。它仍获取同一 `data/LOCK`，因此不能与常驻服务并行运行。

## 4. 余额语义

Spot 查询返回：

- `Total`：链上 spot 总余额；
- `Available = Total - Hold`：当前可发送余额。

Builder sweep 金额和 payout 资金充足性使用 `Available`。Payout 提交前必须连续取得稳定的
settlement `Total/Available` 与 recipient `Total` 基线；journal 同时保存 settlement
与 recipient 的提交前 total。结果不确定时，先要求
提交后看见 settlement 前态，再连续两次同时满足：settlement `Total == TotalBefore - Amount`，
recipient `Total == RecipientTotalBefore + Amount`。Hold 变化、任意单边或较低历史快照都不视为
付款成功。

该确认规则要求 settlement 为本服务专用出账账户，人工或其他程序不得从该账户发送 spot USDC。
观察窗口内若 settlement 或 recipient 出现并发入账，精确边界可能无法成立，系统会保守保持
blocked 而不会把数据库 records 标记完成；此时按运维流程人工核查。

## 5. Records 与金额

Pending records 按 `period_start_at, id` 排序。Manifest 冻结 records、精确 raw total、六位
向上取整后的 payout total、canonical token、settlement 和 recipient。Builder 列表属于当前
配置，不属于 payout 恢复数据。

金额先按 decimal 文本精确汇总，再执行一次 `ceil(raw_total × 10^6) / 10^6`。负金额归档为
`failed_validation` 并 fatal 退出；零总额不执行链上付款，直接完成冻结 records；无 pending
records 时不创建 current 或 history。

## 6. Payout 持久化边界

Settlement → recipient 是唯一对外资金边界，也是唯一需要 exactly-once 防护的动作。

发送前验证完整可重放请求与 manifest 一致，然后依次：

1. 保存 `payout_prepared`（完整请求、付款前 settlement total 与 recipient total）。
2. 保存 `payout_submitting`。
3. 第二次保存成功后才调用 submit。

不确定结果观察到提交后的 settlement 前态时立即连续保存 `payout_submitting`，使 primary 与 backup
都携带该证据，后续崩溃恢复不会丢失确认门槛。旧版 primary `payout_prepared` 若缺 recipient 基线，可在尚未 Submit 的安全
边界补查并持久化；backup/submitting/blocked 状态只能只读观察，不能事后补造基线。

| 结果 | 行为 |
| --- | --- |
| 明确成功 | 保存 `payout_confirmed`，随后无限重试 MySQL Complete |
| 明确拒绝 | 归档 `rejected`、清理 current、fatal 退出，不自动重发 |
| 不确定 | 先等待并双边观察；看到提交后前态且 settlement/recipient 连续两次精确达到各自预期才确认 |

观察耗尽仍无法判断时保存 `blocked`、归档现场、保留 current 并 fatal 退出。系统绝不自动重发
可能已经发送的 payout。后续恢复可以再次执行相同的只读观察，从而让延迟的余额索引自愈。

## 7. Phase 与恢复

| Phase | 恢复行为 |
| --- | --- |
| `prepared` | 重新检查 settlement 并继续 payout，不执行 builder 动作 |
| primary `payout_prepared` | 提交尚未开始，发送已持久化请求 |
| backup `payout_prepared` | primary 可能已提交，只读观察双边余额 |
| `payout_submitting` | 只读观察双边余额 |
| `payout_confirmed` | 校验 journal、请求和 settlement 边界后，只重试数据库 Complete |
| ambiguous `blocked` | 再次只读观察；确认后完成 DB，否则继续 blocked，绝不重发 |
| 其他 `blocked` | 立即 fatal，等待人工判断 |

MySQL Complete 成功后归档 `completed` 并清理 current。Complete 与归档之间崩溃时，恢复
`payout_confirmed` 后会幂等再次 Complete。

## 8. 本地状态

```text
data/
├── LOCK
├── daily-consolidations/
│   └── YYYY-MM-DD.json
├── current.json
├── current.json.bak
└── history/
```

目录权限 `0700`，文件 `0600`。`current` 与 backup 使用 checksummed envelope、原子替换、
`fsync` 和 backup 恢复协议。

每日归集记录供报告与人工检阅，不参与 payout exactly-once 协议。同一零点任务先写
`claimables_captured`，完成后更新为 `completed`；进程若在两次写入之间中断，一点报告仍可读
已冻结 claimable，并标记 claim/sweep 未完成。当日文件已存在时，调度器不会自动重跑零点动作，
以避免在崩溃边界重复提交不确定的 claim。

## 9. 调度、报告与失败

调度器串行安排 UTC 00:00 consolidation 与 UTC 01:00 funding。零点顶层失败只记录错误，继续
等待一点；一点普通失败有限次重试。Fatal payout 或 records 校验错误立即退出。Context 取消
立即停止。

最终 funding report 只在 UTC 01:00 funding 结束时发送，内容包括：各 builder 的零点
claimable 与 claim/sweep 结果、完整 claimable 合计与数据库 payout 的差额、payout 确认依据、
settlement 结束余额、rate limit、告警与处置建议。正数 payout 确认后，结束余额必须连续两次
达到精确的付款后 total，且两次 available 也相同，才可标记为已观察；查询错误不消耗成功样本
预算。有限观察内仍不可见时只报告未确认，不得把付款前余额展示为结束余额。
Payout 结果本身仍不确定时同样不再追加一次普通余额查询，也不展示未经边界确认的 Total/Available。
对本轮已提交或从持久化 phase 判断为可能已提交的账户，若首次 Rate Limit 落在告警阈值附近，
同样延迟重查；仍无法证明动作后计数可见时按 warning 展示最低观测值，不标成健康终值。

全部 builder 的零点 claimable 都成功捕获时才做数值对账；合计低于 payout 时报告为 warning，
但 settlement 余额充足时仍正常付款。Claim 不确定单独告警，不改变已冻结的 claimable 口径。
`--run-on-start` 立即执行一点侧的数据库 payout，不补做错过的零点归集。

## 10. 安全不变量

1. 一个冻结批次的 recipient payout 最多发送一次。
2. 完整 payout 请求和“可能已发送”边界都在发送前持久化。
3. 可能已发送或 blocked 的 payout 只观察或停止，绝不自动重发。
4. UTC 01:00 及其重试、恢复路径都不得执行 builder claim 或 sweep。
5. UTC 00:00 与 `--consolidate-only` 使用同一内部归集实现。
6. `--consolidate-only` 不得读写零点每日归集记录。
7. 每日归集记录只用于报告，不参与 payout exactly-once 协议。
8. 零点 claimable 必须在任何 claim 提交前落盘；后续动作不得修改其数值。
9. Payout 确认状态必须通过 journal、请求与 settlement 边界校验，之后只允许数据库完成和归档。
