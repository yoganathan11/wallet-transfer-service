# Design note - Wallet Transfers

## Problem
Move money between two wallets exactly once per client intent, keep a double-entry ledger, and stay correct under retries, duplicates and concurrent requests.

## Contract
`POST /transfers` body: `{idempotencyKey?, fromWalletId, toWalletId, amount}`; `amount` is an integer in minor units, > 0; wallets must differ.

| Situation | Result |
|---|---|
| New request, enough funds | `201`, transfer `PROCESSED`, 2 ledger entries, balances updated |
| New request, insufficient funds | `422`, transfer `FAILED` (`INSUFFICIENT_FUNDS`) is **persisted**, no ledger entries, no balance change |
| Same key + same payload again | original transfer returned (`200`, or `422` if it failed), `Idempotent-Replayed: true`, no side effects |
| Same key + different payload | `422 idempotency_key_reuse`, nothing executed |
| Invalid input | `400`, nothing persisted |
| Unknown wallet | `404`, **everything rolled back incl. the key**, so a retry after the wallet exists works |
| No key | every call is a separate transfer (at-most-once cannot be promised without a key) |

## State machine
`PENDING -> PROCESSED` and `PENDING -> FAILED`; both terminal. Enforced twice: in the domain (`CanTransitionTo`) and in SQL as a compare-and-set (`UPDATE ... WHERE id=$1 AND status='PENDING'`), so a late/duplicate writer gets `ErrInvalidTransition` instead of overwriting a terminal state.

## Schema (see `db/schema.sql`)
- `wallets(id, balance BIGINT CHECK >= 0)` - stored balance, updated in the same transaction as the ledger.
- `transfers(... amount CHECK > 0, status CHECK IN (...), CHECK from <> to, idempotency_key UNIQUE NULL)`.
- `ledger_entries(entry_id, wallet_id, transfer_id, type, amount, UNIQUE(transfer_id, type))` - the unique constraint makes "exactly one DEBIT + one CREDIT per transfer" a database guarantee.
- `idempotency_records(key PK, request_hash, transfer_id)`.
- Indexes: `ledger_entries(wallet_id, entry_id)`, `transfers(from_wallet_id, created_at)`, `transfers(to_wallet_id, created_at)`; PKs/uniques cover the rest.

Invariant: `balance = opening + sum(CREDIT) - sum(DEBIT)` per wallet, and `sum(DEBIT) = sum(CREDIT)` globally. Tests assert this after every scenario.

## Transaction & concurrency strategy
One DB transaction per request, READ COMMITTED, pessimistic row locks:
1. Claim idempotency key.
2. Insert `PENDING` transfer.
3. `SELECT ... FOR UPDATE` both wallets **in sorted id order** (A->B and B->A can never deadlock).
4. Check balance on the locked row, then `UPDATE balance = balance + delta` (relative, never a stale write-back).
5. Insert the two ledger entries, `PENDING -> PROCESSED`, commit.

Why pessimistic locks rather than optimistic/SERIALIZABLE: money movement on hot wallets is contended by nature; locking gives deterministic behaviour with no retry loop, and `CHECK (balance >= 0)` is a last-resort safety net.

## Idempotency strategy
`INSERT INTO idempotency_records ... ON CONFLICT (key) DO NOTHING` inside the same transaction as the transfer.
- Key stored with a SHA-256 of `(from, to, amount)`; mismatch on reuse -> `422`.
- A concurrent duplicate **blocks** on that insert until the first transaction commits or rolls back. If it committed, the duplicate reads and returns the stored transfer. If it rolled back, the duplicate proceeds as the new owner.
- Because key + transfer + ledger + balances commit atomically, there is no "key saved but transfer lost" or "transfer done but key missing" state, and no IN_PROGRESS state to clean up after a crash.

## Failure modes
| Failure | Behaviour |
|---|---|
| Crash / network drop before commit | Everything rolled back; client retries with the same key |
| Crash / network drop after commit, before response | Retry with the same key returns the stored result |
| Two identical requests at once | One executes, the other waits and replays |
| Opposite transfers at once | Ordered locking, no deadlock |
| Business failure (insufficient funds) | Committed as `FAILED`, replays identically; client must use a new key to try again |

## Observability
Structured `slog` logs: one line per HTTP request (method, path, status, duration) and per transfer (id, status, replayed, wallets, amount). Replays are visible via `replayed=true`. Metrics are out of scope (would add counters by status and lock-wait latency).

## Testing strategy
Domain unit tests (state machine, validation, ledger shape) + PostgreSQL integration tests: success, insufficient funds, sequential replay, key reuse, no-key, rollback on unknown wallet, terminal-state guard, and three concurrency tests (same key x20, no double spend 50 x 10 on 100, opposite directions x100) with ledger invariants asserted at the end of each.

## Trade-offs / assumptions
- Single currency, integer minor units. `POST /wallets` with an initial balance is a seeding helper; that opening balance has no ledger entry (a production system would fund wallets from a system account via the ledger).
- Idempotency records have no TTL/cleanup.
- Ledger is append-only by convention; a DB trigger forbidding UPDATE/DELETE would harden it.
