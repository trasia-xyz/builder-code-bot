# Hyperliquid Builder Code Bot

At UTC 00:00 this service concurrently captures canonical-USDC claimable
rewards for all configured builders and persists that reconciliation boundary
before any claim is submitted. It then claims in parallel, sweeps each
builder's available spot USDC into an operator-controlled settlement account,
and records the execution result. At UTC 01:00 it sends one payout for the
pending MySQL records to a fixed recipient and marks those records complete.

The design treats builder-to-settlement transfers as repeatable internal fund
movement. Only the final settlement-to-recipient payout has a durable recovery
journal. An accepted response confirms the payout directly; an ambiguous
response is confirmed only when the settlement total balance decreases during
five finite observations. Available balance (`total - hold`) is used separately
for sweep amounts and payout sufficiency. An explicit rejection or unresolved
ambiguity alerts and terminates the process.

This balance rule requires the settlement account to be exclusively controlled
by this service. No person or other process may send spot USDC from settlement.

Documentation:

- [`docs/architecture.md`](docs/architecture.md) describes the flow, state
  model, recovery rules, and safety invariants.
- [`docs/operations.md`](docs/operations.md) covers deployment, backups, and
  failure handling.

## Build and test

Go 1.25 or later is required.

```sh
go test ./... -count=1
go build -o bin/builder-code-bot ./cmd/builder-code-bot
go build -o bin/keytool ./cmd/keytool
```

The integration suite uses real signing and HTTP clients against a local
Hyperliquid mock, including ambiguous-but-applied payout confirmation.

## Configure

Generate encrypted private keys in a trusted terminal:

```sh
./bin/keytool encrypt
```

Copy and protect the configuration:

```sh
cp config.example.toml config.toml
chmod 0600 config.toml
```

Configuration is decoded as strict TOML; unknown fields are rejected.

Verify that every configured address matches the address printed by `keytool`.
Builders, settlement, and recipient must satisfy the separation checks in the
configuration: recipient must be non-zero and differ from every builder and
settlement. Keep `signing.decrypt_password` empty in production to read it once
from a controlling TTY at startup.

## Run

Always use the same working directory because state is stored in `./data`.

```sh
./bin/builder-code-bot --help
./bin/builder-code-bot -config ./config.toml
./bin/builder-code-bot -config ./config.toml --run-on-start
./bin/builder-code-bot -config ./config.toml --consolidate-only
```

`--help` prints all available flags and exits successfully without loading the
configuration or starting any runtime component.

`--run-on-start` immediately runs the database/payout phase. It does not claim
or sweep builders and does not backfill a missed UTC 00:00 consolidation.

`--consolidate-only` runs one builder-fund consolidation and exits. It uses the
same parallel claimable capture, parallel claim attempts, bounded retries and
sweep behavior as the UTC 00:00 task. It does not connect to MySQL, read or
update funding records or durable `current` state, read or write the UTC 00:00
daily consolidation record, send a recipient payout, or start the scheduler.
It decrypts only builder keys, but still acquires `data/LOCK`, so it cannot run
alongside the service in the same working directory.

To verify EC2 credentials, network access, and SES delivery without starting
the funding runtime, send one test email and exit:

```sh
./bin/builder-code-bot -config ./config.toml --test-ses
```

The test always uses `[aws]` and `[notification.ses]` and intentionally ignores
`notification.enabled`, so SES can be verified before runtime notifications are
enabled. It does not decrypt private keys, initialize MySQL, acquire the funding
process lock, or start the scheduler. `--consolidate-only`, `--test-ses`, and
`--run-on-start` are mutually exclusive. The message exercises the same UTF-8
status subject ordering, HTML rendering, and inbox preheader used by runtime
reports.

Startup always recovers `data/current.json` before starting a new run. The data
directory retains `LOCK`, date-keyed daily consolidation records, checksummed
current and backup payout states, and history archives. At UTC 00:00
(Asia/Shanghai 08:00), the scheduler captures all builder claimable values in
parallel, retrying only failed `/info` reads up to five times at 200 ms
intervals. It writes a `claimables_captured` record before any claim, then runs
parallel claims with at most three attempts for uncertain responses and updates
the record to `completed` after sweeping.
At UTC 01:00 (Asia/Shanghai 09:00), it freezes pending records and performs the
single settlement-to-recipient payout; it never claims or sweeps builders in
this phase. The SES report compares the captured zero-hour claimable total with
the database payout. Missing or incomplete claimable data prevents a numeric
conclusion; a lower complete total produces a warning. Neither condition blocks
a payout that the settlement balance can fund. Confirmed payouts enter
unlimited MySQL retry and are never sent again during database recovery.
Ordinary funding failures retry at most five times at one-minute intervals,
while retry exhaustion and fatal payout outcomes exit immediately.
