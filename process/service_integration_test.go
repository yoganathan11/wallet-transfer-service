package process_test

// Integration tests: they exercise the real PostgreSQL locking/constraint behaviour.
// Run with:  TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/wallet_test?sslmode=disable go test -race ./...
// Without TEST_DATABASE_URL they are skipped.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"wallet-transfer-service/db"
	"wallet-transfer-service/process"
)

type testEnv struct {
	svc     *process.TransferService
	store   *db.Store
	pool    *pgxpool.Pool
	opening map[string]int64 // opening balances, to verify balance = opening + credits - debits
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	store := db.NewStore(pool)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE idempotency_records, ledger_entries, transfers, wallets`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &testEnv{svc: process.New(store, log), store: store, pool: pool, opening: map[string]int64{}}
}

func (e *testEnv) wallet(t *testing.T, id string, balance int64) {
	t.Helper()
	if _, err := e.svc.CreateWallet(context.Background(), id, balance); err != nil {
		t.Fatalf("create wallet %s: %v", id, err)
	}
	e.opening[id] = balance
}

func (e *testEnv) balance(t *testing.T, id string) int64 {
	t.Helper()
	w, err := e.svc.GetWallet(context.Background(), id)
	if err != nil {
		t.Fatalf("get wallet %s: %v", id, err)
	}
	return w.Balance
}

func (e *testEnv) transfer(key, from, to string, amount int64) (*process.CreateTransferResult, error) {
	return e.svc.CreateTransfer(context.Background(), process.CreateTransferInput{
		IdempotencyKey: key, FromWalletID: from, ToWalletID: to, Amount: amount,
	})
}

func (e *testEnv) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	return n
}

// assertInvariants checks the properties that must hold after ANY sequence of transfers.
func (e *testEnv) assertInvariants(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	// 1. ledger balances globally: sum(DEBIT) == sum(CREDIT)
	var debits, credits int64
	if err := e.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount) FILTER (WHERE type='DEBIT'), 0)::bigint,
		       COALESCE(SUM(amount) FILTER (WHERE type='CREDIT'), 0)::bigint
		FROM ledger_entries`).Scan(&debits, &credits); err != nil {
		t.Fatal(err)
	}
	if debits != credits {
		t.Errorf("ledger out of balance: debits=%d credits=%d", debits, credits)
	}

	// 2. every wallet: balance == opening + credits - debits
	rows, err := e.pool.Query(ctx, `
		SELECT w.id, w.balance,
		       COALESCE(SUM(l.amount) FILTER (WHERE l.type='CREDIT'), 0)::bigint,
		       COALESCE(SUM(l.amount) FILTER (WHERE l.type='DEBIT'), 0)::bigint
		FROM wallets w LEFT JOIN ledger_entries l ON l.wallet_id = w.id
		GROUP BY w.id, w.balance`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var bal, cr, dr int64
		if err := rows.Scan(&id, &bal, &cr, &dr); err != nil {
			t.Fatal(err)
		}
		if want := e.opening[id] + cr - dr; bal != want {
			t.Errorf("wallet %s balance=%d, ledger says %d", id, bal, want)
		}
	}

	// 3. PROCESSED transfers have exactly 2 entries; PENDING/FAILED have none; nothing is stuck PENDING
	bad := e.count(t, `
		SELECT COUNT(*) FROM transfers t
		WHERE (t.status = 'PROCESSED' AND (SELECT COUNT(*) FROM ledger_entries l WHERE l.transfer_id = t.id) <> 2)
		   OR (t.status <> 'PROCESSED' AND EXISTS (SELECT 1 FROM ledger_entries l WHERE l.transfer_id = t.id))`)
	if bad != 0 {
		t.Errorf("%d transfers have a wrong number of ledger entries", bad)
	}
	if pending := e.count(t, `SELECT COUNT(*) FROM transfers WHERE status = 'PENDING'`); pending != 0 {
		t.Errorf("%d transfers left PENDING", pending)
	}
}

