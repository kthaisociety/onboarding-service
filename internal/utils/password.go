package utils

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// passwordAlphabet avoids visually ambiguous characters (0/O, 1/l/I) since a
// human retypes this once at first login.
const passwordAlphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"

// GenerateTempPassword returns a random 20-character temporary password for
// a freshly provisioned Google Workspace account. Never persisted anywhere
// (see internal/provisioning) — held in memory only, from creation to the
// one account-info email that sends it to the member's confirmed kth.se
// address.
func GenerateTempPassword() (string, error) {
	const length = 20
	buf := make([]byte, length)
	for i := range buf {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(passwordAlphabet))))
		if err != nil {
			return "", fmt.Errorf("failed to generate temp password: %w", err)
		}
		buf[i] = passwordAlphabet[n.Int64()]
	}
	return string(buf), nil
}
