package line

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func messenger(t *testing.T, status int, body string) (*Messaging, *[]map[string]any, *[]string) {
	t.Helper()
	var sent []map[string]any
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		sent = append(sent, payload)
		auth = append(auth, r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	m := NewMessaging("a-messaging-token")
	m.endpoint = srv.URL
	return m, &sent, &auth
}

func TestReplyAnswersTheTokenTheEventCarried(t *testing.T) {
	m, sent, auth := messenger(t, http.StatusOK, `{}`)

	err := m.Reply(context.Background(), "a-reply-token",
		Message{Text: "ติดต่อผู้ดูแลหอพัก"})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	payload := (*sent)[0]
	if payload["replyToken"] != "a-reply-token" {
		t.Errorf("replyToken = %v", payload["replyToken"])
	}
	if (*auth)[0] != "Bearer a-messaging-token" {
		t.Errorf("authorization = %v", (*auth)[0])
	}

	messages, _ := payload["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", payload["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "ติดต่อผู้ดูแลหอพัก" {
		t.Errorf("message = %#v", first)
	}
}

// A reply token is single use and short lived, so the caller needs to know it
// failed — but it must never retry, and it must never fail the webhook
// request, because that makes LINE redeliver an event already handled.
func TestReplyReportsARejection(t *testing.T) {
	m, _, _ := messenger(t, http.StatusBadRequest, `{"message":"Invalid reply token"}`)

	err := m.Reply(context.Background(), "a-stale-token", Message{Text: "hello"})
	if err == nil {
		t.Fatal("a rejected reply was reported as sent")
	}
	if !strings.Contains(err.Error(), "Invalid reply token") {
		t.Errorf("err = %v, want LINE's own reason", err)
	}
}

// Neither of these is worth a network round trip, and neither should look like
// a message that went out.
func TestReplyRefusesWithoutATokenOrACredential(t *testing.T) {
	m, sent, _ := messenger(t, http.StatusOK, `{}`)

	if err := m.Reply(context.Background(), "", Message{Text: "hello"}); err == nil {
		t.Error("a reply with no token was attempted")
	}

	m.token = ""
	if err := m.Reply(context.Background(), "a-reply-token", Message{Text: "hello"}); err == nil {
		t.Error("a reply with no access token was attempted")
	}
	if len(*sent) != 0 {
		t.Errorf("something was sent anyway: %#v", *sent)
	}
}

func TestReplyReportsAnUnreachableAPI(t *testing.T) {
	m := NewMessaging("a-messaging-token")
	m.endpoint = "http://127.0.0.1:1"
	if err := m.Reply(context.Background(), "a-reply-token", Message{Text: "hello"}); err == nil {
		t.Fatal("an unreachable API reported success")
	}
}
