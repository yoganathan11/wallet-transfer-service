// Package process holds the business workflow: idempotency, locking order, balance checks,
// ledger writes and state transitions. It knows nothing about HTTP or SQL.
package process

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"sort"
	"time"
)

type TransferService struct {
	store Store
	log   *slog.Logger
	now   func() time.Time
	newID func() string
}

func New(store Store, log *slog.Logger) *TransferService {
	return &TransferService{store: store, log: log, now: time.Now, newID: newUUID}
}

type CreateTransferInput struct {
	IdempotencyKey string // optional
	FromWalletID   string
	ToWalletID     string
	Amount         int64 // minor units
}

type CreateTransferResult struct {
	Transfer *Transfer
	Replayed bool // true when this is the stored result of an earlier request with the same key
}

// CreateTransfer performs a wallet-to-wallet transfer atomically, with exactly-once effect per idempotency key.
//
// Everything below runs in ONE database transaction:
//  1. claim the idempotency key (or detect it is a duplicate and return the stored transfer)
//  2. insert the transfer as PENDING
//  3. lock both wallets in sorted-id order (prevents deadlocks between A->B and B->A)
//  4. insufficient funds -> PENDING -> FAILED, commit (so a retry returns the same FAILED result)
//  5. otherwise move balances, write the two ledger entries, PENDING -> PROCESSED
//
// Any unexpected error rolls back all of it, including the idempotency key, so a retry starts clean.
func (s *TransferService) CreateTransfer(ctx context.Context, in CreateTransferInput) (*CreateTransferResult, error) {
	if err := ValidateTransferRequest(in.IdempotencyKey, in.FromWalletID, in.ToWalletID, in.Amount); err != nil {
		return nil, err
	}
	reqHash := hashRequest(in)

	var result *CreateTransferResult
	err := s.store.WithTx(ctx, func(tx Tx) error {
		result = nil
		transferID := s.newID()

		// 1. Idempotency
		if in.IdempotencyKey != "" {
			claimed, err := tx.ClaimIdempotencyKey(ctx, IdempotencyRecord{
				Key: in.IdempotencyKey, RequestHash: reqHash, TransferID: transferID,
			})
			if err != nil {
				return err
			}
			if !claimed {
				rec, err := tx.GetIdempotencyRecord(ctx, in.IdempotencyKey)
				if err != nil {
					return err
				}
				if rec.RequestHash != reqHash {
					return ErrIdempotencyKeyReuse
				}
				existing, err := tx.GetTransfer(ctx, rec.TransferID)
				if err != nil {
					return err
				}
				result = &CreateTransferResult{Transfer: existing, Replayed: true}
				return nil
			}
		}

		// 2. Lock wallets in a global order. This MUST happen before inserting the transfer row:
		// the foreign keys on transfers take a FOR KEY SHARE lock on both wallet rows, and that
		// conflicts with FOR UPDATE held/requested by a concurrent transfer -> deadlock.
		ids := []string{in.FromWalletID, in.ToWalletID}
		sort.Strings(ids)
		locked := make(map[string]*Wallet, 2)
		for _, id := range ids {
			w, err := tx.LockWallet(ctx, id)
			if err != nil {
				return err // e.g. ErrWalletNotFound -> rollback, key not consumed
			}
			locked[id] = w
		}

		// 3. Create the PENDING transfer (wallet rows are already locked by us)
		now := s.now()
		transfer, err := NewTransfer(transferID, in.IdempotencyKey, in.FromWalletID, in.ToWalletID, in.Amount, now)
		if err != nil {
			return err
		}
		if err := tx.InsertTransfer(ctx, transfer); err != nil {
			return err
		}

		// 4. Business failure is a recorded outcome, not a rollback
		if !locked[in.FromWalletID].CanDebit(in.Amount) {
			if err := transfer.MarkFailed(FailureInsufficientFunds, s.now()); err != nil {
				return err
			}
			if err := tx.TransitionTransfer(ctx, transfer.ID, StatusPending, StatusFailed, transfer.FailureReason); err != nil {
				return err
			}
			result = &CreateTransferResult{Transfer: transfer}
			return nil
		}

		// 5. Apply the transfer
		if err := tx.AdjustBalance(ctx, in.FromWalletID, -in.Amount); err != nil {
			return err
		}
		if err := tx.AdjustBalance(ctx, in.ToWalletID, in.Amount); err != nil {
			return err
		}
		if err := tx.InsertLedgerEntries(ctx, transfer.LedgerEntries(s.now())...); err != nil {
			return err
		}
		if err := transfer.MarkProcessed(s.now()); err != nil {
			return err
		}
		if err := tx.TransitionTransfer(ctx, transfer.ID, StatusPending, StatusProcessed, ""); err != nil {
			return err
		}
		result = &CreateTransferResult{Transfer: transfer}
		return nil
	})
	if err != nil {
		s.log.WarnContext(ctx, "transfer not applied", "idempotency_key", in.IdempotencyKey, "error", err)
		return nil, err
	}

	s.log.InfoContext(ctx, "transfer handled",
		"transfer_id", result.Transfer.ID,
		"status", result.Transfer.Status,
		"replayed", result.Replayed,
		"from", result.Transfer.FromWalletID,
		"to", result.Transfer.ToWalletID,
		"amount", result.Transfer.Amount,
	)
	return result, nil
}

func (s *TransferService) GetTransfer(ctx context.Context, id string) (*Transfer, error) {
	return s.store.GetTransfer(ctx, id)
}

func (s *TransferService) GetWallet(ctx context.Context, id string) (*Wallet, error) {
	return s.store.GetWallet(ctx, id)
}

func (s *TransferService) CreateWallet(ctx context.Context, id string, initialBalance int64) (*Wallet, error) {
	if id == "" || initialBalance < 0 {
		return nil, ErrInvalidWallet
	}
	return s.store.CreateWallet(ctx, id, initialBalance)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
