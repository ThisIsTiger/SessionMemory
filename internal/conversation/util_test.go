package conversation

import (
	"crypto/sha256"
	"encoding/hex"
)

func sha256Sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
