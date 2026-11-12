# Wallet Transfer Service

Go + PostgreSQL service for wallet-to-wallet transfers with idempotency, a double-entry ledger and concurrency safety.

## Layout

```
main.go          wiring + graceful shutdown
process/         domain + business logic
                   wallet.go, transfer.go (entity, state machine, validation),
                   ledger_entries.go, idempotency.go, ports.go (interfaces),
                   service.go (transfer workflow / transaction boundary)
db/              PostgreSQL (pgx) repository + schema.sql
handler/         HTTP: decode, call service, map errors
docs/DESIGN.md   spec / design note (written before the code)
```

## Run

Requires Go 1.22+ and Docker (or any PostgreSQL 13+).

```powershell
docker compose up -d
go mod tidy
go run .
```

Env vars: `DATABASE_URL` (default `postgres://postgres:postgres@localhost:5432/wallet?sslmode=disable`), `PORT` (default `8080`).

## Try it (PowerShell)

```powershell
$h = @{ "Content-Type" = "application/json" }
Invoke-RestMethod -Method Post -Uri http://localhost:8080/wallets -Headers $h -Body '{"id":"wallet_1","initialBalance":1000}'
Invoke-RestMethod -Method Post -Uri http://localhost:8080/wallets -Headers $h -Body '{"id":"wallet_2","initialBalance":0}'

$body = '{"idempotencyKey":"abc123","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100}'
Invoke-RestMethod -Method Post -Uri http://localhost:8080/transfers -Headers $h -Body $body   # 201, processed
Invoke-RestMethod -Method Post -Uri http://localhost:8080/transfers -Headers $h -Body $body   # 200, same transfer id, no second debit
Invoke-RestMethod -Uri http://localhost:8080/wallets/wallet_1                                  # balance 900
```

Amounts are integers in minor units (paise/cents).

## API

| Method | Path | Notes |
|---|---|---|
| POST | `/transfers` | `201` new processed, `200` replay (`Idempotent-Replayed: true`), `422` failed (e.g. insufficient funds, also on replay), `400` validation, `404` wallet missing, `422 idempotency_key_reuse` |
| GET | `/transfers/{id}` | |
| POST | `/wallets` | `{id, initialBalance}` - helper for seeding/testing |
| GET | `/wallets/{id}` | balance API |
| GET | `/healthz` | |

## Tests

```powershell
go test ./process/...          # pure unit tests, no DB

docker exec -it <postgres-container> psql -U postgres -c "CREATE DATABASE wallet_test"
$env:TEST_DATABASE_URL = "postgres://postgres:postgres@localhost:5432/wallet_test?sslmode=disable"
go test -race -count=1 ./...           # includes concurrency tests; they TRUNCATE the tables
```

Integration tests are skipped when `TEST_DATABASE_URL` is unset. Never point it at a database you care about.
