package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// CashSessionIdentities returns the canonical key and the historical Sheets
// key. Upserts recognize both and preserve whichever already exists, including
// after a raw backend migration. New rows use the canonical key in both stores.
func CashSessionIdentities(date time.Time, firstHandID string) [2]string {
	hash := func(clock string) string {
		sum := sha256.Sum256([]byte(clock + "|" + firstHandID))
		return hex.EncodeToString(sum[:])[:12]
	}
	return [2]string{hash(date.UTC().Format(time.RFC3339)), hash(date.Format("2006-01-02 15:04"))}
}
