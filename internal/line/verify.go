// Package line verifies LINE identity tokens.
//
// The MINI App sends the ID token it obtained from LIFF. This service asks
// LINE to verify the signature and claims rather than trusting anything the
// browser asserts. No channel secret is involved: the verification endpoint
// authenticates the token itself, not the caller.
package line

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const verifyURL = "https://api.line.me/oauth2/v2.1/verify"

// ErrInvalidToken means LINE rejected the token. It is never the client's
// fault in a way worth detailing to them, so callers map it to 401.
var ErrInvalidToken = errors.New("line: invalid id token")

// Identity is what LINE confirms about the user.
type Identity struct {
	// UserID is LINE's stable identifier for this user, from the `sub` claim.
	UserID string
	// DisplayName and PictureURL require the profile scope; both may be empty.
	DisplayName string
	PictureURL  string
}

type Verifier struct {
	// channelID is the expected `aud`. A token minted for another channel must
	// not be accepted here.
	channelID string
	// endpoint is LINE's verification URL. Only a test ever changes it, and
	// only to a server answering in LINE's own shape - the audience check
	// below is the whole point of this type and cannot be tested without one.
	endpoint string
	http     *http.Client
}

func NewVerifier(channelID string) *Verifier {
	return &Verifier{
		channelID: channelID,
		endpoint:  verifyURL,
		http:      &http.Client{Timeout: 10 * time.Second},
	}
}

type verifyResponse struct {
	Sub     string `json:"sub"`
	Aud     string `json:"aud"`
	Iss     string `json:"iss"`
	Exp     int64  `json:"exp"`
	Name    string `json:"name"`
	Picture string `json:"picture"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Verify checks an ID token with LINE and returns the confirmed identity.
func (v *Verifier) Verify(ctx context.Context, idToken string) (*Identity, error) {
	if strings.TrimSpace(idToken) == "" {
		return nil, ErrInvalidToken
	}

	form := url.Values{}
	form.Set("id_token", idToken)
	form.Set("client_id", v.channelID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("line: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := v.http.Do(req)
	if err != nil {
		// A transport failure is our problem, not the user's: it must not be
		// reported as an invalid token.
		return nil, fmt.Errorf("line: verify request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("line: read response: %w", err)
	}

	var out verifyResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("line: decode response (status %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK || out.Error != "" {
		return nil, ErrInvalidToken
	}

	// LINE has already checked the signature, issuer and expiry. Re-check the
	// audience anyway: it is the claim that keeps a token minted for a
	// different channel from being accepted here.
	if out.Aud != v.channelID || out.Sub == "" {
		return nil, ErrInvalidToken
	}

	return &Identity{
		UserID:      out.Sub,
		DisplayName: out.Name,
		PictureURL:  out.Picture,
	}, nil
}
