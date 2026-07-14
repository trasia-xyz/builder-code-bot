# Hyperliquid Builder Code Bot

This service claims rewards for configured builder accounts, sweeps each
builder's full available spot USDC balance into an operator-controlled
settlement account, sends one payout to a fixed recipient, and marks the
corresponding MySQL records complete.

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
configuration. Keep `signing.decrypt_password` empty in production to read it
once from a controlling TTY at startup.

## Run

Always use the same working directory because state is stored in `./data`.

```sh
./bin/builder-code-bot -config ./config.toml
./bin/builder-code-bot -config ./config.toml --run-on-start
```

Startup always recovers `data/current.json` before starting a new run. The data
directory retains `LOCK`, checksummed current and backup snapshots, and history
archives. Confirmed payouts enter unlimited MySQL retry and are never sent
again during database recovery. Successful runs wait for the next UTC midnight;
ordinary failures retry at most five times at one-minute intervals, while
retry exhaustion and fatal payout outcomes exit immediately.
