# 运维指南

系统设计与安全不变量见 [架构说明](architecture.md)。

## 构建与测试

```sh
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
staticcheck ./...
unparam ./...
```

集成测试使用本地 HTTP Hyperliquid mock 和真实签名，覆盖明确成功、拒绝、响应不确定但已
执行，以及不确定且无法确认的路径。

## 配置与私钥

```sh
go build -o bin/keytool ./cmd/keytool
./bin/keytool encrypt
cp config.example.toml config.toml
chmod 0600 config.toml
```

配置使用严格 TOML decoder，未知字段会导致启动失败。生产环境建议把
`signing.decrypt_password` 留空，由真实 TTY 无回显读取。核对每个配置地址与 keytool 派生
地址一致，不要把私钥、密码、完整签名请求或生产配置写入日志、邮件或 shell history。

Builder 与 settlement 必须全部为我方账户。Settlement 必须是本服务专用出账账户，不得
人工或由其他程序转出 spot USDC；否则 total 下降不能作为 payout 成功证据。
Recipient 必须是非零地址，并且不得与任一 builder 或 settlement 相同。

## 启动与调度

始终使用同一工作目录，因为状态位于 `./data`：

```sh
./bin/builder-code-bot -config ./config.toml
./bin/builder-code-bot -config ./config.toml --run-on-start
```

EC2 部署后，可单独验证实例凭证、网络和 SES 投递：

```sh
./bin/builder-code-bot -config ./config.toml --test-ses
```

该命令使用 `[aws]` 与 `[notification.ses]` 配置发送一封测试邮件，成功后立即退出。它会忽略
`notification.enabled`，因此可以在正式启用通知前测试；不会解密私钥、初始化 MySQL、获取资金
任务锁或启动调度器。`--test-ses` 不能与 `--run-on-start` 同时使用。

启动阶段最多立即执行一个 funding run：存在 current 时只恢复 current，即使指定
`--run-on-start` 也不会继续创建新 run；没有 current 且指定 `--run-on-start` 时才创建新
run。启动阶段失败会直接返回，由值守人员处理。进入 UTC Scheduler 后，普通错误每隔约
1 分钟最多重试 5 次；全部失败或 payout fatal 都会退出。不要为这些退出配置无间隔自动
重启，否则进程重启会开启一组新的重试，绕过单次任务的自动重试上限。

输入私钥解密密码后，日志会依次报告私钥初始化、MySQL pool、进程锁、current 检查和
pending records 查询。进入 Scheduler 等待后会通过 `next_run_at` 报告下一轮执行时间；失败
重试前也会更新该时间。控制台格式会在每次 funding task 或启动恢复检查前插入空行，并用
`==========` 标记起始日志，便于区分相邻轮次；JSON 格式不插入展示用分隔。MySQL pool 使用
延迟连接，真正的首次建连发生在 pending 查询期间。

每次 funding task 执行结束后（包括成功、失败以及没有 pending records），都会查询所有 builder
和 settlement 的 Hyperliquid 地址请求额度。`funding_user_rate_limit_observed` 日志包含
`requests_remaining`；remaining 严格小于 200 时日志升级为 warning 并发送告警。连续低额度只
告警一次，额度恢复到至少 200 后会解除抑制，未来再次跌破时重新告警。最终 `userRateLimit`
查询使用不受原任务取消影响的 context；查询失败只产生 warning，
不会覆盖原任务结果，也不会改变 claim、sweep 或 payout 的状态。

## 状态与备份

```text
data/
├── LOCK
├── current.json
├── current.json.bak
└── history/
```

备份时先优雅停止服务，再一起复制 `config.toml` 和完整 `data/`，保留目录 `0700`、文件
`0600`。恢复后先不带 `--run-on-start` 启动。不要在运行中修改 current、backup 或 history。

Primary 无效时自动使用有效 backup 并告警。Primary `payout_prepared` 可以首次发送；从
backup 恢复同一 phase 时，更新的 primary 可能已提交，因此只观察 total，不会重发。
Schema version 当前且始终从 `1` 开始；项目没有 migration 或旧 schema fallback。

History 只包含 `completed`、`rejected`、`blocked`、`failed_validation`。无 pending records
不会创建 current 或 `no_data` archive。

负金额等确定性的 record 校验错误只归档一次 `failed_validation`，随后 fatal 退出，不消耗
普通错误的 5 次自动重试。修复数据库记录后再由值守人员启动服务。

## Builder 归集与普通错误

每轮先查询所有 builder 的 referral info，并读取 `tokenToState` 中 token index `0` 的
`unclaimedRewards`；不要使用最外层的全币种汇总字段。该 USDC 数值严格超过 1 才提交 claim，
等于或低于 1、列表为空或没有 index `0` 时正常跳过且不告警。字段缺失、响应非法、查询失败
或实际 claim 失败仍会告警。随后最多进行 5 轮查询与 sweep。Builder 的 available
(`total - hold`) 为正时全部发送到 settlement；每轮检查 settlement available，仍不足就
等待约 1 秒。有限轮次耗尽会返回普通错误，保留 `prepared` current，并约 1 分钟后自动
恢复重试，最多重试 5 次，不需要人工清理或 builder action journal。重试全部失败后进程
退出，current 继续保留，便于排障后人工重启恢复。

## Payout 结果处置

### 明确成功

程序保存 `payout_confirmed`，随后无限重试 MySQL Complete。数据库恢复后自动继续；重启也
只完成数据库，不再发送 payout。

### 明确拒绝

程序记录错误、发送 email、归档 `rejected`、清理 current，然后 fatal 退出。修复拒绝原因
后可以重启；records 仍 pending，且已知没有发生付款。

### 响应不确定

程序最多观察 settlement 5 次，只比较 total：

- `TotalAfter < TotalBefore`：确认成功并进入数据库完成；
- 始终未下降或无法可靠读取：保存 `blocked`、归档现场、保留 current、告警并退出。

Hold 变化不会确认 payout。Blocked 表示请求可能已经执行，禁止删除 current 后直接重启。
人工应使用 current 中的 recipient、amount、nonce、request hash、total before 和时间核查：

- 明确已付款：在停机状态下幂等完成 manifest 中 records，再归档并移走 current/backup；
- 明确未付款：保留现场归档后，在停机状态下移走 current/backup，再允许重新运行；
- 无法明确：继续保留 blocked，绝不重发。

## MySQL 故障与通知

瞬时连接错误、server shutdown、deadlock、lock wait timeout 等无限重试，退避响应进程取消。
连续不可用达到阈值只发送一次 outage email；重试期间按合理间隔记录 progress；恢复只发送
一次 recovery email 并重置状态。未来 outage 会重新告警。通知发送失败不会中断资金主流程。

认证、权限、schema、SQL、扫描和业务完整性错误不是瞬时故障，应根据日志修复。Payout 已
确认时 current 保持 `payout_confirmed`，因此任何 MySQL 维护或进程重启都不会造成重复付款。
