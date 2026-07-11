# Hyperliquid Builder Code Bot

This service claims rewards for configured Hyperliquid builder accounts, sweeps
their spot USDC to a settlement account, pays one fixed recipient, and marks the
corresponding MySQL funding records complete. A checksummed state snapshot in
`./data` makes each run recoverable after a crash.

Project documentation:

- [`docs/architecture.md`](docs/architecture.md) describes components, state
  transitions, reconciliation, and safety invariants.
- [`docs/operations.md`](docs/operations.md) is the operations guide.

## Build and test

Go 1.25 or later is required.

```sh
go test ./... -count=1
go build -o bin/builder-code-bot ./cmd/builder-code-bot
go build -o bin/keytool ./cmd/keytool
```

The hermetic integration suite starts a local Hyperliquid mock and exercises
real HTTP requests, signing, state persistence, and recovery:

```sh
go test ./internal/dev/hyperliquidmock -count=1
go test ./internal/funding -run Integration -count=1
```

## Encrypt account keys

Run the key tool from a trusted terminal. It reads secrets without echoing them
and does not read the decryption password from an environment variable.

```sh
./bin/keytool encrypt
./bin/keytool decrypt
```

`encrypt` prints a signer address and an `encrypted_private_key` value. Verify
that the address matches the intended builder or settlement account before
copying the ciphertext into the configuration. `decrypt` prints only the
derived address by default. Avoid `--show-private-key` except during a
controlled recovery, because it prints the private key to standard output.

Never put a private key, decryption password, signature, signed request, or
complete production configuration in logs, tickets, shell history, or email.

## Configure the service

```sh
cp config.example.toml config.toml
chmod 0600 config.toml
```

Replace every placeholder. Builder names and addresses must be unique. The
settlement account must be separate from all builders, and the recipient must
be separate from the settlement account. Keep `hyperliquid.base_url` empty in
production; it exists to target a local mock while `network` continues to
select the signing domain.

All encrypted account keys share one decryption password. The recommended
production configuration leaves `signing.decrypt_password` empty. On every
process start the service then requires a controlling TTY and reads the
password once without echo. A non-empty configured password permits unattended
startup but puts the ciphertext and its password in the same file, reducing
the protection provided by encryption.

When notifications are enabled, set the AWS region, SES sender, and at least
one recipient. AWS SDK v2 uses its normal credential provider chain; `profile`
optionally selects a shared-config profile. Confirm that the SES identity is
verified and that the service account can call SES.

## Run

Always start the process from the same project working directory because the
state path is fixed at `./data`.

```sh
./bin/builder-code-bot -config ./config.toml
./bin/builder-code-bot -config ./config.toml --run-on-start
```

Without `--run-on-start`, startup first recovers an existing `current.json` and
then waits for the next UTC 00:00 schedule. With the flag, recovery still runs
first, followed by one new funding cycle; the process then remains resident.
The scheduler recalculates the next UTC midnight rather than using a 24-hour
ticker.

## State, backup, and recovery

The service creates `./data` with mode `0700`; snapshots and the process lock
use mode `0600`. Restrict both `config.toml` and `./data` to the service account.
Back up the source database independently. To capture or restore a consistent
local recovery snapshot, stop the service, copy `config.toml` and the complete
`./data` directory together, preserve permissions, and restart from the same
working directory.

Do not delete or edit an uncertain `data/current.json`. It may describe a final
payout that reached Hyperliquid even when the HTTP result was lost. Recovery
must reconcile the persisted signed request and action time with the balance
change and a unique, exact ledger record before another payout can be created.
The ledger transfer payload does not expose the request nonce. If both the
primary and backup snapshots are invalid, stop and investigate rather than
forcing a new run.

## MySQL outages and alerts

Transient connection failures, server restarts, deadlocks, and lock timeouts
are retried until shutdown, with bounded backoff. If the final payout was
accepted before MySQL became unavailable, recovery remains in the database
phase and never creates another payout. Authentication, schema, SQL, scan, and
business-validation errors are not retried automatically.

Configure SES notifications for an end-of-run report on every successful or
failed funding cycle, plus alerts for negative source amounts, builder failures,
insufficient settlement balance, ambiguous chain results, sustained MySQL
outages and recovery, database completion failures, and damaged state.
Notification failure does not change the authoritative funding state.

## Manual upgrade procedure

1. Stop the service cleanly and retain the current working directory.
2. Back up `config.toml`, `./data`, and the database using the procedure above.
3. Build the new binaries and run the full test suite in a separate release
   directory.
4. Review release notes for state schema or configuration changes; regenerate
   encrypted keys only when the encryption format explicitly changes.
5. Replace the binaries without deleting `./data`.
6. Start the service without `--run-on-start` and confirm that any current run
   recovers. Use `--run-on-start` later only when an additional new cycle is
   intentionally required.
