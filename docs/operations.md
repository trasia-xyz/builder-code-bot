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
./bin/builder-code-bot --help
./bin/builder-code-bot -config ./config.toml
./bin/builder-code-bot -config ./config.toml --run-on-start
./bin/builder-code-bot -config ./config.toml --consolidate-only
```

`--help` 会列出全部可用 flags 并以成功状态退出，不读取配置，也不初始化任何运行组件。

`--consolidate-only` 只执行一次 builder 资金归集并退出：并行查询所有 builder 的 claimable，
并行领取符合条件的 canonical USDC reward，等待余额可见后，把每个 builder 最新的正
available spot balance 全部发送到 settlement。它复用零点任务的短间隔查询重试和不确定 claim
重试，但不会保存本次查询结果。该模式不连接 MySQL，不读取或修改 funding records、`current`、
backup、history 或 `daily-consolidations`，不发送 recipient payout，也不启动 Scheduler；只
解密 builder 私钥，不会解密或使用 settlement 私钥。它仍会获取同一个 `data/LOCK`，因此必须
先停止同一工作目录下的常驻服务。

EC2 部署后，可单独验证实例凭证、网络和 SES 投递：

```sh
./bin/builder-code-bot -config ./config.toml --test-ses
```

该命令使用 `[aws]` 与 `[notification.ses]` 配置发送一封测试邮件，成功后立即退出。它会忽略
`notification.enabled`，因此可以在正式启用通知前测试；不会解密私钥、初始化 MySQL、获取资金
任务锁或启动调度器。测试邮件包含蓝色状态标题、HTML 布局和 preheader，可同时验证 macOS
Mail 与 iOS Gmail 的标题、列表预览和正文渲染。`--consolidate-only`、`--test-ses` 和
`--run-on-start` 彼此互斥。

启动阶段最多立即执行一个 funding run：存在 current 时只恢复 current，即使指定
`--run-on-start` 也不会继续创建新 run；没有 current 且指定 `--run-on-start` 时才创建新
run。`--run-on-start` 只执行数据库 payout 侧，不会补做零点 claim。启动阶段失败会直接返回，
由值守人员处理。进入 UTC Scheduler 后，每天 UTC 00:00（北京时间 08:00）先并行冻结并保存
claimable 对账数据，再并行 claim 与单轮归集；UTC 01:00（北京时间 09:00）只执行数据库批次
和 recipient payout。零点顶层失败不会阻止一点任务；一点普通错误每隔约 1 分钟最多
重试 5 次，全部失败或 payout fatal 才会退出。所有一点重试都只重新检查 settlement 和 payout，
不会执行 builder action。不要为这些退出配置
无间隔自动重启，否则进程重启会开启一组新的重试，绕过单次任务的自动重试上限。
服务不会在错过 UTC 00:00 后用更晚的执行冒充零点任务；若当时服务未运行，UTC 01:00 报告
会明确显示当日归集记录缺失。

输入私钥解密密码后，日志会依次报告私钥初始化、MySQL pool、进程锁、current 检查和
pending records 查询。进入 Scheduler 等待后会通过 `scheduled_task` 与 `next_run_at`
报告下一项任务及执行时间；funding 失败重试前也会更新该时间。控制台格式会在每次 funding
task 或启动恢复检查前插入空行，并用 `==========` 标记起始日志，便于区分相邻轮次。
控制台属性使用
`Label: value` 和 `│` 分隔，不显示与消息重复的 `event`；布尔值显示为 `yes`/`no`，USDC
金额会带单位。在连接 TTY 且 `logging.color = "auto"` 时，Payout、Claimable、余额和转账
金额会优先排列并以粗体青色突出显示；重定向到文件或管道时自动关闭 ANSI 颜色。JSON 格式
保留原始字段和 `event`，且不插入展示用分隔。MySQL pool 使用延迟连接，真正的首次建连发生
在 pending 查询期间。

每次 funding task 执行结束后（包括成功、失败以及没有 pending records），都会查询所有 builder
和 settlement 的 Hyperliquid 地址请求额度。`funding_user_rate_limit_observed` 日志包含
`requests_remaining`；remaining 严格小于 200 时日志升级为 warning 并发送告警。连续低额度只
告警一次，额度恢复到至少 200 后会解除抑制，未来再次跌破时重新告警。最终 `userRateLimit`
查询在非取消结果下使用独立 context；进程取消时跳过结束余额和 rate limit 观察，避免拖慢
关闭。查询失败只产生 warning，不会覆盖原任务结果，也不会改变 claim、sweep 或 payout
的状态。

Funding report 的标题状态如下：

- 🟢：任务成功；
- 🟡：任务完成，但 Builder action 或 rate limit 等存在告警；
- 🔵：没有待处理 records；
- 🟠：普通失败且仍有自动重试次数；
- 🔴：fatal、重试耗尽或需要人工核查 Payout。

🟠 邮件是精简的重试通知；其他终态邮件包含完整阶段、Builder、Payout 和处置明细。状态圆点
位于 `notification.ses.subject_prefix` 之前，便于在移动端邮件列表中直接识别。
完整报告和精简重试通知都会展示任务结束时 settlement 的 canonical USDC spot `Total`、
`Hold`、`Available` 与完整地址。最终余额查询失败只会把报告标记为有告警，不会改变原
funding 结果。

报告中的“UTC 00:00 Claim 与资金归集”展示每个 builder 的零点 claimable、查询与 claim
尝试次数、claim/sweep 状态、查询覆盖数、claimable 合计、本轮数据库 Payout 及
`Claimable - Payout` 差额。一点任务不会再实时查询 builder。每日记录缺失、损坏、builder
条目不足、claimable 查询失败或 claim 结果不确定都会显示 warning，但 settlement 余额充足时
payout 照常执行。精简重试邮件也保留该区块。

当所有 builder 的零点查询完整且 `Claimable >= Payout` 时，对账正常，区块显示
“完整，Claimable ≥ Payout”以及绿色的零或正差额。Claim 结果是否确定不改变已经冻结的
claimable 数值，但不确定结果会作为独立 warning 展示。

当零点查询完整且 `Claimable < Payout` 时，邮件状态升级为 warning，区块标红并展示
Claimable、Payout、负差额和短缺金额，要求人工核查结算口径。该告警绝不作为付款门槛；
Settlement available 足够时，主任务必须继续正常 payout。

从 `payout_prepared`、`payout_submitting` 或 `payout_confirmed` 恢复时，报告按 persisted
`UTCDate` 重新读取每日归集记录，不会重复执行 builder action。

## 状态与备份

```text
data/
├── LOCK
├── daily-consolidations/
│   └── YYYY-MM-DD.json
├── current.json
├── current.json.bak
└── history/
```

备份时先优雅停止服务，再一起复制 `config.toml` 和完整 `data/`，保留目录 `0700`、文件
`0600`。恢复后先不带 `--run-on-start` 启动。不要在运行中修改 daily consolidations、
current、backup 或 history。

`daily-consolidations` 按 UTC 日期以格式化 JSON 保存零点对账量和 claim/sweep 结果，使用
checksum 和私有文件权限。任务先写 `claimables_captured`，完成后原子更新为 `completed`；若只留下前一状态，
说明零点数值已保存，但后续动作可能在任意边界中断，Scheduler 不会自动重跑。该目录主要供
报告和人工检阅，不属于 payout journal，也不提供旧格式 migration 或 fallback；损坏或缺失时
不得通过复制其他日期的文件来伪造。Primary
无效时自动使用有效
backup 并告警。Primary `payout_prepared` 可以首次发送；从
backup 恢复同一 phase 时，更新的 primary 可能已提交，因此只观察 total，不会重发。
Schema version 当前且始终从 `1` 开始；项目没有 migration 或旧 schema fallback。

History 只包含 `completed`、`rejected`、`blocked`、`failed_validation`。无 pending records
不会创建 current 或 `no_data` archive。

负金额等确定性的 record 校验错误只归档一次 `failed_validation`，随后 fatal 退出，不消耗
普通错误的 5 次自动重试。修复数据库记录后再由值守人员启动服务。

## Builder 归集与普通错误

只有 UTC 00:00 归集和 `--consolidate-only` 会查询所有 builder 的 referral info，并读取
`tokenToState` 中 token index `0` 的 `unclaimedRewards`；不要使用最外层的全币种汇总字段。
所有 builder 同一轮并行查询，只重试失败项，每项最多 5 次、间隔 200 ms。成功读取且 USDC
严格超过 1 才正常提交 claim；等于或低于 1、列表为空或没有 index `0` 时跳过且不告警。若
5 次查询全部失败，则该 builder 的对账数据标记缺失，但仍执行受限 claim，避免漏领。`/info`
重试不消耗账户 exchange 请求额度；claim 使用 `/exchange`，所以提交重试严格受 3 次上限约束。

零点任务在任何 claim 前先保存 claimable。若该次写入失败，不会提交 claim；若当日文件已经
存在，无论是 `claimables_captured` 还是 `completed`，Scheduler 都会跳过重复执行。
`--consolidate-only` 复用同一查询、claim 和 sweep 实现，但完全不读写该文件，因此不会改动
一点报告的对账基准。

Builder claim 每一轮并行提交。明确接受立即完成；没有更早不确定结果时，明确拒绝立即停止。
不确定结果会在 250 ms 后使用新 nonce 重试，每个 builder 最多 3 次。曾经出现不确定结果后，
后续明确拒绝仍不能消除第一次的不确定性，因此会在上限内继续尝试；后续任一次明确接受则按
成功处理。全部提交结束后统一等待约
1 秒，再对已接受或结果不确定的 claim 查询 spot balance。每个 builder 最多查询 10 次，
`available >= claimable` 即停止该 builder 的查询；否则轮次间等待约 1 秒。10 次耗尽后会告警，
并按最后一次成功读取的 available 尽力归集；一次都未成功读取则跳过该 builder。零点
claimable 缺失时仅在余额可见性等待中使用 1 USDC 阈值，报告仍将对账数据标为缺失。

`--consolidate-only` 使用同一套 claim 与最多 10 次查询逻辑，但只进行一次 sweep，不查询
funding records 或 payout 所需金额，也不因 settlement 当前余额充足或不足改变归集行为。

零点任务和 `--consolidate-only` 都只做一次 sweep。UTC 01:00 不再进行任何内部归集；若
settlement available 不足，会返回普通错误并保留 `prepared` current，一分钟后只重新检查
settlement，最多重试 5 次。重试全部失败后进程退出，current 继续保留，便于排障后人工重启
恢复。

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
