package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"fmt"
	"strings"
)

// setupPasswordBytes is the entropy behind the first-time password: 120 bits,
// which is far past guessing and still short enough to be read off a terminal
// and typed into a browser on another machine.
const setupPasswordBytes = 15

// Setup guards the first-time wizard.
//
// While nobody has ever signed in there is no identity to authenticate, so the
// wizard is guarded by a password generated for this process and printed in its
// log — the only channel a server nobody can sign in to already shares with the
// operator. It never reaches the disk and dies with the process, so a restart
// is also a revocation, and the password in this run's log is the only one that
// works.
//
// Only the digest is kept, as with a session cookie. It buys less here, since
// there is no store to leak, but a secret that does not have to be held in full
// is not held in full.
type Setup struct {
	digest [sha256.Size]byte
}

// NewSetup generates this process's setup password and returns it beside the
// guard. This is the only time it exists in full: printing it is the caller's
// job, and there is no second chance to ask for it.
func NewSetup() (*Setup, string, error) {
	raw := make([]byte, setupPasswordBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", fmt.Errorf("generate the setup password: %w", err)
	}

	// Base32 over A-Z2-7: no pair of characters that a person retyping could
	// confuse, unlike base64 with its l/I and O/0.
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	password := group(encoded, 4)
	return &Setup{digest: sha256.Sum256([]byte(normalizePassword(password)))}, password, nil
}

// Verify reports whether password is the one this process printed. Hyphens,
// spaces and case are ignored: the password is read off a log and typed back,
// and refusing a correct password over a lost hyphen would only teach the
// operator to paste it somewhere else first.
func (s *Setup) Verify(password string) bool {
	got := sha256.Sum256([]byte(normalizePassword(password)))
	return subtle.ConstantTimeCompare(got[:], s.digest[:]) == 1
}

func normalizePassword(password string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "").Replace(strings.TrimSpace(password)))
}

// group breaks a string into hyphenated runs of n characters, so it can be read
// aloud and typed without losing the place.
func group(s string, n int) string {
	var b strings.Builder
	for i, c := range s {
		if i > 0 && i%n == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(c)
	}
	return b.String()
}
