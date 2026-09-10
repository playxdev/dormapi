package line

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const secret = "a-messaging-channel-secret"

const delivery = `{"destination":"Uabc","events":[{"type":"message","mode":"active",` +
	`"webhookEventId":"01EVENT","replyToken":"a-reply-token","timestamp":1789000000,` +
	`"source":{"type":"user","userId":"U_resident"},` +
	`"message":{"type":"text","id":"1","text":"ADMIN"}}]}`

func TestASignedDeliveryIsAccepted(t *testing.T) {
	body := []byte(delivery)
	events, err := ParseWebhook(secret, Sign(secret, body), body)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	e := events[0]
	if e.Type != "message" || e.Message.Text != "ADMIN" || e.Source.UserID != "U_resident" {
		t.Errorf("event = %+v", e)
	}
	if e.WebhookID != "01EVENT" || e.ReplyToken != "a-reply-token" {
		t.Errorf("event = %+v", e)
	}
}

// The endpoint is a public URL and the payload names a resident. Without the
// signature anyone who found it could claim to be anyone, and the answer would
// land in that resident's chat.
func TestAnUnsignedOrForgedDeliveryIsRejected(t *testing.T) {
	body := []byte(delivery)
	good := Sign(secret, body)

	cases := map[string]string{
		"no signature":        "",
		"not base64":          "not-base64!",
		"another secret":      Sign("some-other-secret", body),
		"right length, wrong": base64.StdEncoding.EncodeToString(make([]byte, 32)),
	}
	for name, signature := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseWebhook(secret, signature, body); !errors.Is(err, ErrBadSignature) {
				t.Errorf("err = %v, want ErrBadSignature", err)
			}
		})
	}

	// A body altered after signing. The signature is over the bytes, so this
	// is the case the check exists for: someone replaying a real delivery with
	// a different userId in it.
	tampered := []byte(strings.Replace(delivery, "U_resident", "U_someoneelse", 1))
	if _, err := ParseWebhook(secret, good, tampered); !errors.Is(err, ErrBadSignature) {
		t.Errorf("err = %v, want a tampered body rejected", err)
	}
}

// Saving the endpoint in the LINE console sends a signed delivery with no
// events in it. It has to be accepted, or the console reports the webhook as
// broken and refuses to enable it.
func TestTheConsolesVerificationDeliveryIsValid(t *testing.T) {
	body := []byte(`{"destination":"Uabc","events":[]}`)
	events, err := ParseWebhook(secret, Sign(secret, body), body)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("got %d events, want none", len(events))
	}
}

// No secret is not the same as no check. A deployment that has not been given
// one must refuse, never wave deliveries through.
func TestNoSecretRefuses(t *testing.T) {
	body := []byte(delivery)
	if _, err := ParseWebhook("", Sign(secret, body), body); err == nil {
		t.Fatal("a delivery was accepted with no secret configured")
	}
}

// Quick replies are how a resident with two operators says which one they
// mean, so the postback data has to survive the round trip intact.
func TestAQuickReplyCarriesItsChoice(t *testing.T) {
	out := textMessage(Message{
		Text: "เลือกหอพัก",
		Choices: []Choice{
			{Label: "หอพักทดสอบ", Data: "select_tenant:01TENANT"},
			{Label: "หออื่น", Data: "select_tenant:01OTHER"},
		},
	})

	quick, ok := out["quickReply"].(map[string]any)
	if !ok {
		t.Fatalf("no quick reply: %#v", out)
	}
	items, _ := quick["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	first, _ := items[0].(map[string]any)
	action, _ := first["action"].(map[string]any)
	if action["type"] != "postback" || action["data"] != "select_tenant:01TENANT" {
		t.Errorf("action = %#v", action)
	}
}

// LINE rejects an over-long message or an over-long quick-reply label whole,
// so both are trimmed rather than sent to be refused.
func TestOversizedMessagesAreTrimmedNotRejected(t *testing.T) {
	out := textMessage(Message{
		Text:    strings.Repeat("ก", 6000),
		Choices: []Choice{{Label: strings.Repeat("ข", 40), Data: "d"}},
	})
	if n := len([]rune(out["text"].(string))); n != 5000 {
		t.Errorf("text is %d runes, want 5000", n)
	}
	quick := out["quickReply"].(map[string]any)
	action := quick["items"].([]any)[0].(map[string]any)["action"].(map[string]any)
	if n := len([]rune(action["label"].(string))); n != 20 {
		t.Errorf("label is %d runes, want 20", n)
	}
}

func TestNoChoicesMeansNoQuickReply(t *testing.T) {
	if _, ok := textMessage(Message{Text: "hello"})["quickReply"]; ok {
		t.Error("a plain message carries an empty quick reply")
	}
}
