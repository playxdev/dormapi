package httpx

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewTokenIsUnguessableAndStoredHashed(t *testing.T) {
	raw, hash, err := newToken()
	if err != nil {
		t.Fatalf("newToken: %v", err)
	}

	// 32 bytes of crypto/rand. Anything materially shorter is guessable by a
	// caller who is allowed ten attempts every fifteen minutes and patient.
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("token is not base64url: %v", err)
	}
	if len(decoded) != 32 {
		t.Errorf("token carries %d bytes of entropy, want 32", len(decoded))
	}

	// The token travels in a URL, so it must survive one without escaping.
	if strings.ContainsAny(raw, "+/=?&#") {
		t.Errorf("token %q contains characters a URL would have to escape", raw)
	}

	want := sha256.Sum256([]byte(raw))
	if hash != hex.EncodeToString(want[:]) {
		t.Error("stored value is not the SHA-256 of the token")
	}
	if strings.Contains(hash, raw) {
		t.Fatal("the raw token appears in the value written to the database")
	}
	if hash != hashToken(raw) {
		t.Error("hashToken disagrees with newToken, so no issued token could be consumed")
	}
}

func TestNewTokenDoesNotRepeat(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		raw, _, err := newToken()
		if err != nil {
			t.Fatalf("newToken: %v", err)
		}
		if seen[raw] {
			t.Fatal("newToken returned the same token twice")
		}
		seen[raw] = true
	}
}

func TestValidEmail(t *testing.T) {
	cases := []struct {
		in   string
		want string // empty means it must be rejected
	}{
		{"tenant@example.com", "tenant@example.com"},
		{"  Tenant@Example.COM  ", "tenant@example.com"},
		{"first.last+tag@sub.example.co.th", "first.last+tag@sub.example.co.th"},
		{"", ""},
		{"tenant", ""},
		{"tenant@", ""},
		{"@example.com", ""},
		{"tenant @example.com", ""},
		// A display name is a valid address to a mail parser and not to this
		// system: the account is keyed on the address alone, and storing the
		// decorated form would leave two spellings of one account.
		{"Tenant <tenant@example.com>", ""},
		{strings.Repeat("a", 250) + "@example.com", ""},
	}

	for _, c := range cases {
		got, ok := validEmail(c.in)
		if c.want == "" {
			if ok {
				t.Errorf("validEmail(%q) accepted it as %q, want rejected", c.in, got)
			}
			continue
		}
		if !ok {
			t.Errorf("validEmail(%q) rejected a real address", c.in)
			continue
		}
		if got != c.want {
			t.Errorf("validEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTokenTTLs(t *testing.T) {
	// Recovery hands an account to whoever holds the link, so it is short.
	// Verification competes with nothing and may wait for the evening.
	if recoveryTokenTTL > verifyTokenTTL {
		t.Fatal("a recovery link outlives a verification link")
	}
	if recoveryTokenTTL > time.Hour {
		t.Errorf("recovery TTL is %v, long enough for a forwarded mail to still work", recoveryTokenTTL)
	}
}

func TestRateLimitBlocksPastTheLimit(t *testing.T) {
	handler := RateLimit(3, time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 1; i <= 3; i++ {
		if got := callFrom(handler, "203.0.113.9:5000"); got != http.StatusOK {
			t.Fatalf("request %d = %d, want 200", i, got)
		}
	}
	if got := callFrom(handler, "203.0.113.9:5000"); got != http.StatusTooManyRequests {
		t.Errorf("request 4 = %d, want 429", got)
	}

	// A different caller is a different budget. Sharing one would let a single
	// abusive client lock every tenant out of the same endpoint.
	if got := callFrom(handler, "198.51.100.4:5000"); got != http.StatusOK {
		t.Errorf("second caller = %d, want 200", got)
	}
}

func TestRateLimitKeysOnAddressNotPort(t *testing.T) {
	// Every request from one client arrives on a fresh source port. Keying on
	// the whole RemoteAddr would give unlimited attempts to anyone.
	handler := RateLimit(2, time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	callFrom(handler, "203.0.113.9:40001")
	callFrom(handler, "203.0.113.9:40002")
	if got := callFrom(handler, "203.0.113.9:40003"); got != http.StatusTooManyRequests {
		t.Errorf("third request from one address = %d, want 429", got)
	}
}

func TestRateLimitWindowResets(t *testing.T) {
	l := newLimiter(1, 20*time.Millisecond)
	if !l.allow("caller") {
		t.Fatal("first request refused")
	}
	if l.allow("caller") {
		t.Fatal("second request inside the window allowed")
	}

	time.Sleep(30 * time.Millisecond)
	if !l.allow("caller") {
		t.Error("the window never reopened")
	}
}

func TestRateLimitAnswersRetryAfter(t *testing.T) {
	handler := RateLimit(1, time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodPost, "/recovery/request", nil)
	req.RemoteAddr = "203.0.113.9:5000"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a rejected caller is not told when to come back")
	}
	if body := rec.Body.String(); !strings.Contains(body, "rate_limited") {
		t.Errorf("body = %q, want a rate_limited code", body)
	}
}

func callFrom(h http.Handler, addr string) int {
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}
