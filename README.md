# Hyperliquid Builder Code Bot

At UTC 00:00 this service concurrently captures canonical-USDC claimable
rewards for all configured builders and persists that reconciliation boundary
before any claim is submitted. It then claims in parallel, sweeps each
builder's available spot USDC into an operator-controlled settlement account,
and records the execution result. At UTC 01:00 it sends one payout for the
pending MySQL records to a fixed recipient and marks those records complete.

Claim visibility is measured against a stable pre-claim balance tuple. The
captured reward is normalized to token precision and used as a lower-bound
increase; ambiguous claims additionally require a decrease in claimable rewards.
Every sweep reloads a stable Builder balance immediately before Submit. An
accepted response completes that Builder without an immediate balance query.
Because Builder and settlement are both operator-controlled accounts, an
ambiguous sweep waits, reloads the latest stable Available balance, and retries
with a fresh nonce up to three times. A lower stale snapshot is guarded by
retrying at least the last ambiguous amount. A later accepted retry is reported
with a warning because the cumulative amount of earlier ambiguous requests is
unknown; retries never apply to the final payout.

The design treats builder-to-settlement transfers as repeatable internal fund
movement. Only the final settlement-to-recipient payout has a durable recovery
journal. An accepted response confirms the payout directly; an ambiguous
response is confirmed only after the post-submit pre-balance has been observed
and two consecutive paired observations show both the exact expected settlement
total and the exact expected recipient total. The observer waits before its
first query and keeps up to ten successful paired observations; a later recovery
may repeat this read-only check but never resubmits the payout. Available balance
(`total - hold`) is used separately for sweep amounts and payout sufficiency. An
explicit rejection or unresolved ambiguity alerts and terminates the process.
Settlement and recipient pre-payout boundaries also require consecutive stable
reads, and post-submit pre-balance evidence is persisted as soon as it appears.

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
Hyperliquid mock, including delayed `/info` visibility after claims and accepted
or ambiguous-but-applied payout actions, plus bounded internal sweep retries.

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
same parallel claimable capture, pre-claim balance boundaries, parallel claim
submissions and post-action observation as the UTC 00:00 task. It does not
connect to MySQL, read or
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
intervals. It writes a `claimables_captured` record before any claim, then
captures eligible builders' stable balance baselines and submits claims in
parallel. Ambiguous claims are observed without a second nonce; ambiguous
Builder-to-settlement sweeps instead reload current Available and retry with a
fresh nonce because both accounts are operator-controlled. The record is
updated to `completed` after the bounded processing finishes.
At UTC 01:00 (Asia/Shanghai 09:00), it freezes pending records and performs the
single settlement-to-recipient payout; it never claims or sweeps builders in
this phase. The SES report compares the captured zero-hour claimable total with
the database payout. Missing or incomplete claimable data prevents a numeric
conclusion; a lower complete total produces a warning. Neither condition blocks
a payout that the settlement balance can fund. Confirmed payouts enter
unlimited MySQL retry and are never sent again during database recovery.
Ordinary funding failures retry at most five times at one-minute intervals,
while retry exhaustion and fatal payout outcomes exit immediately.
