package googleworkspace

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// asciiFold maps the Swedish letters likely to appear in a KTHAIS member's
// name to their ASCII equivalents for email-address purposes. Decided
// 2026-09-07: simple fold (å/ä->a, ö->o), not a digraph fold (ä->ae) —
// matches how most Swedish organizations handle this in practice.
var asciiFold = strings.NewReplacer(
	"å", "a", "Å", "a",
	"ä", "a", "Ä", "a",
	"ö", "o", "Ö", "o",
)

func foldToEmailLocal(s string) string {
	s = asciiFold.Replace(s)
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ResolvePrimaryEmail finds an available firstname.lastname@domain address
// for a new member, checking via UserExists before ever calling CreateUser
// — never inserts speculatively. On a collision it appends a number
// (firstname.lastname2, firstname.lastname3, ...) — decided 2026-09-07,
// simpler and unambiguous compared to a surname-initial fallback tier.
func ResolvePrimaryEmail(ctx context.Context, p Provisioner, domain, firstName, lastName string) (string, error) {
	base := foldToEmailLocal(firstName) + "." + foldToEmailLocal(lastName)
	if base == "." {
		return "", fmt.Errorf("first name and last name must not both be empty")
	}

	const maxAttempts = 50
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		local := base
		if attempt > 1 {
			local += strconv.Itoa(attempt)
		}
		candidate := local + "@" + domain

		exists, err := p.UserExists(ctx, candidate)
		if err != nil {
			return "", fmt.Errorf("resolving primary email for %s %s: %w", firstName, lastName, err)
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find an available address for %s %s after %d attempts", firstName, lastName, maxAttempts)
}
