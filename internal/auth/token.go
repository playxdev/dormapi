// Package auth issues and validates this service's own session tokens.
//
// A session token carries the internal user ID and nothing else. Property,
// room and tenant are deliberately absent: they are resolved from the database
// on every request so that a revoked or moved tenancy takes effect immediately
// rather than when the token expires.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var ErrInvalidToken = errors.New("auth: invalid session token")

const issuer = "dorm.place"

type Issuer struct {
	secret []byte
	ttl    time.Duration
}

func NewIssuer(secret []byte, ttl time.Duration) *Issuer {
	if ttl == 0 {
		ttl = 12 * time.Hour
	}
	return &Issuer{secret: secret, ttl: ttl}
}

// Issue mints a session token for an internal user ID.
func (i *Issuer) Issue(userID string) (string, time.Time, error) {
	now := time.Now()
	expires := now.Add(i.ttl)

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Subject:   userID,
		Issuer:    issuer,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expires),
	})

	signed, err := token.SignedString(i.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign token: %w", err)
	}
	return signed, expires, nil
}

// Verify returns the user ID carried by a valid token.
func (i *Issuer) Verify(raw string) (string, error) {
	claims := &jwt.RegisteredClaims{}
	// The parser rejects any algorithm other than the one we sign with, which
	// is what closes the "alg: none" and HMAC/RSA confusion attacks.
	_, err := jwt.ParseWithClaims(raw, claims, func(*jwt.Token) (any, error) {
		return i.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil || claims.Subject == "" {
		return "", ErrInvalidToken
	}
	return claims.Subject, nil
}
