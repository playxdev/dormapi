// Package mail delivers the few messages this system sends.
//
// Every one of them is transactional and addressed to one person: verify this
// address, or recover this account. There is no bulk send and no list, which is
// why there is no template engine and no queue — a message that fails to send
// fails the request that asked for it, and the tenant tries again.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Message is one email to one recipient.
//
// Both bodies are required. A message with only HTML lands in the spam folder
// more often, and some clients show the plain part exclusively.
type Message struct {
	To      string
	Subject string
	HTML    string
	Text    string
}

// Sender delivers a message.
//
// An interface rather than a concrete client because the transport is a
// deployment decision, not a design one: the recovery flow must not know which
// company carries the mail.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// Cloudflare sends through Cloudflare Email Sending's REST API.
//
// The Workers binding is the better path for code running on Workers; this
// service is a Go binary elsewhere, so it authenticates with a scoped API
// token instead.
//
// The sending domain must be onboarded to Email Sending before the first send,
// and From must use it — an unonboarded domain is rejected, not queued.
type Cloudflare struct {
	AccountID string
	Token     string
	From      string
	FromName  string

	HTTP *http.Client
}

const sendTimeout = 10 * time.Second

func (c *Cloudflare) Send(ctx context.Context, msg Message) error {
	// The REST API names these fields differently from the Workers binding:
	// `address` rather than `email`, and snake_case throughout.
	body, err := json.Marshal(map[string]any{
		"to":      msg.To,
		"from":    map[string]string{"address": c.From, "name": c.FromName},
		"subject": msg.Subject,
		"html":    msg.HTML,
		"text":    msg.Text,
	})
	if err != nil {
		return fmt.Errorf("mail: encode message: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/email/sending/send", c.AccountID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mail: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mail: send failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("mail: read response: %w", err)
	}

	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("mail: decode response (status %d): %w", resp.StatusCode, err)
	}
	if !out.Success {
		if len(out.Errors) > 0 {
			return fmt.Errorf("mail: rejected: %d %s", out.Errors[0].Code, out.Errors[0].Message)
		}
		return fmt.Errorf("mail: rejected (status %d)", resp.StatusCode)
	}
	return nil
}

// Log is the development transport. It writes what would have been sent and
// reports success.
//
// It logs the recipient and the subject but never the body, because the body
// is where the single-use link is. A development log is still a log.
type Log struct{ Logger *slog.Logger }

func (l Log) Send(ctx context.Context, msg Message) error {
	l.Logger.InfoContext(ctx, "mail not sent: no transport configured",
		"to", msg.To, "subject", msg.Subject)
	return nil
}
