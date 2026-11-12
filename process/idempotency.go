package process

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// IdempotencyRecord ties a client-supplied key to the transfer it produced.
type IdempotencyRecord struct {
	Key         string
	RequestHash string
	TransferID  string
	CreatedAt   time.Time
}

// hashRequest fingerprints the payload (not the key) so the same key with different data is rejected.
func hashRequest(in CreateTransferInput) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", in.FromWalletID, in.ToWalletID, in.Amount)))
	return hex.EncodeToString(sum[:])
}
