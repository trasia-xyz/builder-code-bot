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
4. 对符合条件的 builder 并行提交 `claimRewards`；结果不确定时换新 nonce 有限次重试。
5. 等待已提交 claim 的 spot 余额可见，再把每个 builder 正 available 余额全部 sweep 到
   settlement。
6. 将 claim/sweep 结果写回当日文件，状态更新为 `completed`。

零点 claimable 是与数据库 payout 对账的唯一数值基准；claim 接受状态是独立的执行结果。
某个 builder 的 claimable 查询全部失败时标记为数据缺失，仍会尝试 claim，但当日不能得出完整
数值对账结论。Claim 在有限重试后仍不确定时保留 warning，不会改写已落盘的零点 claimable，
也不阻止其他已捕获数据参与合计。

### UTC 01:00：数据库 Payout

一点任务只处理数据库批次与最终付款：

1. 从 MySQL 冻结全部 `status = 0` records。
2. 计算精确 raw total 和六位向上取整的 payout total。
3. 查询 settlement available；余额充足时向固定 recipient 发送唯一一笔 payout。
4. payout 确认后将冻结 records 标记完成。
5. 读取同一 UTC 日期的零点归集记录，生成最终报告。

一点任务不查询 builder claimable、不 claim、不 sweep。Settlement 不足属于普通错误，调度器
有限次重试，且重试仍只执行 payout 阶段。

## 2. 内部归集规则

Claimable 只取 referral info 中 canonical USDC 对应项的 `unclaimedRewards`，不使用外层多币种
折算汇总。成功读取且金额严格超过 1 USDC 才正常 claim；等于或低于阈值则跳过。若 claimable
查询最终失败，因无法判断阈值，为避免漏领会执行一次受相同上限保护的 claim，同时将对账数据
标记为不完整。

同一轮 builder claim 并行提交，各 builder 重试状态独立。一次不确定提交之后，后续明确拒绝
仍不能证明首次未执行，整体保持不确定；任一次明确接受则确认完成。若存在已接受或仍不确定的
claim，等待余额可见后再按最新 available 归集到 settlement。Sweep 对每个 builder 有限次重试：
明确接受即停止；无更早不确定结果时明确拒绝不重试；结果不确定时换新 nonce 重试，并在重试前
重新读取 available——若余额已清空则视为上次不确定提交已生效。Sweep 仍只移动运营方控制账户间
资金，不使用 payout journal，也不触及 settlement→recipient 付款。

零点任务与 `--consolidate-only` 共用同一套 claim、可见性等待和有限次 sweep。单账户 claim/sweep
失败会告警，但不阻止其他 builder。

## 3. `--consolidate-only` 隔离

`--consolidate-only` 执行一次内部归集后退出。它不连接 MySQL；不读写 funding records、
`current`、backup、history 或 `daily-consolidations`；不发送 recipient payout 或最终报告；
只解密 builder 私钥。它仍获取同一 `data/LOCK`，因此不能与常驻服务并行运行。

## 4. 余额语义

Spot 查询返回：

- `Total`：链上 spot 总余额；
- `Available = Total - Hold`：当前可发送余额。

Builder sweep 金额和 payout 资金充足性使用 `Available`。Payout journal 保存提交前 settlement
`Total`；结果不确定时只比较 `TotalAfter < TotalBefore`。Hold 变化不视为付款成功。

该确认规则要求 settlement 为本服务专用出账账户，人工或其他程序不得从该账户发送 spot USDC。

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

1. 保存 `payout_prepared`（完整请求与付款前 settlement total）。
2. 保存 `payout_submitting`。
3. 第二次保存成功后才调用 submit。

| 结果 | 行为 |
| --- | --- |
| 明确成功 | 保存 `payout_confirmed`，随后无限重试 MySQL Complete |
| 明确拒绝 | 归档 `rejected`、清理 current、fatal 退出，不自动重发 |
| 不确定 | 有限次观察 settlement total；任一下降即确认成功 |

观察耗尽仍无法判断时保存 `blocked`、归档现场、保留 current 并 fatal 退出。系统绝不自动重发
可能已经发送的 payout。

## 7. Phase 与恢复

| Phase | 恢复行为 |
| --- | --- |
| `prepared` | 重新检查 settlement 并继续 payout，不执行 builder 动作 |
| primary `payout_prepared` | 提交尚未开始，发送已持久化请求 |
| backup `payout_prepared` | primary 可能已提交，只观察 total |
| `payout_submitting` | 只观察 total |
| `payout_confirmed` | 只重试数据库 Complete |
| `blocked` | 立即 fatal，等待人工判断 |

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
settlement 结束余额、rate limit、告警与处置建议。正数 payout 确认后，结束余额必须等到
settlement total 相比付款前下降才可标记为已观察；有限观察内仍不可见时只报告未确认，不得把
付款前余额展示为结束余额。

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
9. Payout 确认后只允许数据库完成和归档。
