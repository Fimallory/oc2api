// Package zen builds the request shape the OpenCode Zen free tier accepts.
package zen

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// sessionPattern is OpenCode's canonical session id: "ses_" followed by a
// 12-character descending timestamp and 14 Base62 random characters.
var sessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// timePartPattern restricts the timestamp half to the counter window the free
// tier accepts: a 48-bit inverted timestamp whose low 12 bits are 0xFC0-0xFFF.
var timePartPattern = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

// IsOfficialSession reports whether id is a session minted by the official
// client, and therefore safe to forward unchanged.
func IsOfficialSession(id string) bool {
	return sessionPattern.MatchString(id) && timePartPattern.MatchString(id[4:16])
}

// SessionID derives the upstream session id for signal. Official ids are kept
// verbatim so prompt-cache affinity survives; every other signal is hashed into
// the canonical layout.
func SessionID(signal string) string {
	if IsOfficialSession(signal) {
		return signal
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	return "ses_" + timePart(sum[:6]) + base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
}

// NewSessionID mints a fresh session id that keeps signal's affinity but uses
// new random characters, for retries that must not look like a repeat.
func NewSessionID(signal string) string {
	sum := sha256.Sum256([]byte("ses\x00" + signal + "\x00" + RandomID("nonce", 8)))
	return "ses_" + timePart(sum[:6]) + base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
}

// timePart renders six hash bytes as the official client's descending
// timestamp: a 36-bit value shifted into the top of a 48-bit word, ORed with
// counter 1 and inverted, so the low 12 hex digits read "ffe".
func timePart(hash []byte) string {
	current := (uint64(hash[0])<<28 | uint64(hash[1])<<20 | uint64(hash[2])<<12 |
		uint64(hash[3])<<4 | uint64(hash[4])>>4) << 12
	current |= 1
	inverted := ^current & 0xFFFFFFFFFFFF
	return fmt.Sprintf("%012x", inverted)
}

func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(62)
	out := make([]byte, width)
	remainder := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
	}
	return string(out)
}

// ProjectID renders a project signal as the 40-hex value the v2 gate expects.
func ProjectID(signal string) string {
	if signal == "global" || regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(signal) {
		return signal
	}
	if signal == "" {
		signal = "oc2api:default-project"
	}
	sum := sha256.Sum256([]byte("opencode-project\x00" + signal))
	return hex.EncodeToString(sum[:20])
}

// RandomID returns prefix + "_" + size random bytes in hex.
func RandomID(prefix string, size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

// MessageID returns a fresh ascending message id ("msg_" + hex).
func MessageID() string {
	return RandomID("msg", 12)
}

// Seed fingerprints how a conversation begins so independent conversations do
// not share an upstream session.
func Seed(instructions string, firstUser string) string {
	payload, err := json.Marshal([]string{instructions, firstUser})
	if err != nil {
		return instructions + "\x00" + firstUser
	}
	return string(payload)
}

// Truncate shortens s for log lines.
func Truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
