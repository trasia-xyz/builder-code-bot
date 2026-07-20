# 架构说明

本文描述 Hyperliquid Builder Code Bot 的资金流程、恢复模型和安全不变量。部署与处置步骤见
[运维指南](operations.md)。

## 1. 系统目标与边界

服务在 UTC 01:00 运行，也可用 `--run-on-start` 在启动时立即运行。一次触发最多处理一个
funding run：存在 current 时只恢复该 run，不再连续创建新 run；没有 current 时才读取
pending records 并创建新 run。

1. 从 MySQL 冻结全部 `status = 0` records。
2. 查询所有 builder 的 claimable USDC，仅在超过 1 USDC 时执行 `claimRewards`。
3. 依据实时 available spot USDC 将 builder 资金归集到 settlement。
4. settlement 向固定 recipient 发送一笔 payout。
5. payout 确认后将冻结 records 标记完成。

Builder 和 settlement 都归属发送方账户，内部 claim/sweep 可以安全重复，不需要 journal。
settlement -> recipient 是唯一对外资金边界，也是唯一需要 exactly-once 防护的动作。
服务是单实例串行流程，`data/LOCK` 防止同一工作目录误双开。

## 2. 余额语义

一次 spot 查询返回两个明确数值：

- `Total`：链上 spot 总余额；
- `Available = Total - Hold`：当前可发送余额。

负 available 或无法解析的 decimal 是查询错误。Builder sweep 数额和 settlement 资金充足性
只使用 `Available`。Payout journal 保存提交前 `Total`；提交结果不确定时只比较
`TotalAfter < TotalBefore`。Hold 增减不会被误认为 payout，也不会掩盖 total 的真实下降。

“total 下降即成功”依赖 settlement 为本服务专用出账账户。不得由人工或其他程序从该
账户转出 spot USDC。

## 3. 内部归集的有限收敛

每轮先通过 referral info 查询所有 builder，并从 `tokenToState` 中定位 canonical USDC 的
token index（当前为 `0`），读取该 token state 的 `unclaimedRewards`。最外层同名字段是所有
reward 币种折算后的汇总值，不用于判断。USDC reward 严格超过 1 时才提交 claim，等于或低于
1 时只记录 INFO 并跳过，不发送告警；`tokenToState` 为空或没有 USDC 条目同样视为 0。
字段缺失、结构或金额非法、查询失败以及实际 claim 失败仍记录并告警。之后最多进行 5 轮
无持久化收敛：

1. 查询每个 builder 的 available；正数就全部 sweep 到 settlement。
2. 查询 settlement available；达到 `payout_total` 时立即停止收敛。
3. 尚不足时等待约 1 秒再进入下一轮。

单个 builder 的 prepare、submit 或查询失败会记录和告警，但其他 builder 继续。有限轮次
仍不足时返回普通错误，current 保持 `prepared`，调度器约 1 分钟后通过同一恢复路径再试，
最多自动重试 5 次。
内部 submit 结果不确定不建立持久化协议；下一轮继续依据实时余额收敛。

## 4. 数据快照与金额

查询按 `period_start_at, id` 排序读取 pending records。Manifest 持久化冻结的 records、
精确 raw total、六位向上取整后的 payout total、canonical token、settlement 和 recipient。
Builder 列表来自当前配置，不属于 payout 恢复数据。

金额按 decimal 文本精确汇总，再执行一次 `ceil(raw_total × 10^6) / 10^6`。负金额使本轮
归档一次 `failed_validation` 并 fatal 退出，不进入自动重试；零总额不执行链上动作，直接
完成冻结 records。无待处理 records 时只记录并报告，不创建 current 或 `no_data` history。

## 5. Payout 的两道持久化边界

发送前先构造完整可重放请求，并校验 signer、recipient、token、amount、nonce、请求 body
和 request hash 与 manifest 一致：

1. 保存 `payout_prepared`，其中包含完整 request 和 payout 前 settlement `Total`。
2. 请求可能离开进程前，再保存 `payout_submitting`。
3. 只有第二次保存成功后才调用 submit。

这保证 primary 与 backup 至少各持有一份完整付款意图。Submit response body 会完整写入
结构化日志，但不写入 durable state；完整签名请求始终不写日志。

