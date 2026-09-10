package line

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const replyURL = "https://api.line.me/v2/bot/message/reply"

// Replier answers a webhook event in the chat it arrived from.
//
// An interface so the handler can be tested without a channel access token and
// without sending anyone a message.
type Replier interface {
	Reply(ctx context.Context, replyToken string, message Message) error
}

// Message is one text bubble, optionally with quick replies under it.
type Message struct {
	Text string

	// Choices become quick-reply buttons. Each carries opaque data that comes
	// back as a postback event — never a tenant this service then trusts: the
	// value is checked against the sender's memberships before it is used
	// (STANDARD §4.3.1).
	Choices []Choice
}

// Choice is one quick-reply button.
type Choice struct {
	Label string
	Data  string
}

// Messaging talks to the LINE Messaging API.
//
// Its credential is the Messaging channel's access token, which is not the
// channel secret that verifies a webhook and not the Login channel this
// service verifies ID tokens against. Three different secrets, three different
// jobs.
type Messaging struct {
	token    string
	endpoint string
	http     *http.Client
}

func NewMessaging(accessToken string) *Messaging {
	return &Messaging{
		token:    accessToken,
		endpoint: replyURL,
		http:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Reply answers within the window LINE allows for a reply token.
//
// A reply token is single use and short lived, so a failure here is not worth
// retrying: by the time a retry ran the token would be spent or stale. The
// caller logs it and moves on, because the alternative — answering LINE with a
// non-2xx — makes LINE redeliver the whole event.
func (m *Messaging) Reply(ctx context.Context, replyToken string, message Message) error {
	if m.token == "" {
		return fmt.Errorf("line: no messaging access token configured")
	}
	if replyToken == "" {
		return fmt.Errorf("line: no reply token")
	}

	payload := map[string]any{
		"replyToken": replyToken,
		"messages":   []any{textMessage(message)},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("line: encode reply: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("line: build reply: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("line: reply failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("line: reply rejected (%d): %s", resp.StatusCode, detail)
	}
	return nil
}

// textMessage builds the wire form. LINE caps a text message at 5000
// characters and quick replies at 13 items; nothing here comes close, but a
// message that exceeds either is rejected whole, so both are trimmed rather
// than sent to be refused.
func textMessage(m Message) map[string]any {
	text := m.Text
	if len([]rune(text)) > 5000 {
		text = string([]rune(text)[:5000])
	}

	out := map[string]any{"type": "text", "text": text}
	if len(m.Choices) == 0 {
		return out
	}

	choices := m.Choices
	if len(choices) > 13 {
		choices = choices[:13]
	}
	items := make([]any, 0, len(choices))
	for _, c := range choices {
		label := c.Label
		if len([]rune(label)) > 20 {
			label = string([]rune(label)[:20])
		}
		items = append(items, map[string]any{
			"type": "action",
			"action": map[string]any{
				"type":  "postback",
				"label": label,
				"data":  c.Data,
				// Shown in the chat as what the person "said", so the
				// transcript reads as a conversation rather than as a button
				// press with no trace.
				"displayText": label,
			},
		})
	}
	out["quickReply"] = map[string]any{"items": items}
	return out
}
