package line

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// verifierFor points a Verifier at a server answering in LINE's shape.
func verifierFor(t *testing.T, channelID string, answer map[string]any, status int) *Verifier {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		// The channel is sent as client_id. LINE rejects a mismatch too; this
		// service does not rely on that alone.
		if got := r.PostForm.Get("client_id"); got != channelID {
			t.Errorf("client_id = %q, want %q", got, channelID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)

	v := NewVerifier(channelID)
	v.endpoint = server.URL
	return v
}

func TestVerifyAcceptsATokenForThisChannel(t *testing.T) {
	v := verifierFor(t, "2011358311", map[string]any{
		"sub": "U_abc", "aud": "2011358311", "name": "น้องมาย",
	}, http.StatusOK)

	id, err := v.Verify(context.Background(), "an-id-token")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.UserID != "U_abc" {
		t.Errorf("UserID = %q, want U_abc", id.UserID)
	}
}

func TestVerifyRejectsAnotherChannelsToken(t *testing.T) {
	// The audience is the claim that stops a token minted for a different
	// channel - including this project's own unused MINI App channel - from
	// opening a tenant's account here.
	v := verifierFor(t, "2011358311", map[string]any{
		"sub": "U_abc", "aud": "2011361700",
	}, http.StatusOK)

	if _, err := v.Verify(context.Background(), "an-id-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsASubjectlessToken(t *testing.T) {
	// Without a subject there is nobody to be. Accepting it would resolve to
	// the account whose identity row carries the empty string.
	v := verifierFor(t, "2011358311", map[string]any{"aud": "2011358311"}, http.StatusOK)

	if _, err := v.Verify(context.Background(), "an-id-token"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsLinesOwnError(t *testing.T) {
	v := verifierFor(t, "2011358311", map[string]any{
		"error": "invalid_request", "error_description": "IdToken expired.",
	}, http.StatusBadRequest)

	if _, err := v.Verify(context.Background(), "expired"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrInvalidToken", err)
	}
}

func TestVerifyRejectsAnEmptyTokenWithoutAsking(t *testing.T) {
	asked := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		asked = true
	}))
	t.Cleanup(server.Close)

	v := NewVerifier("2011358311")
	v.endpoint = server.URL
	if _, err := v.Verify(context.Background(), "   "); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrInvalidToken", err)
	}
	if asked {
		t.Error("an empty token was sent to LINE")
	}
}

func TestVerifyDoesNotReportATransportFailureAsABadToken(t *testing.T) {
	// A network fault is this service's problem. Reported as ErrInvalidToken
	// it becomes a 401, and the app logs the tenant out over an outage.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	v := NewVerifier("2011358311")
	v.endpoint = server.URL
	server.Close()

	_, err := v.Verify(context.Background(), "an-id-token")
	if err == nil {
		t.Fatal("Verify succeeded against a closed server")
	}
	if errors.Is(err, ErrInvalidToken) {
		t.Error("a transport failure was reported as an invalid token")
	}
}
