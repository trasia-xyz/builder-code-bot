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

零点 claimable 是与两类实际 payout 合计对账的唯一数值基准；claim 接受状态是独立的执行结果。
某个 builder 的 claimable 查询全部失败时标记为数据缺失，在取得余额基线后仍会尝试一次 claim，
但当日不能得出完整数值对账结论。Claim 在有限观察后仍不确定时保留 warning，不会改写已落盘的零点 claimable，
也不阻止其他已捕获数据参与合计。

### UTC 01:00：数据库 Payout

一点任务只处理数据库批次与最终付款：

1. 从 `trasia_points_rebate_funding_record` 冻结全部 `status = 0` records；同时从
   `trasia_referral_funding_record` 冻结满足 `cycle_type IN (1, 2) AND status = 0` 的 records。
   两张表均以 `0` 表示 pending、`1` 表示 complete。
2. 两个 target 分别精确汇总 raw total，并分别执行六位向上取整；数据库 payout total 是两个取整结果
   之和，不能先合并 raw total 再取整。
3. 在第一笔发送前连续读取稳定的 settlement `Total/Available`，并按两个 target 的 payout total
   合计检查资金充足性；不足时一笔也不发送。可能出现的 1 USDC 新地址费用只参与提交后的精确余额
   判断，不计入数据库 payout total 或零点 Claimable 对账。
4. 固定按 `PointsRebatePayout`、`ReferralPayout` 的顺序执行。每个 target 都取得并持久化自己的
   settlement 基线与 payout journal；开始第二笔前还要确认第一笔对应的 settlement 精确余额边界。
5. 每笔 payout 确认后立即将该 target 冻结的 records 标记完成。第二笔失败不会回滚或重发已经完成的
   第一笔；非终态错误恢复时从第一个未完成 target 继续，最终 blocked 则停止。
6. 读取同一 UTC 日期的零点归集记录，生成最终报告。

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
新 nonce 重试；首次提交结果不确定时，只允许重放一次完整持久化的原请求。

## 3. `--consolidate-only` 隔离

`--consolidate-only` 执行一次内部归集后退出。它不连接 MySQL；不读写 funding records、
`current`、backup、history 或 `daily-consolidations`；不发送 recipient payout 或最终报告；
只解密 builder 私钥。它仍获取同一 `data/LOCK`，因此不能与常驻服务并行运行。

## 4. 余额语义

Spot 查询返回：

- `Total`：链上 spot 总余额；
- `Available = Total - Hold`：当前可发送余额。

Builder sweep 金额和 payout 资金充足性使用 `Available`。第一笔 Payout 提交前必须用稳定的
settlement Available 对两个正金额 target 的 payout total 合计做预检。每个 target
提交前还必须连续取得
稳定的 settlement `Total/Available` 基线；journal 保存 settlement 的提交前 total。第二笔基线必须
与第一笔确认后的精确 settlement 边界一致。

结果不确定时只观察发送方 settlement，不查询 recipient 余额。Settlement Total 连续两次精确满足
以下任一边界才确认付款：

- `TotalBefore - Amount`；
- `TotalBefore - Amount - 1 USDC`，兼容首次向新地址转账可能额外扣除的 1 USDC。

不能只判断“低于付款前余额”，其他差额、Hold 变化或较低历史快照都不视为付款成功。Recipient 收款
后可能立即发起后续转账，因此其余额既不参与确认，也不出现在确认依据中。

该确认规则要求 settlement 为本服务专用出账账户，人工或其他程序不得从该账户发送 spot USDC。
观察窗口内若 settlement 出现其他并发收支，精确边界可能无法成立，系统会保守保持
blocked 而不会把数据库 records 标记完成；此时按运维流程人工核查。

## 5. Records 与金额

两个 target 分别维护 Manifest。Points rebate records 与 referral records 都按
`period_start_at, id` 排序，referral 的查询、完成与状态复核始终限定 `cycle_type IN (1, 2)`。
每个 Manifest 独立冻结 records、精确 raw total、六位向上取整后的 payout total、canonical token、
settlement 和自己的 recipient。Builder 列表属于当前配置，不属于 payout 恢复数据。

每个 target 都先按 decimal 文本精确汇总，再各自执行
`ceil(raw_total × 10^6) / 10^6`；aggregate payout 是两个已取整结果之和。负金额归档为
`failed_validation` 并 fatal 退出；某个 target 零总额不执行链上付款，直接完成该 target 的冻结
records。两个 target 都无 pending records 时不创建 current 或 history。

## 6. Payout 持久化边界

两个 Settlement → recipient transfer 是对外资金边界；每个 target 都有相互独立的 at-most-once
防护，执行顺序固定为 `PointsRebatePayout` 后 `ReferralPayout`。

发送前验证完整可重放请求与 manifest 一致，然后依次：

1. 保存 `payout_prepared`（完整请求与付款前 settlement total）。
2. 保存 `payout_submitting`。
3. 第二次保存成功后才调用 submit。

完整请求包括原始 request body、nonce、签名、action time 与 request hash。任何 exact replay 必须逐字节
复用该请求，禁止重新 Prepare、重新签名或取得新 nonce。首次网络调用前先将 `submit_attempts = 1`
与 `payout_submitting` 持久化，表示首次提交已授权、可能已发送；重放调用前先将
`submit_attempts = 2` 与 `payout_replaying` 连续保存两代，使 current 与 backup 都耗尽重放预算，再调用
Submit；该状态表示唯一一次 exact replay 已授权、可能已发送。

