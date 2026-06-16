package setup

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// TokenPrefix marks generated setup tokens so they're recognizable in logs.
const TokenPrefix = "tsm_setup_" // #nosec G101 -- a token-name prefix, not a credential value

// minProvidedTokenLen guards against a trivially weak operator-supplied token.
const minProvidedTokenLen = 16

// ResolveSetupToken decides the raw setup token to hash at boot. When provided
// is non-empty (an operator pre-seeded SETUP_TOKEN, e.g. from a Secret), it is
// trimmed, length-checked, and returned with generated=false — the operator
// already knows it, so the boot path must not echo it. Otherwise a fresh random
// 32-byte token is generated (generated=true) for the operator to read from logs.
func ResolveSetupToken(provided string) (raw string, generated bool, err error) {
	if p := strings.TrimSpace(provided); p != "" {
		if len(p) < minProvidedTokenLen {
			return "", false, fmt.Errorf("provided setup token must be at least %d characters", minProvidedTokenLen)
		}
		return p, false, nil
	}
	b := make([]byte, 32)
	if _, rerr := rand.Read(b); rerr != nil {
		return "", false, fmt.Errorf("generate setup token: %w", rerr)
	}
	return TokenPrefix + base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b), true, nil
}
