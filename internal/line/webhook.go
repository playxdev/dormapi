package line

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrBadSignature means the body did not come from LINE, or did not arrive
// intact.
//
// The channel secret is what makes the difference. The webhook URL is public
// and the payload names a user; without this check anyone who learned the URL
// could claim to be any resident, and the reply would go to that resident's
// chat.
var ErrBadSignature = errors.New("line: bad webhook signature")

// WebhookEvent is one entry in a delivery. Only the fields this service acts
// on are decoded; LINE adds more over time and an unknown field must not be an
// error.
type WebhookEvent struct {
	Type       string `json:"type"` // message|follow|unfollow|postback|…
	Mode       string `json:"mode"` // active|standby
	WebhookID  string `json:"webhookEventId"`
	ReplyToken string `json:"replyToken"`
	Timestamp  int64  `json:"timestamp"`

	Source struct {
		Type   string `json:"type"` // user|group|room
		UserID string `json:"userId"`
	} `json:"source"`

	Message struct {
		Type string `json:"type"` // text|image|…
		ID   string `json:"id"`
		Text string `json:"text"`
	} `json:"message"`

	Postback struct {
		Data string `json:"data"`
	} `json:"postback"`
}

type webhookBody struct {
	Destination string         `json:"destination"`
	Events      []WebhookEvent `json:"events"`
}

// ParseWebhook verifies the signature and decodes the body.
//
// Verification happens first and on the raw bytes. Decoding before checking
// would run a parser on input from anyone who found the URL, and re-encoding
// the decoded form to check it afterwards would not reproduce the bytes LINE
// signed.
//
// LINE also sends a verification request with an empty `events` array when the
// endpoint is saved in the console. That is a valid, signed delivery with
// nothing to do, and it must answer 200.
func ParseWebhook(channelSecret string, signature string, body []byte) ([]WebhookEvent, error) {
	if channelSecret == "" {
		return nil, fmt.Errorf("line: no channel secret configured")
	}

	want, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return nil, ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(channelSecret))
	mac.Write(body)
	// Constant time: a byte-by-byte comparison leaks how much of a forged
	// signature was right, which is enough to build the rest of it.
	if !hmac.Equal(want, mac.Sum(nil)) {
		return nil, ErrBadSignature
	}

	var parsed webhookBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("line: decode webhook: %w", err)
	}
	return parsed.Events, nil
}

// Sign produces the header LINE would send for a body. It exists so a test can
// send a real delivery rather than one with the check turned off.
func Sign(channelSecret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(channelSecret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
