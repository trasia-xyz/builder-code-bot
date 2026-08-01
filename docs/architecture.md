# 架构说明

本文描述 Hyperliquid Builder Code Bot 的资金流程、恢复模型和安全不变量。部署与故障处置见
[运维指南](operations.md)。

## 1. 两阶段日任务

每天的工作按 UTC 日期拆成两个边界清晰的阶段。

### UTC 00:00：Claim 与资金归集

零点任务不连接 MySQL，也不读取或修改 payout `current` 状态：

1. 查询 canonical USDC；失败时执行有限次短间隔重试。
2. 同时查询所有 builder 的 canonical-USDC claimable reward。只重试失败的 builder，每个最多
   查询 5 次，轮次间隔 200 ms。
3. 把零点 claimable 结果以 `claimables_captured` 状态原子写入当日文件。只有落盘成功才继续。
4. 对所有符合条件的 builder 并行提交 `claimRewards`。结果不确定时换新 nonce 重试，最多提交
   3 次，轮次间隔 250 ms；没有更早不确定结果时，明确成功或明确拒绝不重试。
5. 等待已提交 claim 的 spot 余额可见，然后把每个 builder 最新的正 available 余额全部
   sweep 到 settlement。
6. 将 claim/sweep 结果写回当日文件，并把状态更新为 `completed`。

零点 claimable 是与数据库 payout 对账的唯一数值基准；claim 接受状态是独立的执行结果。
某个 builder 的 `/info` 查询全部失败时，该 builder 标记为零点数据缺失，仍会尝试 claim，
但当日不能得出完整的数值对账结论。Claim 在有限重试后仍不确定时保留 warning，不会改写已经
落盘的零点 claimable，也不会阻止其他已成功捕获的数据参与合计。

### UTC 01:00：数据库 Payout

一点任务只处理数据库批次与最终付款：

1. 从 MySQL 冻结全部 `status = 0` records。
2. 计算精确 raw total 和六位向上取整的 payout total。
3. 查询 settlement available；余额充足时向固定 recipient 发送唯一一笔 payout。
4. payout 确认后将冻结 records 标记完成。
5. 读取同一 UTC 日期的零点归集记录，生成最终报告。

一点任务不会查询 builder claimable、不会 claim，也不会 sweep。Settlement 不足属于普通错误；
调度器一分钟后重新检查 settlement，最多自动重试五次，但重试仍然只执行 payout 阶段。

这两个阶段修复了旧实现的核心口径错误：旧实现零点记录 claimable，却在一点才真正 claim，
导致数据边界和资金动作相隔一小时。现在零点先并行冻结对账量并可靠落盘，紧接着在同一任务
执行 claim 与归集；一点只消费零点记录并付款。

## 2. 内部归集规则

Claimable 查询只使用 referral info 的 `tokenToState` 中 canonical USDC 对应项的
`unclaimedRewards`。最外层多币种折算汇总不参与判断。成功读取且金额严格超过 1 USDC 才
正常 claim；等于或低于阈值时跳过。若零点查询在 5 次后仍失败，因为无法判断阈值，为避免
漏领会执行一次受相同重试上限保护的 claim，同时将对账数据标记为不完整。`/info` 查询不消耗
账户的 exchange 请求额度；`/exchange` claim 会消耗额度，因此只有不确定提交才重复发送。

同一轮的 builder claim 并行提交，但每个 builder 的重试状态相互独立。一次不确定提交之后，
后续明确拒绝仍不能证明第一次没有执行，因此整体保持不确定；后续任一次明确接受则确认该
builder 的 claim 已完成。如果至少一个 claim 被接受或仍不确定，统一等待约 1 秒，然后开始
余额可见性查询。每个相关 builder 最多查询 10 次 spot balance；
`Available >= claimable` 时认为本次 reward 已可见。查询耗尽后产生 warning，并按最后一次
成功读取的 available 尽力归集；一次余额都未读取成功则跳过该 builder。零点 claimable 缺失
时使用 1 USDC 阈值作为可见性目标，仍不会把该值写入或冒充零点对账数据。

随后每个 builder 只执行一次 sweep 尝试。零点任务和 `--consolidate-only` 复用完全相同的
claim、可见性等待和单轮 sweep 实现。Claim 或 sweep 的单账户失败会告警，但不阻止其他
builder；这些内部动作只在运营方控制的账户间移动资金，因此不使用 payout journal。

## 3. `--consolidate-only` 隔离

`--consolidate-only` 只执行一次上述内部归集并退出。它：

- 不连接 MySQL；
- 不读取或修改 funding records、`current`、backup 或 history；
- 不读取、创建或覆盖 `daily-consolidations/YYYY-MM-DD.json`；
- 不构造 recipient payout，也不发送最终 funding report；
- 只解密 builder 私钥。

