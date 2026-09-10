package ulid

import (
	"regexp"
	"sort"
	"sync"
	"testing"
)

var shape = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func TestShape(t *testing.T) {
	for i := 0; i < 200; i++ {
		if id := New(); !shape.MatchString(id) {
			t.Fatalf("id %q is not 26 chars of Crockford Base32", id)
		}
	}
}

// The point of a ULID over a UUIDv4: rows written near each other in time land
// near each other in the index, and a list ordered by id is already in
// creation order. Ids from one millisecond must not shuffle.
func TestIdsSortInCreationOrder(t *testing.T) {
	const n = 500
	ids := make([]string, n)
	for i := range ids {
		ids[i] = New()
	}

	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("id %d is out of order: %q, want %q", i, sorted[i], ids[i])
		}
	}
}

// Two goroutines sharing the monotonic counter without a lock hand out the
// same id. The TypeScript original needs no mutex; this service serves
// requests concurrently and does.
func TestConcurrentIdsAreUnique(t *testing.T) {
	const workers, each = 8, 200

	var wg sync.WaitGroup
	out := make(chan string, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				out <- New()
			}
		}()
	}
	wg.Wait()
	close(out)

	seen := make(map[string]bool, workers*each)
	for id := range out {
		if seen[id] {
			t.Fatalf("id %q was handed out twice", id)
		}
		seen[id] = true
	}
}

// The backoffice hashes the normalised code; this service has to reproduce it
// character for character or no QR ever resolves.
func TestNormalizeCrockford(t *testing.T) {
	cases := map[string]string{
		"A7K9Q2MX":    "A7K9Q2MX",
		"a7k9q2mx":    "A7K9Q2MX",
		"A7K9-Q2MX":   "A7K9Q2MX",
		" a7k9 q2mx ": "A7K9Q2MX",
		// The characters Crockford drops, mapped back to what they are read as.
		"ILOU": "110V",
		"ilou": "110V",
	}
	for in, want := range cases {
		if got := NormalizeCrockford(in); got != want {
			t.Errorf("NormalizeCrockford(%q) = %q, want %q", in, got, want)
		}
	}
}
