// Package db implements process.Store on PostgreSQL using pgx.
// It contains SQL only - no business rules.
package db

import (
	"context"
	_ "embed"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"wallet-transfer-service/process"
)

//go:embed schema.sql
var schemaSQL string

const pgUniqueViolation = "23505"

// dbtx is satisfied by both *pgxpool.Pool and pgx.Tx, so read helpers work in or out of a transaction.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Store struct {
	pool *pgxpool.Pool
}

var _ process.Store = (*Store)(nil)

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Migrate applies the (idempotent) schema.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// WithTx runs fn in one READ COMMITTED transaction. Correctness comes from row locks
// (SELECT ... FOR UPDATE) and constraints, not from a stricter isolation level.
// Returning an error from fn rolls everything back; returning nil commits.
func (s *Store) WithTx(ctx context.Context, fn func(tx process.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	if err := fn(&pgTx{db: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

func (s *Store) GetTransfer(ctx context.Context, id string) (*process.Transfer, error) {
	return getTransfer(ctx, s.pool, id)
}

func (s *Store) GetWallet(ctx context.Context, id string) (*process.Wallet, error) {
	var w process.Wallet
	err := s.pool.QueryRow(ctx,
		`SELECT id, balance, created_at FROM wallets WHERE id = $1`, id,
	).Scan(&w.ID, &w.Balance, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, process.ErrWalletNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get wallet: %w", err)
	}
	return &w, nil
}

func (s *Store) CreateWallet(ctx context.Context, id string, initialBalance int64) (*process.Wallet, error) {
	var w process.Wallet
	err := s.pool.QueryRow(ctx,
		`INSERT INTO wallets (id, balance) VALUES ($1, $2) RETURNING id, balance, created_at`,
		id, initialBalance,
	).Scan(&w.ID, &w.Balance, &w.CreatedAt)
	if isUniqueViolation(err) {
		return nil, process.ErrWalletExists
	}
	if err != nil {
		return nil, fmt.Errorf("create wallet: %w", err)
	}
	return &w, nil
}

// ---- transaction-scoped operations ----

type pgTx struct {
	db dbtx
}

func (t *pgTx) ClaimIdempotencyKey(ctx context.Context, rec process.IdempotencyRecord) (bool, error) {
	// If a concurrent transaction has inserted the same key but not yet committed, this statement
	// WAITS for it. After it commits we get 0 rows affected (key exists); if it rolled back we win.
	tag, err := t.db.Exec(ctx,
		`INSERT INTO idempotency_records (key, request_hash, transfer_id)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (key) DO NOTHING`,
		rec.Key, rec.RequestHash, rec.TransferID)
	if err != nil {
		return false, fmt.Errorf("claim idempotency key: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (t *pgTx) GetIdempotencyRecord(ctx context.Context, key string) (*process.IdempotencyRecord, error) {
	var r process.IdempotencyRecord
	err := t.db.QueryRow(ctx,
		`SELECT key, request_hash, transfer_id, created_at FROM idempotency_records WHERE key = $1`, key,
	).Scan(&r.Key, &r.RequestHash, &r.TransferID, &r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("get idempotency record: %w", err)
	}
	return &r, nil
}

func (t *pgTx) GetTransfer(ctx context.Context, id string) (*process.Transfer, error) {
	return getTransfer(ctx, t.db, id)
}

func (t *pgTx) InsertTransfer(ctx context.Context, tr *process.Transfer) error {
	var key any // NULL when the client sent no key (UNIQUE allows many NULLs)
	if tr.IdempotencyKey != "" {
		key = tr.IdempotencyKey
	}
	_, err := t.db.Exec(ctx,
		`INSERT INTO transfers (id, idempotency_key, from_wallet_id, to_wallet_id, amount, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		tr.ID, key, tr.FromWalletID, tr.ToWalletID, tr.Amount, string(tr.Status), tr.CreatedAt, tr.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert transfer: %w", err)
	}
	return nil
}

func (t *pgTx) TransitionTransfer(ctx context.Context, id string, from, to process.TransferStatus, reason string) error {
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	// "AND status = $2" makes the transition a compare-and-set: a duplicate or racing
	// writer finds 0 rows and gets ErrInvalidTransition instead of overwriting a terminal state.
	tag, err := t.db.Exec(ctx,
		`UPDATE transfers SET status = $3, failure_reason = $4, updated_at = now()
		 WHERE id = $1 AND status = $2`,
		id, string(from), string(to), reasonArg)
	if err != nil {
		return fmt.Errorf("transition transfer: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return process.ErrInvalidTransition
	}
	return nil
}

func (t *pgTx) LockWallet(ctx context.Context, id string) (*process.Wallet, error) {
	var w process.Wallet
	err := t.db.QueryRow(ctx,
		`SELECT id, balance, created_at FROM wallets WHERE id = $1 FOR UPDATE`, id,
	).Scan(&w.ID, &w.Balance, &w.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, process.ErrWalletNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock wallet: %w", err)
	}
	return &w, nil
}

func (t *pgTx) AdjustBalance(ctx context.Context, walletID string, delta int64) error {
	// Relative update (balance + delta) - never write back a value computed from a stale read.
	// CHECK (balance >= 0) would abort the transaction if the service ever got this wrong.
	_, err := t.db.Exec(ctx,
		`UPDATE wallets SET balance = balance + $2, updated_at = now() WHERE id = $1`, walletID, delta)
	if err != nil {
		return fmt.Errorf("adjust balance: %w", err)
	}
	return nil
}

func (t *pgTx) InsertLedgerEntries(ctx context.Context, entries ...process.LedgerEntry) error {
	for _, e := range entries {
		_, err := t.db.Exec(ctx,
			`INSERT INTO ledger_entries (wallet_id, transfer_id, type, amount, created_at)
			 VALUES ($1, $2, $3, $4, $5)`,
			e.WalletID, e.TransferID, string(e.Type), e.Amount, e.CreatedAt)
		if err != nil {
			return fmt.Errorf("insert ledger entry: %w", err)
		}
	}
	return nil
}

// ---- helpers ----

func getTransfer(ctx context.Context, db dbtx, id string) (*process.Transfer, error) {
	var (
		tr     process.Transfer
		key    *string
		reason *string
		status string
	)
	err := db.QueryRow(ctx,
		`SELECT id, idempotency_key, from_wallet_id, to_wallet_id, amount, status, failure_reason, created_at, updated_at
		 FROM transfers WHERE id = $1`, id,
	).Scan(&tr.ID, &key, &tr.FromWalletID, &tr.ToWalletID, &tr.Amount, &status, &reason, &tr.CreatedAt, &tr.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, process.ErrTransferNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get transfer: %w", err)
	}
	tr.Status = process.TransferStatus(status)
	if key != nil {
		tr.IdempotencyKey = *key
	}
	if reason != nil {
		tr.FailureReason = *reason
	}
	return &tr, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}
