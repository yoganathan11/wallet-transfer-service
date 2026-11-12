package process

import "time"

type TransferStatus string

const (
	StatusPending   TransferStatus = "PENDING"
	StatusProcessed TransferStatus = "PROCESSED"
	StatusFailed    TransferStatus = "FAILED"
)

const (
	FailureInsufficientFunds = "INSUFFICIENT_FUNDS"
	MaxIdempotencyKeyLength  = 255
)

// allowedTransitions is the whole state machine. PROCESSED and FAILED are terminal.
var allowedTransitions = map[TransferStatus][]TransferStatus{
	StatusPending: {StatusProcessed, StatusFailed},
}

func (s TransferStatus) CanTransitionTo(next TransferStatus) bool {
	for _, allowed := range allowedTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

type Transfer struct {
	ID             string
	IdempotencyKey string // empty when the client did not send one
	FromWalletID   string
	ToWalletID     string
	Amount         int64
	Status         TransferStatus
	FailureReason  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ValidateTransferRequest holds the pure input rules so they are testable without a DB.
func ValidateTransferRequest(idempotencyKey, from, to string, amount int64) error {
	if len(idempotencyKey) > MaxIdempotencyKeyLength {
		return ErrInvalidIdempotencyKey
	}
	if from == "" || to == "" {
		return ErrMissingWalletID
	}
	if from == to {
		return ErrSameWallet
	}
	if amount <= 0 {
		return ErrInvalidAmount
	}
	return nil
}

// NewTransfer creates a transfer in PENDING state.
func NewTransfer(id, idempotencyKey, from, to string, amount int64, now time.Time) (*Transfer, error) {
	if err := ValidateTransferRequest(idempotencyKey, from, to, amount); err != nil {
		return nil, err
	}
	return &Transfer{
		ID:             id,
		IdempotencyKey: idempotencyKey,
		FromWalletID:   from,
		ToWalletID:     to,
		Amount:         amount,
		Status:         StatusPending,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

func (t *Transfer) transition(next TransferStatus, reason string, now time.Time) error {
	if !t.Status.CanTransitionTo(next) {
		return ErrInvalidTransition
	}
	t.Status = next
	t.FailureReason = reason
	t.UpdatedAt = now
	return nil
}

func (t *Transfer) MarkProcessed(now time.Time) error {
	return t.transition(StatusProcessed, "", now)
}

func (t *Transfer) MarkFailed(reason string, now time.Time) error {
	return t.transition(StatusFailed, reason, now)
}

// LedgerEntries returns the debit (source) and credit (destination) entries for this transfer.
// Both always carry the same amount, so the ledger balances by construction.
func (t *Transfer) LedgerEntries(now time.Time) []LedgerEntry {
	return []LedgerEntry{
		{WalletID: t.FromWalletID, TransferID: t.ID, Type: EntryDebit, Amount: t.Amount, CreatedAt: now},
		{WalletID: t.ToWalletID, TransferID: t.ID, Type: EntryCredit, Amount: t.Amount, CreatedAt: now},
	}
}
