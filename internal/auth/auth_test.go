package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var secret = []byte("a-test-signing-secret-of-some-length")

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	i := NewIssuer(secret, time.Hour)

	token, expires, err := i.Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if d := time.Until(expires); d < 55*time.Minute || d > time.Hour {
		t.Errorf("expiry in %v, want about an hour", d)
	}

	got, err := i.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != "user-1" {
		t.Errorf("subject = %q, want user-1", got)
	}
}

func TestTokenCarriesOnlyTheUserID(t *testing.T) {
	// Property, room and tenant are resolved per request. A token that carried
	// them would keep answering for a tenancy that has since been moved or
	// ended, for as long as the token lives.
	i := NewIssuer(secret, time.Hour)
	token, _, err := i.Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}

	for _, forbidden := range []string{"tenant_id", "contract_id", "property_id", "room_id", "email"} {
		if _, ok := claims[forbidden]; ok {
			t.Errorf("token carries %q", forbidden)
		}
	}
	if claims["sub"] != "user-1" {
		t.Errorf("sub = %#v, want user-1", claims["sub"])
	}
}

func TestVerifyRejectsAnotherSecret(t *testing.T) {
	// The whole weight of every session rests here. This is also why the
	// signing secret is the one credential worth rotating before real tenants
	// are onboarded: anyone holding it mints a session for any tenant.
	token, _, err := NewIssuer([]byte("the-attackers-own-secret-value-here"), time.Hour).Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, err := NewIssuer(secret, time.Hour).Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsAnExpiredToken(t *testing.T) {
	i := NewIssuer(secret, time.Hour)
	token := signWith(t, jwt.SigningMethodHS256, secret, jwt.RegisteredClaims{
		Subject:   "user-1",
		Issuer:    issuer,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
	})

	if _, err := i.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRequiresAnExpiry(t *testing.T) {
	// A token with no exp never stops working. It is properly signed, so
	// nothing but this check stands between it and a permanent session.
	i := NewIssuer(secret, time.Hour)
	token := signWith(t, jwt.SigningMethodHS256, secret, jwt.RegisteredClaims{
		Subject: "user-1",
		Issuer:  issuer,
	})

	if _, err := i.Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsAnUnsignedToken(t *testing.T) {
	// "alg": "none". The parser is pinned to HS256, which is what closes this
	// and the HMAC/RSA confusion it belongs to.
	token := signWith(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, jwt.RegisteredClaims{
		Subject:   "user-1",
		Issuer:    issuer,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})

	if _, err := NewIssuer(secret, time.Hour).Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsAnotherIssuer(t *testing.T) {
	// Same secret, different issuer: a token minted by something else that was
	// handed the same key must not open a session here.
	token := signWith(t, jwt.SigningMethodHS256, secret, jwt.RegisteredClaims{
		Subject:   "user-1",
		Issuer:    "somewhere-else",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})

	if _, err := NewIssuer(secret, time.Hour).Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsASubjectlessToken(t *testing.T) {
	// Without a subject there is no user to be, and an empty string would
	// match whichever row carries one.
	token := signWith(t, jwt.SigningMethodHS256, secret, jwt.RegisteredClaims{
		Issuer:    issuer,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	})

	if _, err := NewIssuer(secret, time.Hour).Verify(token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	i := NewIssuer(secret, time.Hour)
	for _, raw := range []string{"", "not-a-token", "a.b.c", strings.Repeat("x", 4096)} {
		if _, err := i.Verify(raw); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Verify(%.20q) = %v, want ErrInvalidToken", raw, err)
		}
	}
}

func TestDefaultTTL(t *testing.T) {
	_, expires, err := NewIssuer(secret, 0).Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if d := time.Until(expires); d < 11*time.Hour || d > 12*time.Hour {
		t.Errorf("default expiry in %v, want about twelve hours", d)
	}
}

func signWith(t *testing.T, method jwt.SigningMethod, key any, claims jwt.RegisteredClaims) string {
	t.Helper()
	token, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return token
}
