package process

import "errors"

var (
	ErrInvalidAmount         = errors.New("amount must be a positive integer (minor units)")
	ErrMissingWalletID       = errors.New("fromWalletId and toWalletId are required")
	ErrSameWallet            = errors.New("fromWalletId and toWalletId must be different")
	ErrInvalidIdempotencyKey = errors.New("idempotencyKey must be at most 255 characters")
	ErrInvalidWallet         = errors.New("wallet id is required and initialBalance must be >= 0")
	ErrWalletNotFound        = errors.New("wallet not found")
	ErrWalletExists          = errors.New("wallet already exists")
	ErrTransferNotFound      = errors.New("transfer not found")
	ErrIdempotencyKeyReuse   = errors.New("idempotencyKey was already used with a different request")
	ErrInvalidTransition     = errors.New("invalid transfer state transition")
)
