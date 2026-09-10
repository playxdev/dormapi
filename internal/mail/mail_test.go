package mail

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const verification = "https://api.example/api/v1/email/verify?token=a-single-use-token"

func message() Message {
	return Message{
		To:      "tenant@example.com",
		Subject: "ยืนยันอีเมลของคุณ · dorm.place",
		Text:    "ยืนยันอีเมลนี้\n\n" + verification,
		HTML:    `<p><a href="` + verification + `">ยืนยันอีเมล</a></p>`,
	}
}

func sender(t *testing.T, status int, body string) (*Cloudflare, *[]map[string]any, *[]string) {
	t.Helper()
	var sent []map[string]any
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		payload["_path"] = r.URL.Path
		sent = append(sent, payload)
		auth = append(auth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	// The endpoint is built from the account id, so the test server stands in
	// by being the whole URL the client would have built.
	c := &Cloudflare{
		AccountID: "acct", Token: "a-mail-token",
		From: "no-reply@dorm.place", FromName: "dorm.place",
		HTTP: &http.Client{Transport: redirect{srv.URL}},
	}
	return c, &sent, &auth
}

// redirect points every request at the test server, whatever URL was built.
type redirect struct{ base string }

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	to, err := http.NewRequestWithContext(req.Context(), req.Method, r.base+req.URL.Path, req.Body)
	if err != nil {
		return nil, err
	}
	to.Header = req.Header
	return http.DefaultTransport.RoundTrip(to)
}

func TestSendPostsTheRESTAPIsFieldNames(t *testing.T) {
	c, sent, auth := sender(t, http.StatusOK, `{"success":true}`)

	if err := c.Send(context.Background(), message()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	payload := (*sent)[0]
	if payload["_path"] != "/client/v4/accounts/acct/email/sending/send" {
		t.Errorf("path = %v", payload["_path"])
	}
	if (*auth)[0] != "Bearer a-mail-token" {
		t.Errorf("authorization = %v", (*auth)[0])
	}
	if payload["to"] != "tenant@example.com" {
		t.Errorf("to = %v", payload["to"])
	}

	// `address`, not `email`: the REST API names this differently from the
	// Workers binding, and the wrong name is rejected rather than defaulted.
	from, ok := payload["from"].(map[string]any)
	if !ok || from["address"] != "no-reply@dorm.place" || from["name"] != "dorm.place" {
		t.Errorf("from = %#v", payload["from"])
	}

	// Both bodies. HTML alone lands in spam more often, and some clients show
	// only the plain part.
	if payload["text"] == "" || payload["html"] == "" {
		t.Errorf("payload = %#v", payload)
	}
}

// Cloudflare reports a rejected message in the body with HTTP 200. Reading
// only the status would tell a tenant their verification link is on its way
// when the domain was never onboarded.
func TestARejectionInTheBodyIsAnError(t *testing.T) {
	c, _, _ := sender(t, http.StatusOK, `{"success":false,"errors":[
		{"code":2036,"message":"Unauthorized"}]}`)

	err := c.Send(context.Background(), message())
	if err == nil {
		t.Fatal("a rejected message was reported as sent")
	}
	// 2036 is what an unonboarded sending domain answers, and is the error
	// this deployment is sitting on today.
	if !strings.Contains(err.Error(), "2036") {
		t.Errorf("err = %v, want the code the caller has to look up", err)
	}
}

func TestARejectionWithNoDetailIsStillAnError(t *testing.T) {
	c, _, _ := sender(t, http.StatusForbidden, `{"success":false,"errors":[]}`)
	if err := c.Send(context.Background(), message()); err == nil {
		t.Fatal("an unsuccessful response was accepted")
	}
}

func TestANonJSONResponseIsAnError(t *testing.T) {
	c, _, _ := sender(t, http.StatusBadGateway, `<html>502</html>`)
	if err := c.Send(context.Background(), message()); err == nil {
		t.Fatal("an HTML error page was read as a successful send")
	}
}

func TestAnUnreachableTransportIsAnError(t *testing.T) {
	c := &Cloudflare{
		AccountID: "acct", Token: "t", From: "no-reply@dorm.place",
		HTTP: &http.Client{Transport: refuse{}},
	}
	if err := c.Send(context.Background(), message()); err == nil {
		t.Fatal("an unreachable transport reported success")
	}
}

type refuse struct{}

func (refuse) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, io.ErrUnexpectedEOF
}

// The development transport. It reports success so a deployment without a
// mailbox still serves, and it must never write the body — the body is where
// the single-use link is, and a development log is still a log.
func TestTheLogTransportNeverWritesTheLink(t *testing.T) {
	var out strings.Builder
	logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))

	if err := (Log{Logger: logger}).Send(context.Background(), message()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	line := out.String()
	if !strings.Contains(line, "tenant@example.com") {
		t.Errorf("the log does not say who it was for:\n%s", line)
	}
	if strings.Contains(line, "a-single-use-token") || strings.Contains(line, verification) {
		t.Errorf("the log carries the recovery link:\n%s", line)
	}
}