它仍获取与常驻服务相同的 `data/LOCK`，因此同一工作目录内不能和服务并行运行。即使手工执行
时间接近 UTC 00:00，也不会直接污染一点报告所使用的零点记录。

## 4. 余额语义

Spot 查询返回两个金额：

- `Total`：链上 spot 总余额；
- `Available = Total - Hold`：当前可发送余额。

Builder sweep 金额和 payout 资金充足性只使用 `Available`。Payout journal 保存提交前
settlement `Total`；提交结果不确定时只比较 `TotalAfter < TotalBefore`。Hold 变化不会被误判
为付款成功。

“total 下降即成功”要求 settlement 是本服务专用出账账户。人工或其他程序不得从该账户发送
spot USDC。

## 5. Records 与金额

Pending records 按 `period_start_at, id` 排序。Manifest 冻结 records、精确 raw total、六位
向上取整后的 payout total、canonical token、settlement 和 recipient。Builder 列表属于当前
配置，不属于 payout 恢复数据。

金额先按 decimal 文本精确汇总，再执行一次
`ceil(raw_total × 10^6) / 10^6`。负金额归档为 `failed_validation` 并 fatal 退出；零总额不执行
链上付款，直接完成冻结 records；没有 pending records 时不创建 current 或 history。

## 6. Payout 持久化边界

Settlement 到 recipient 是唯一对外资金边界，也是唯一需要 exactly-once 防护的动作。

发送前先验证完整可重放请求与 manifest 一致，然后依次：

1. 保存 `payout_prepared`，包含完整请求和付款前 settlement total。
2. 保存 `payout_submitting`。
3. 只有第二次保存成功后才调用 submit。

提交结果分三类：

| 结果 | 行为 |
| --- | --- |
| 明确成功 | 保存 `payout_confirmed`，随后无限重试 MySQL Complete |
| 明确拒绝 | 归档 `rejected`、清理 current、fatal 退出，不自动重发 |
| 不确定 | 最多观察 settlement total 5 次；任何下降都确认成功 |

有限观察后仍无法判断时保存 `blocked`、归档现场、保留 current 并 fatal 退出。系统绝不自动
重发一个可能已经发送的 payout。

## 7. Phase 与恢复

| Phase | 恢复行为 |
| --- | --- |
| `prepared` | 只重新检查 settlement 并继续 payout，不执行 builder 动作 |
| primary `payout_prepared` | 证明提交尚未开始，发送持久化请求 |
| backup `payout_prepared` | 可能落后于已提交的 primary，只观察 total |
| `payout_submitting` | 只观察 total |
| `payout_confirmed` | 只重试数据库 Complete |
| `blocked` | 立即 fatal，等待人工判断 |

MySQL Complete 成功后归档 `completed` 并清理 current。Complete 与归档之间崩溃时，恢复
`payout_confirmed` 后会幂等地再次 Complete。

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

目录权限为 `0700`，文件为 `0600`。`current` 与 backup 使用 checksummed envelope、原子替换、
`fsync` 和 backup 恢复协议。

每日归集记录使用便于人工检阅的格式化 JSON，并保留 version 1 checksum envelope、同目录临时
文件、原子替换和 `fsync`。
同一零点任务先写 `claimables_captured`，完成后更新为 `completed`。进程若在两次写入之间中断，
一点报告仍可读取已冻结的 claimable，同时明确标记 claim/sweep 未完成。已经存在任一状态的
当日文件时，Scheduler 不会自动重跑零点动作；这避免在崩溃边界重复提交不确定的 claim。
该文件只供报告和人工检阅，不属于 payout 恢复协议，也没有旧格式迁移或兼容分支。

## 9. 调度、报告与失败

Scheduler 串行安排 UTC 00:00 consolidation 和 UTC 01:00 funding。零点顶层失败只记录错误，
继续等待一点；一点普通失败一分钟后重试，最多五次。Fatal payout 或 records 校验错误立即
退出。所有 context 取消都会立即停止。

最终 funding report 仍只在 UTC 01:00 funding 结束时发送。报告展示：

- 每个 builder 的零点 claimable、查询次数以及后续 claim/sweep 结果；
- 完整零点 claimable 合计与数据库 payout 的差额；
- payout journal 与确认依据；
- settlement 结束余额、rate limit、告警和处置建议。

全部 builder 的零点 claimable 都成功捕获时才进行数值对账；合计低于 payout 时报告升级为
warning，但 settlement 余额充足时仍正常付款。Claim 结果不确定会单独告警，不会改变已经冻结
的 claimable 对账口径。`--run-on-start` 立即执行一点侧的数据库 payout 流程；它不会代替已
错过的零点归集。

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
