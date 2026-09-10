// Package ulid generates the identifiers every XYZ primary key is made of.
//
// STANDARD §13.1: 26 characters, Crockford Base32, 48 bits of millisecond
// timestamp in the high bits. Rows written near each other in time land near
// each other in the index, which UUIDv4 cannot do, and `ORDER BY <id>` is
// already newest-last without a second column.
//
// This is a port of the backoffice's `src/lib/ulid.ts`. Both services write to
// the same tables, so the two must agree on the alphabet, the length and the
// normalisation rules — a code normalised one way here and another way there
// hashes to a different value and opens nothing.
//
// An id must not carry tenant_id or any business meaning. An id with meaning
// lies the moment the thing it describes moves.
package ulid

import (
	"crypto/rand"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Crockford Base32: no I, L, O or U. I/L/O are confusable with 1/0, and
// dropping U keeps accidental profanity out of a code a human may read aloud.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	timeLen   = 10
	randomLen = 16
)

// Monotonicity is per process. Two ids created in the same millisecond
// increment the random field rather than re-rolling it, so a set of rows
// written in one request still sorts in creation order. Without it, ids from
// the same millisecond order arbitrarily and a paged list can repeat or skip.
//
// The mutex is what the TypeScript original does not need: this service serves
// requests concurrently, and two goroutines sharing the counter without it
// would hand out the same id.
var (
	mu         sync.Mutex
	lastTime   int64
	lastRandom []byte
)

// New returns a fresh identifier.
func New() string { return at(time.Now().UnixMilli()) }

func at(now int64) string {
	mu.Lock()
	defer mu.Unlock()

	if now == lastTime && lastRandom != nil {
		lastRandom = increment(lastRandom)
	} else {
		lastTime = now
		lastRandom = randomChars()
	}

	var b strings.Builder
	b.Grow(timeLen + randomLen)
	b.WriteString(encodeTime(now))
	for _, n := range lastRandom {
		b.WriteByte(alphabet[n])
	}
	return b.String()
}

func encodeTime(now int64) string {
	out := make([]byte, timeLen)
	for i := timeLen - 1; i >= 0; i-- {
		out[i] = alphabet[now%32]
		now /= 32
	}
	return string(out)
}

// One random byte per character wastes three bits each and keeps every
// character uniform over the alphabet: 32 divides 256, so the modulo is not
// biased.
func randomChars() []byte {
	buf := make([]byte, randomLen)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail on any platform this runs on, and there is
		// no safe id to return if it did.
		panic("ulid: " + err.Error())
	}
	for i := range buf {
		buf[i] %= 32
	}
	return buf
}

func increment(chars []byte) []byte {
	out := make([]byte, len(chars))
	copy(out, chars)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] < 31 {
			out[i]++
			return out
		}
		out[i] = 0
	}
	// Overflowed all 80 bits inside one millisecond. Not reachable in
	// practice; re-rolling is still correct, only no longer strictly
	// monotonic.
	return randomChars()
}

// NormalizeCrockford repairs what a human typed or a scanner misread:
// uppercase, strip dashes and spaces, then map the characters Crockford
// deliberately excluded onto the ones they are mistaken for.
//
// Used for invitation codes (STANDARD §7.2), never for ids arriving from our
// own database. It must stay character-for-character identical to the
// backoffice's, because the backoffice hashes the normalised code and this
// service has to reproduce that hash to find it.
func NormalizeCrockford(input string) string {
	var b strings.Builder
	b.Grow(len(input))
	for _, r := range strings.ToUpper(input) {
		switch {
		case unicode.IsSpace(r) || r == '-':
			continue
		}
		switch r {
		case 'I', 'L':
			b.WriteRune('1')
		case 'O':
			b.WriteRune('0')
		case 'U':
			b.WriteRune('V')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
