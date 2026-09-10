// Package pii holds the matching hash for personal data and for invitation
// codes.
//
// It is a port of `hashField` from the backoffice's `src/lib/crypto.ts`, and
// its only reason to exist is that both services must produce the same digest
// from the same input. The backoffice writes `invitation.secret_hash`; this
// service resolves a code by computing that hash and looking it up. One byte
// of difference and no invitation is ever found.
//
// Encryption is deliberately not ported. Reading `party.phone_enc` or
// `resident_profile.national_id_enc` requires a permission and writes an audit
// row on every call (STANDARD §12.5); the tenant-facing API reads neither, so
// DATA_MASTER_KEY has no business being in this process at all.
package pii

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// Pepper is the server-held HMAC key, PII_PEPPER, base64 as it is stored in
// the secret store.
//
// A bare SHA-256 of a Thai mobile number is not a hash: there are about a
// hundred million of them and a leaked database would be reversed by brute
// force in minutes. The pepper is what makes the stored value useless without
// the secret store.
//
// Rotating it invalidates every stored hash and breaks phone matching, so it
// is effectively permanent.
type Pepper []byte

// ParsePepper decodes the configured value.
func ParsePepper(encoded string) (Pepper, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("pii: PII_PEPPER is not base64: %w", err)
	}
	if len(raw) < 32 {
		return nil, fmt.Errorf("pii: PII_PEPPER is %d bytes, want at least 32", len(raw))
	}
	return Pepper(raw), nil
}

// Hash is HMAC-SHA256 of the normalised value, base64 encoded.
//
// The normalisation is the backoffice's, quirk included: a value that looks
// like a Thai phone number is canonicalised to E.164 first, and anything else
// is trimmed and lowercased. That means an invitation code of digits alone
// beginning with 0 is hashed as though it were a phone number. It is
// deterministic on both sides, which is all the lookup needs, and the
// alternative — one service normalising differently from the other — is a
// class of bug that presents as "the QR does not work" with nothing in the
// logs. Kept identical on purpose; if the backoffice's version changes, this
// one changes in the same commit.
func (p Pepper) Hash(value string) string {
	mac := hmac.New(sha256.New, p)
	if phone, ok := NormalizePhone(value); ok {
		mac.Write([]byte(phone))
	} else {
		mac.Write([]byte(strings.ToLower(strings.TrimSpace(value))))
	}
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// NormalizePhone puts a Thai mobile number into E.164, so the same person
// hashes the same way however they wrote it down.
func NormalizePhone(input string) (string, bool) {
	var digits strings.Builder
	for _, r := range input {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	d := digits.String()
	switch {
	case d == "":
		return "", false
	case strings.HasPrefix(d, "66"):
		return "+" + d, true
	case strings.HasPrefix(d, "0"):
		return "+66" + d[1:], true
	default:
		return "", false
	}
}
