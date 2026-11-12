package process_test

import (
	"errors"
	"testing"
	"time"

	"wallet-transfer-service/process"
)

func TestValidateTransferRequest(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		from, to string
		amount   int64
		want     error
	}{
		{"valid", "k", "a", "b", 100, nil},
		{"valid without key", "", "a", "b", 1, nil},
		{"zero amount", "k", "a", "b", 0, process.ErrInvalidAmount},
		{"negative amount", "k", "a", "b", -5, process.ErrInvalidAmount},
		{"same wallet", "k", "a", "a", 10, process.ErrSameWallet},
		{"missing from", "k", "", "b", 10, process.ErrMissingWalletID},
		{"missing to", "k", "a", "", 10, process.ErrMissingWalletID},
		{"key too long", string(make([]byte, 256)), "a", "b", 10, process.ErrInvalidIdempotencyKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := process.ValidateTransferRequest(tc.key, tc.from, tc.to, tc.amount)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTransferStateMachine(t *testing.T) {
	now := time.Now()
	newPending := func() *process.Transfer {
		tr, err := process.NewTransfer("t1", "k", "a", "b", 100, now)
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}

	t.Run("PENDING -> PROCESSED", func(t *testing.T) {
		tr := newPending()
		if err := tr.MarkProcessed(now); err != nil || tr.Status != process.StatusProcessed {
			t.Fatalf("err=%v status=%s", err, tr.Status)
		}
	})
	t.Run("PENDING -> FAILED keeps reason", func(t *testing.T) {
		tr := newPending()
		if err := tr.MarkFailed(process.FailureInsufficientFunds, now); err != nil {
			t.Fatal(err)
		}
		if tr.Status != process.StatusFailed || tr.FailureReason != process.FailureInsufficientFunds {
			t.Fatalf("unexpected: %+v", tr)
		}
	})
	t.Run("terminal states cannot transition again", func(t *testing.T) {
		tr := newPending()
		_ = tr.MarkProcessed(now)
		if err := tr.MarkProcessed(now); !errors.Is(err, process.ErrInvalidTransition) {
			t.Fatalf("PROCESSED -> PROCESSED: got %v", err)
		}
		if err := tr.MarkFailed("x", now); !errors.Is(err, process.ErrInvalidTransition) {
			t.Fatalf("PROCESSED -> FAILED: got %v", err)
		}
		tr2 := newPending()
		_ = tr2.MarkFailed("x", now)
		if err := tr2.MarkProcessed(now); !errors.Is(err, process.ErrInvalidTransition) {
			t.Fatalf("FAILED -> PROCESSED: got %v", err)
		}
	})
}

func TestLedgerEntriesBalance(t *testing.T) {
	tr, _ := process.NewTransfer("t1", "", "a", "b", 250, time.Now())
	entries := tr.LedgerEntries(time.Now())
	if len(entries) != 2 {
		t.Fatalf("want exactly 2 entries, got %d", len(entries))
	}
	debit, credit := entries[0], entries[1]
	if debit.Type != process.EntryDebit || debit.WalletID != "a" ||
		credit.Type != process.EntryCredit || credit.WalletID != "b" {
		t.Fatalf("wrong entry shape: %+v", entries)
	}
	if debit.Amount != credit.Amount || debit.Amount != 250 {
		t.Fatalf("ledger does not balance: %+v", entries)
	}
}
