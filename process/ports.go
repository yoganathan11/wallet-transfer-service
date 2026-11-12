package process

import "context"

// Tx is the set of operations available inside one database transaction.
// The service decides WHAT happens in a transaction; the repository decides HOW.
type Tx interface {
	// ClaimIdempotencyKey inserts the record. claimed=false means the key already exists.
	// If another in-flight transaction holds the same key, this blocks until it commits or rolls back.
	ClaimIdempotencyKey(ctx context.Context, rec IdempotencyRecord) (claimed bool, err error)
	GetIdempotencyRecord(ctx context.Context, key string) (*IdempotencyRecord, error)

	GetTransfer(ctx context.Context, id string) (*Transfer, error)
	InsertTransfer(ctx context.Context, t *Transfer) error
	// TransitionTransfer is a guarded update: it only succeeds if the row is currently in `from`.
	TransitionTransfer(ctx context.Context, id string, from, to TransferStatus, reason string) error

	// LockWallet does SELECT ... FOR UPDATE and returns the locked row.
	LockWallet(ctx context.Context, id string) (*Wallet, error)
	AdjustBalance(ctx context.Context, walletID string, delta int64) error
	InsertLedgerEntries(ctx context.Context, entries ...LedgerEntry) error
}

// Store is the persistence port used by the service layer.
type Store interface {
	WithTx(ctx context.Context, fn func(tx Tx) error) error
	GetTransfer(ctx context.Context, id string) (*Transfer, error)
	GetWallet(ctx context.Context, id string) (*Wallet, error)
	CreateWallet(ctx context.Context, id string, initialBalance int64) (*Wallet, error)
}