func TestTransfer_Success(t *testing.T) {
	e := newEnv(t)
	e.wallet(t, "wallet_1", 500)
	e.wallet(t, "wallet_2", 0)

	res, err := e.transfer("abc123", "wallet_1", "wallet_2", 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed || res.Transfer.Status != process.StatusProcessed {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := e.balance(t, "wallet_1"); got != 400 {
		t.Errorf("wallet_1 = %d, want 400", got)
	}
	if got := e.balance(t, "wallet_2"); got != 100 {
		t.Errorf("wallet_2 = %d, want 100", got)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM ledger_entries WHERE transfer_id = $1`, res.Transfer.ID); n != 2 {
		t.Errorf("ledger entries = %d, want 2", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM ledger_entries WHERE transfer_id=$1 AND wallet_id='wallet_1' AND type='DEBIT' AND amount=100`, res.Transfer.ID); n != 1 {
		t.Errorf("missing DEBIT entry on wallet_1")
	}
	if n := e.count(t, `SELECT COUNT(*) FROM ledger_entries WHERE transfer_id=$1 AND wallet_id='wallet_2' AND type='CREDIT' AND amount=100`, res.Transfer.ID); n != 1 {
		t.Errorf("missing CREDIT entry on wallet_2")
	}
	e.assertInvariants(t)
}

func TestTransfer_InsufficientFunds_IsRecordedAsFailed_AndReplayed(t *testing.T) {
	e := newEnv(t)
	e.wallet(t, "a", 50)
	e.wallet(t, "b", 0)

	first, err := e.transfer("k-fail", "a", "b", 100)
	if err != nil {
		t.Fatal(err)
	}
	if first.Transfer.Status != process.StatusFailed || first.Transfer.FailureReason != process.FailureInsufficientFunds {
		t.Fatalf("want FAILED/INSUFFICIENT_FUNDS, got %+v", first.Transfer)
	}
	if e.balance(t, "a") != 50 || e.balance(t, "b") != 0 {
		t.Error("balances must be unchanged after a failed transfer")
	}

	// Even if the wallet is topped up later, the same key must return the ORIGINAL (failed) result.
	if _, err := e.pool.Exec(context.Background(), `UPDATE wallets SET balance = 1000 WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	e.opening["a"] = 1000 // test-only top-up done outside the ledger
	second, err := e.transfer("k-fail", "a", "b", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.Transfer.ID != first.Transfer.ID || second.Transfer.Status != process.StatusFailed {
		t.Fatalf("replay must return the original FAILED transfer, got %+v", second)
	}
	e.assertInvariants(t)
}

func TestIdempotency_SequentialReplayDoesNotTransferTwice(t *testing.T) {
	e := newEnv(t)
	e.wallet(t, "a", 1000)
	e.wallet(t, "b", 0)

	first, err := e.transfer("same-key", "a", "b", 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := e.transfer("same-key", "a", "b", 100)
		if err != nil {
			t.Fatal(err)
		}
		if !again.Replayed || again.Transfer.ID != first.Transfer.ID {
			t.Fatalf("retry %d: want replay of %s, got %+v", i, first.Transfer.ID, again)
		}
	}
	if got := e.balance(t, "a"); got != 900 {
		t.Errorf("a = %d, want 900 (debited once)", got)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM transfers`); n != 1 {
		t.Errorf("transfers = %d, want 1", n)
	}
	e.assertInvariants(t)
}

func TestIdempotency_SameKeyDifferentPayloadIsRejected(t *testing.T) {
	e := newEnv(t)
	e.wallet(t, "a", 1000)
	e.wallet(t, "b", 0)

	if _, err := e.transfer("k", "a", "b", 100); err != nil {
		t.Fatal(err)
	}
	_, err := e.transfer("k", "a", "b", 999)
	if !errors.Is(err, process.ErrIdempotencyKeyReuse) {
		t.Fatalf("got %v, want ErrIdempotencyKeyReuse", err)
	}
	if got := e.balance(t, "a"); got != 900 {
		t.Errorf("a = %d, want 900", got)
	}
}

func TestNoIdempotencyKey_EachRequestIsSeparate(t *testing.T) {
	e := newEnv(t)
	e.wallet(t, "a", 1000)
	e.wallet(t, "b", 0)

	for i := 0; i < 2; i++ {
		if _, err := e.transfer("", "a", "b", 100); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.balance(t, "a"); got != 800 {
		t.Errorf("a = %d, want 800", got)
	}
	e.assertInvariants(t)
}

func TestUnknownWallet_RollsBackAndDoesNotConsumeKey(t *testing.T) {
	e := newEnv(t)
	e.wallet(t, "a", 100)

	_, err := e.transfer("retry-me", "a", "ghost", 10)
	if !errors.Is(err, process.ErrWalletNotFound) {
		t.Fatalf("got %v, want ErrWalletNotFound", err)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM idempotency_records`); n != 0 {
		t.Errorf("idempotency key must not be stored after a rollback, found %d", n)
	}
	if n := e.count(t, `SELECT COUNT(*) FROM transfers`); n != 0 {
		t.Errorf("no transfer row should survive the rollback, found %d", n)
	}

	e.wallet(t, "ghost", 0)
	res, err := e.transfer("retry-me", "a", "ghost", 10)
	if err != nil || res.Replayed || res.Transfer.Status != process.StatusProcessed {
		t.Fatalf("retry after fixing the cause should process normally: %+v %v", res, err)
	}
	e.assertInvariants(t)
}

func TestStateTransition_TerminalStateCannotBeOverwritten(t *testing.T) {
	e := newEnv(t)
	e.wallet(t, "a", 100)
	e.wallet(t, "b", 0)
	res, err := e.transfer("k", "a", "b", 10)
	if err != nil {
		t.Fatal(err)
	}

	err = e.store.WithTx(context.Background(), func(tx process.Tx) error {
		return tx.TransitionTransfer(context.Background(), res.Transfer.ID, process.StatusPending, process.StatusFailed, "late writer")
	})
	if !errors.Is(err, process.ErrInvalidTransition) {
		t.Fatalf("got %v, want ErrInvalidTransition", err)
	}
	got, _ := e.svc.GetTransfer(context.Background(), res.Transfer.ID)
	if got.Status != process.StatusProcessed {
		t.Errorf("status changed to %s", got.Status)
	}
}

// 20 clients retry the same request at the same instant -> exactly one transfer happens.
func TestConcurrency_SameIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	e.wallet(t, "a", 1000)
	e.wallet(t, "b", 0)

	const n = 20
	results := make([]*process.CreateTransferResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = e.transfer("dup-key", "a", "b", 100)
		}(i)
	}
	close(start)
	wg.Wait()

	fresh := 0
	var id string
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d failed: %v", i, errs[i])
		}
		if id == "" {
			id = results[i].Transfer.ID
		}
		if results[i].Transfer.ID != id {
			t.Errorf("request %d returned a different transfer id", i)
		}
		if !results[i].Replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Errorf("exactly one request should execute the transfer, got %d", fresh)
	}
	if got := e.balance(t, "a"); got != 900 {
		t.Errorf("a = %d, want 900", got)
	}
	e.assertInvariants(t)
}

// 50 different transfers of 10 from a wallet holding 100 -> exactly 10 succeed (no double spend).
func TestConcurrency_NoDoubleSpend(t *testing.T) {
	e := newEnv(t)
	e.wallet(t, "a", 100)
	e.wallet(t, "b", 0)

	const n = 50
	statuses := make([]process.TransferStatus, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := e.transfer(fmt.Sprintf("key-%d", i), "a", "b", 10)
			errs[i] = err
			if err == nil {
				statuses[i] = res.Transfer.Status
			}
		}(i)
	}
	close(start)
	wg.Wait()

	processed, failed := 0, 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		switch statuses[i] {
		case process.StatusProcessed:
			processed++
		case process.StatusFailed:
			failed++
		}
	}
	if processed != 10 || failed != 40 {
		t.Errorf("processed=%d failed=%d, want 10/40", processed, failed)
	}
	if e.balance(t, "a") != 0 || e.balance(t, "b") != 100 {
		t.Errorf("balances a=%d b=%d, want 0/100", e.balance(t, "a"), e.balance(t, "b"))
	}
	e.assertInvariants(t)
}

// A->B and B->A at the same time is the classic deadlock; ordered locking must prevent it.
func TestConcurrency_OppositeDirectionsDoNotDeadlock(t *testing.T) {
	e := newEnv(t)
	e.wallet(t, "a", 1000)
	e.wallet(t, "b", 1000)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const n = 100
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			from, to := "a", "b"
			if i%2 == 1 {
				from, to = "b", "a"
			}
			<-start
			_, errs[i] = e.svc.CreateTransfer(ctx, process.CreateTransferInput{
				IdempotencyKey: fmt.Sprintf("swap-%d", i), FromWalletID: from, ToWalletID: to, Amount: 1,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d failed (deadlock?): %v", i, err)
		}
	}
	if total := e.balance(t, "a") + e.balance(t, "b"); total != 2000 {
		t.Errorf("money was created or destroyed: total=%d", total)
	}
	e.assertInvariants(t)
}
