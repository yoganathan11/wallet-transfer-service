package process

import "time"

type Wallet struct {
	ID        string
	Balance   int64 // minor units
	CreatedAt time.Time
}

func (w *Wallet) CanDebit(amount int64) bool { return w.Balance >= amount }