| 结果 | 行为 |
| --- | --- |
| 明确成功 | 保存 `payout_confirmed`，随后无限重试 MySQL Complete |
| 首次响应明确拒绝，且此前没有不确定结果 | 归档 `rejected`、清理 current、fatal 退出，不自动重发 |
| 首次提交不确定 | 先观察 settlement；确认精确扣减则成功；完整观察窗全部保持付款前 Total 时 exact replay 原请求一次；查询不足或其他变化则直接 blocked |
| exact replay 明确成功 | 保存 `payout_confirmed`，随后无限重试 MySQL Complete |
| exact replay 返回 duplicate nonce、其他 err、超时、网络错误或不明确结果 | 一律不视为首次请求失败；再次只读观察 settlement，仍无法确认则 blocked |

首次观察耗尽后最多重放一次原请求；第二次观察仍无法判断时保存 `blocked`、归档现场、保留 current，
发送包含首次提交、exact replay 和最终证据的终态报告，然后结束本次任务。系统绝不进行第三次 Submit，
也绝不构造新请求。若
`PointsRebatePayout` 已完成而 `ReferralPayout` 失败，前者保持 completed，其数据库 records 已完成，
恢复不会回滚或重新发送前者。

## 7. Phase 与恢复

下表适用于当前未完成 target；已完成 target 的 phase 为 `completed`，恢复时直接跳过。

| Phase | 恢复行为 |
| --- | --- |
| `prepared` | 重新检查 settlement 并继续 payout，不执行 builder 动作 |
| `payout_prepared` | 提交尚未开始，发送已持久化请求 |
| `payout_submitting`（`submit_attempts = 1`） | 先观察 settlement；未确认时持久化 replay 边界并重放原请求一次 |
| `payout_replaying`（`submit_attempts = 2`） | 只读观察 settlement，绝不再次 Submit |
| `payout_confirmed` | 校验 journal、请求和 settlement 边界后，只重试数据库 Complete |
| ambiguous `blocked` | 保持终态并报告，绝不重发 |
| 其他 `blocked` | 立即 fatal，等待人工判断 |
| `completed` | 不再发送、不再回滚，继续下一个 target |

每个 target 的 MySQL Complete 成功后立即保存其 `completed` phase。两个 target 都 completed 后才
归档整轮 `completed` 并清理 current。Complete 与 target phase 落盘之间崩溃时，恢复会幂等再次
Complete；第一笔 completed、第二笔未完成时只继续第二笔。

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

每日归集记录供报告与人工检阅，不参与 payout at-most-once 协议。同一零点任务先写
`claimables_captured`，完成后更新为 `completed`；进程若在两次写入之间中断，一点报告仍可读
已冻结 claimable，并标记 claim/sweep 未完成。当日文件已存在时，调度器不会自动重跑零点动作，
以避免在崩溃边界重复提交不确定的 claim。

## 9. 调度、报告与失败

调度器串行安排 UTC 00:00 consolidation 与 UTC 01:00 funding。零点顶层失败只记录错误，继续
等待一点；一点普通失败有限次重试。Fatal payout 或 records 校验错误立即退出。Context 取消
立即停止。

最终 funding report 只在 UTC 01:00 funding 结束时发送，内容包括：各 builder 的零点
claimable 与 claim/sweep 结果、两个 target 各自的 raw/payout total 和状态、完整 claimable 合计与
两类实际 payout 合计的差额、各笔首次提交是否发生、是否发生一次 exact replay、最终发送方确认依据
或 blocked 结果、settlement 结束余额、rate limit、告警与处置
建议。正数 payout 确认后，结束余额必须连续两次
达到 `total before - amount` 或 `total before - amount - 1 USDC`，且两次 available 也相同，才可标记
为已观察；查询错误不消耗成功样本
预算。有限观察内仍不可见时只报告未确认，不得把付款前余额展示为结束余额。
Payout 结果本身仍不确定时同样不再追加一次普通余额查询，也不展示未经边界确认的 Total/Available。
对本轮已提交或从持久化 phase 判断为可能已提交的账户，若首次 Rate Limit 落在告警阈值附近，
同样延迟重查；仍无法证明动作后计数可见时按 warning 展示最低观测值，不标成健康终值。

全部 builder 的零点 claimable 都成功捕获时才做数值对账；合计低于两类实际 payout 之和时报告为 warning，
但 settlement 余额充足时仍正常付款。Claim 不确定单独告警，不改变已冻结的 claimable 口径。
`--run-on-start` 立即执行一点侧的数据库 payout，不补做错过的零点归集。

## 10. 安全不变量

1. 每个 target 最多首次 Submit 一次并 exact replay 同一请求一次，且固定先 PointsRebate、后 Referral。
2. 第一笔发送前必须确认 settlement Available 足以覆盖两个 target 已分别取整后的 payout 合计。
3. 每笔完整 payout 请求和“可能已发送”边界都在该笔发送前持久化。
4. Exact replay 必须复用完全相同的 request body、nonce 与签名；禁止构造新请求或第三次 Submit。
5. 第二笔失败不得回滚、重置数据库状态或重发已完成的第一笔。
6. UTC 01:00 及其重试、恢复路径都不得执行 builder claim 或 sweep。
7. UTC 00:00 与 `--consolidate-only` 使用同一内部归集实现。
8. `--consolidate-only` 不得读写零点每日归集记录。
9. 每日归集记录只用于报告，不参与 payout at-most-once 协议。
10. 零点 claimable 必须在任何 claim 提交前落盘；后续动作不得修改其数值。
11. Payout 确认状态必须通过 journal、请求与 settlement 边界校验，之后只允许对应数据库完成和归档。
