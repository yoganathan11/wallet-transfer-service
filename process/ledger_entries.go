package process

import "time"

type EntryType string

const (
	EntryDebit  EntryType = "DEBIT"
	EntryCredit EntryType = "CREDIT"
)

type LedgerEntry struct {
	ID         int64
	WalletID   string
	TransferID string
	Type       EntryType
	Amount     int64
	CreatedAt  time.Time
}