只有成功 HTTP transport 返回的 `status = ok/err` 才属于明确结果。非 2xx、网络错误或无法解析的
响应即使携带类似 JSON body，也一律视为不确定。提交结果分三类：

| 结果 | 行为 |
| --- | --- |
| 明确成功 | 保存 `payout_confirmed`，然后无限重试 MySQL Complete |
| 明确拒绝 | 记录错误并告警，归档 `rejected`，清理 current，fatal 退出；不自动重发 |
| 不确定 | 最多读取 settlement 5 次；任一次 `TotalAfter < TotalBefore` 即确认成功 |

有限观察后仍无法判断时保存 `blocked`、归档现场、保留 current、告警并 fatal 退出。系统
绝不自动重发可能已发送的 payout；total 只需变少，不要求精确减少 payout 数额。

## 6. Phase 与恢复

Durable phase 只有：

| Phase | 恢复行为 |
| --- | --- |
| `prepared` | 重做内部 claim/sweep；达到资金要求后准备 payout |
| primary `payout_prepared` | 确定提交尚未开始，发送持久化原请求 |
| backup `payout_prepared` | 更新的 primary 可能已提交，只观察 total，绝不发送 |
| `payout_submitting` | 请求可能已发，只观察 total |
| `payout_confirmed` | 只重试数据库 Complete，不再执行链上动作 |
| `blocked` | 立即 fatal，等待人工判断，不重发 |

MySQL Complete 成功后直接归档 `completed` 并清理 current，不保存 `completed` phase。如果
Complete 与归档之间崩溃，恢复 `payout_confirmed` 后幂等地再次 Complete。

## 7. 本地持久化

```text
data/
├── LOCK
├── current.json
├── current.json.bak
└── history/
```

目录权限为 `0700`，文件为 `0600`。Envelope schema version 固定为 `1`，checksum 覆盖完整
RunState。保存通过同目录临时文件、`fsync`、primary -> backup rename、新 primary 安装及目录
`fsync` 完成。Primary 无效时自动尝试 backup；两份均无效则停止。

有效 history 类别为 `completed`、`rejected`、`blocked` 和 `failed_validation`。Blocked 保留
current；其他终态在相应安全条件满足后不阻止新运行。项目没有旧 schema 或 migration 分支。

## 8. MySQL、通知与调度

ListPending 和 Complete 对瞬时 MySQL 错误无限重试，退避响应 context cancel。达到持续不可用
阈值后只发送一次 outage email，期间按间隔记录 progress；恢复后发送一次 recovery email 并
重置状态，未来 outage 可重新告警。通知投递失败只记录，不中断资金流程。

结构化日志使用 16 位十六进制 run ID，并记录 record/builder 数量、raw/payout total、每个
builder 的 claimable USDC 与 claim eligibility、builder 与 settlement 的 total/hold/available、
实际 sweep 数量、payout 前余额和 submit response。每次 funding task 执行结束后，无论成功
还是失败，都会通过 `userRateLimit` 查询所有
builder 和 settlement，只记录 requests remaining；remaining 严格小于 200 时按账户告警。
查询失败只记录警告，不会覆盖原任务结果或改变资金状态机；同一账户持续处于低额度
时只告警一次，恢复到阈值后再次跌破才重新告警。固定的 service component 不重复写入每条日志。

成功 funding run 等待下一 UTC 01:00。普通错误（API 暂时失败、reward 未可见、归集不足）
约 1 分钟后重试，最多重试 5 次；全部失败则返回 retry-exhausted 错误并退出。Fatal payout
错误和确定性的 record 校验错误立即退出；context 取消立即退出且不会忙循环。MySQL 自身
的维护恢复仍无限重试。

## 9. 安全与测试不变量

1. 一个冻结批次的 recipient payout 最多发送一次。
2. 完整请求和“可能已发送”边界都在发送前持久化。
3. 只有 primary `payout_prepared` 可以首次发送。
4. 可能已发送或 blocked 的 payout 只观察/停止，不自动重发。
5. Payout 确认后只允许数据库完成和归档。
6. Builder 内部归集可重复并按实时 available 自动收敛。
7. Envelope schema version 保持 `1`，没有兼容旧 schema 的分支。

测试保留真实 HTTP mock、签名已知向量、claimRewards/spotSend、primary/backup 恢复、状态原子
写入、MySQL 无限重试、TTY/私钥/地址校验、SES 与敏感信息过滤。
