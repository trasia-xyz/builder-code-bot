# 运维指南

系统设计与安全不变量见 [架构说明](architecture.md)。

## 构建与测试

```sh
go test ./... -count=1
go build -o bin/builder-code-bot ./cmd/builder-code-bot
go build -o bin/keytool ./cmd/keytool
```

## 配置与私钥

```sh
./bin/keytool encrypt
cp config.example.toml config.toml
chmod 0600 config.toml
```

- 生产环境将 `signing.decrypt_password` 留空，启动时从 TTY 读取。
- 核对配置地址与 keytool 派生地址一致。
- 不要把私钥、密码或生产配置写入日志、邮件或 shell history。
- Builder、settlement 均为我方账户；settlement **必须由本服务独占出账**，禁止人工或其他程序
  从该账户发送 spot USDC。
- Recipient 为非零地址，且不得与任一 builder 或 settlement 相同。

## 启动

始终使用同一工作目录（状态在 `./data`）：

```sh
./bin/builder-code-bot -config ./config.toml
./bin/builder-code-bot -config ./config.toml --run-on-start
./bin/builder-code-bot -config ./config.toml --consolidate-only
./bin/builder-code-bot -config ./config.toml --test-ses
```

| 模式 | 作用 |
| --- | --- |
| 默认 | 恢复 `current`（若有）后进入调度 |
| `--run-on-start` | 无 `current` 时立即跑一点侧 payout；有 `current` 时只恢复，不新建 run；不补零点归集 |
| `--consolidate-only` | 执行一次 builder→settlement 归集后退出；不碰 MySQL / payout journal / 每日归集文件 |
| `--test-ses` | 发送一封 SES 测试邮件后退出；忽略 `notification.enabled`；不解密密钥、不连 MySQL |

`--consolidate-only`、`--test-ses`、`--run-on-start` 互斥。`--consolidate-only` 仍占用
`data/LOCK`，须先停常驻服务。

## 调度与退出

| 时间 | 任务 |
| --- | --- |
| UTC 00:00（北京 08:00） | Claim + 归集；失败不阻断一点 |
| UTC 01:00（北京 09:00） | 冻结 pending records + recipient payout |

一点普通错误约每分钟重试，最多 5 次；fatal 或重试耗尽则进程退出。一点重试只检查 settlement
与 payout，不执行 builder 动作。

**不要**为这些退出配置无间隔自动重启：重启会开启新一轮重试计数，绕过单次任务的重试上限。
错过 UTC 00:00 不会事后冒充零点任务；当日归集缺失会在一点报告中标明。

## 状态与备份

```text
data/
├── LOCK
├── daily-consolidations/YYYY-MM-DD.json
├── current.json
├── current.json.bak
└── history/
```

1. 优雅停服务。
2. 一并备份 `config.toml` 与完整 `data/`（目录 `0700`、文件 `0600`）。
3. 恢复后先**不带** `--run-on-start` 启动。

运行中不要改动 `data/` 下状态文件。不要用其他日期的归集文件伪造当日记录。History 仅含
`completed` / `rejected` / `blocked` / `failed_validation`。

## 邮件状态

| 标记 | 含义 |
| --- | --- |
| 🟢 | 成功 |
| 🟡 | 完成，但有 builder / rate limit 等告警 |
| 🔵 | 无 pending records |
| 🟠 | 普通失败，仍有自动重试 |
| 🔴 | fatal、重试耗尽，或需人工核查 payout |

🟠 为精简重试通知；其余终态含完整明细。对账口径与付款门槛见架构说明：claimable 短缺只升
warning，不阻止 settlement 充足时的 payout。

## 故障处置

### Settlement 不足

单次一点任务会先有限重查并要求连续两个完整余额样本稳定，避免刚入账但 `/info` 尚未刷新的旧值
直接判定不足或成为付款基线；recipient 基线同样必须稳定。仍不足时
保留 `prepared` current 并由调度器自动重试。重试耗尽后进程退出，current 仍在，修好余额后
重启即可继续。

### 明确成功 / MySQL 未完成

状态为 `payout_confirmed`：先校验持久化 journal、请求和 settlement 付款前边界，再只重试数据库
Complete，**不会**再发链上 payout。校验失败会保留 current 并停止，不会完成 records。

### 明确拒绝

归档 `rejected`、清理 current、fatal 退出。修复原因后重启即可；records 仍 pending，且已知
未付款。

### 响应不确定 → blocked

禁止删除 current。重启或恢复时，程序会先等待并再次只读观察 settlement 与 recipient。只有先
看到提交后的 settlement 前态，随后两边连续两次分别达到 `settlement total before - amount`
与 `recipient total before + amount`，才自动确认；这个过程绝不会重发 payout。旧版本留下且
缺少 recipient 基线的 journal 会保持 blocked，必须人工核查。若仍然 blocked，再根据 current
中的 recipient、amount、nonce、request hash、双边 total before 与时间人工核查：

- **已付款**：停机状态下幂等完成 manifest 中 records，再归档并移走 current/backup；
- **未付款**：保留现场归档后，停机状态下移走 current/backup，再允许重新运行；
- **无法判断**：继续保留 blocked，绝不重发。

### Records 校验失败

负金额等确定性错误归档一次 `failed_validation` 后 fatal，不占用普通重试。修好数据库后再
启动。

### MySQL 瞬时故障

连接类瞬时错误会自动重试；持续不可用会发 outage 邮件，恢复后发 recovery 邮件。认证、权限、
schema 等非瞬时错误需按日志修复。通知发送失败不中断资金主流程。
