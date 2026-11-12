# PR: Wallet transfer service

## Summary
Go + PostgreSQL service: `POST /transfers` with idempotency, double-entry ledger, state machine (PENDING -> PROCESSED/FAILED) and concurrency-safe balances. Plus wallet balance and transfer lookup endpoints. Full spec and rationale in `docs/DESIGN.md`.

## Database schema design
<!-- Summarise in your own words: wallets (stored balance + CHECK >= 0), transfers, ledger_entries (UNIQUE(transfer_id,type)), idempotency_records, indexes. -->

## Idempotency strategy
<!-- Own words: idempotency_records insert ON CONFLICT DO NOTHING in the same tx; request hash; concurrent duplicates wait; rollback releases key. -->

## Concurrency handling
<!-- Own words: single tx, READ COMMITTED, SELECT FOR UPDATE in sorted order, relative balance update, why not optimistic/SERIALIZABLE. -->

## Assumptions / trade-offs
<!-- Own words: minor units, FAILED persisted & replayed, no key = no dedupe, opening balance not in ledger, no key TTL. -->

## How to run / test
See README.md.

## AI usage
1. **Tool used:** <!-- e.g. Claude (claude.ai chat) -->
2. **How I generally use it:** <!-- your honest description -->
3. **Transcript:** <!-- add the full session export to the repo (e.g. docs/ai-transcript.md) or attach all prompts -->
4. **What I changed / verified myself:** <!-- e.g. ran the tests, reviewed the locking order, modified X -->
