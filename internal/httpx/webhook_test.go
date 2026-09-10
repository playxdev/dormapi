package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/playxdev/dormapi/internal/d1/d1test"
	"github.com/playxdev/dormapi/internal/line"
)

const channelSecret = "a-messaging-channel-secret"

// recordingReplier keeps what the Official Account would have said.
type recordingReplier struct {
	sent []line.Message
	err  error
}

func (r *recordingReplier) Reply(_ context.Context, _ string, m line.Message) error {
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, m)
	return nil
}

func newWebhookAPI(t *testing.T, fake *d1test.Server) (*API, *recordingReplier) {
	t.Helper()
	replier := &recordingReplier{}
	api := newAPI(t, fake, &stubVerifier{}, &recordingMail{})
	api.LineChannelSecret = channelSecret
	api.Messenger = replier
	return api, replier
}

// deliver posts a body the way LINE would, signature included.
func deliver(t *testing.T, api *API, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/line", strings.NewReader(body))
	req.Header.Set("X-Line-Signature", line.Sign(channelSecret, []byte(body)))
	rec := httptest.NewRecorder()
	api.Routes([]string{"https://example.test"}).ServeHTTP(rec, req)
	return rec
}

func textEvent(eventID, userID, text string) string {
	e := map[string]any{
		"type": "message", "mode": "active", "webhookEventId": eventID,
		"replyToken": "a-reply-token", "timestamp": 1789000000,
		"source":  map[string]any{"type": "user", "userId": userID},
		"message": map[string]any{"type": "text", "id": "1", "text": text},
	}
	body, _ := json.Marshal(map[string]any{"destination": "Uoa", "events": []any{e}})
	return string(body)
}

// Not from LINE. Nothing is read out of the body and nothing is answered.
func TestWebhookRefusesAForgedDelivery(t *testing.T) {
	fake := d1test.New(t)
	api, replier := newWebhookAPI(t, fake)

	body := textEvent("01EVENT", "U_resident", "ADMIN")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/line", strings.NewReader(body))
	req.Header.Set("X-Line-Signature", line.Sign("the-wrong-secret", []byte(body)))
	rec := httptest.NewRecorder()
	api.Routes([]string{"https://example.test"}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("a forged delivery reached the database: %#v", fake.Calls)
	}
	if len(replier.sent) != 0 {
		t.Errorf("a forged delivery was answered: %#v", replier.sent)
	}
}

// An endpoint that cannot verify a signature must not exist. Without a secret
// the route is never registered, so it 404s rather than accepting anything.
func TestWebhookIsNotRegisteredWithoutASecret(t *testing.T) {
	fake := d1test.New(t)
	api, _ := newWebhookAPI(t, fake)
	api.LineChannelSecret = ""

	body := textEvent("01EVENT", "U_resident", "ADMIN")
	req := httptest.NewRequest(http.MethodPost, "/webhooks/line", strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.Routes([]string{"https://example.test"}).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// LINE redelivers on a timeout, on a non-2xx, and sometimes for no visible
// reason. The second delivery of one event must do nothing at all.
func TestWebhookHandlesAnEventOnlyOnce(t *testing.T) {
	fake := d1test.New(t,
		// first delivery: the event is new, then the routing reads
		d1test.Answer{Changes: 1},
		d1test.Answer{Rows: []map[string]any{{
			"account_id": "01ACCOUNT", "tenant_id": "01TENANT", "tenant_name": "หอพักทดสอบ",
		}}},
		d1test.Answer{Rows: []map[string]any{{
			"tenant_id": "01TENANT", "membership_id": "01MEMBER", "operator_name": "หอพักทดสอบ",
			"party_id": "01PARTY", "account_id": "01ACCOUNT", "display_name": "ผู้เช่า",
			"contract_id": "01CONTRACT", "building_id": "01BUILDING", "building_name": "Oscar",
			"room_id": "01ROOM", "room_number": "609",
		}}},
		d1test.Answer{Rows: []map[string]any{{"building_name": "Oscar", "address": "123 ถนนทดสอบ"}}},
		// second delivery: the insert is ignored, so nothing follows
		d1test.Answer{Changes: 0},
	)
	api, replier := newWebhookAPI(t, fake)

	body := textEvent("01EVENT", "U_resident", "ADMIN")
	if rec := deliver(t, api, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	first := len(fake.Calls)
	if len(replier.sent) != 1 {
		t.Fatalf("answered %d times, want 1", len(replier.sent))
	}
	requireSQL(t, fake.Calls[0].SQL, "INSERT OR IGNORE INTO webhook_event_seen")

	if rec := deliver(t, api, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(fake.Calls) != first+1 {
		t.Errorf("the redelivery ran %d more statements, want only the idempotency check",
			len(fake.Calls)-first)
	}
	if len(replier.sent) != 1 {
		t.Errorf("answered %d times, want the redelivery to say nothing", len(replier.sent))
	}
}

// INV-32. A person renting from two operators who types into the chat must be
// asked which one they mean — answering for either would put one operator's
// business in a conversation about the other's.
func TestWebhookAsksWhichOperatorRatherThanGuessing(t *testing.T) {
	fake := d1test.New(t,
		d1test.Answer{Changes: 1},
		d1test.Answer{Rows: []map[string]any{
			{"account_id": "01ACCOUNT", "tenant_id": "01TENANT", "tenant_name": "หอพักทดสอบ"},
			{"account_id": "01ACCOUNT", "tenant_id": "01OTHER", "tenant_name": "หออื่น"},
		}},
		// no unexpired conversation context
		d1test.Answer{},
	)
	api, replier := newWebhookAPI(t, fake)

	deliver(t, api, textEvent("01EVENT", "U_resident", "ADMIN"))

	if len(replier.sent) != 1 {
		t.Fatalf("answered %d times, want 1", len(replier.sent))
	}
	msg := replier.sent[0]
	if len(msg.Choices) != 2 {
		t.Fatalf("offered %d choices, want one per operator", len(msg.Choices))
	}
	for _, c := range msg.Choices {
		if !strings.HasPrefix(c.Data, selectPrefix) {
			t.Errorf("choice %+v carries no tenant to check", c)
		}
	}
	// Neither operator's contact details were given out.
	if strings.Contains(msg.Text, "ถนน") {
		t.Errorf("an operator was answered for anyway: %q", msg.Text)
	}
}

// With one membership there is nothing to ask, and ADMIN — the rich menu's
// button, which has been going unanswered — gets the office details.
func TestWebhookAnswersAdminForASingleOperator(t *testing.T) {
	fake := d1test.New(t,
		d1test.Answer{Changes: 1},
		d1test.Answer{Rows: []map[string]any{{
			"account_id": "01ACCOUNT", "tenant_id": "01TENANT", "tenant_name": "หอพักทดสอบ",
		}}},
		d1test.Answer{Rows: []map[string]any{{
			"tenant_id": "01TENANT", "membership_id": "01MEMBER", "operator_name": "หอพักทดสอบ",
			"party_id": "01PARTY", "account_id": "01ACCOUNT", "display_name": "ผู้เช่า",
			"contract_id": "01CONTRACT", "building_id": "01BUILDING", "building_name": "Oscar",
			"room_id": "01ROOM", "room_number": "609",
		}}},
		d1test.Answer{Rows: []map[string]any{{"building_name": "Oscar", "address": "123 ถนนทดสอบ"}}},
	)
	api, replier := newWebhookAPI(t, fake)

	deliver(t, api, textEvent("01EVENT", "U_resident", "admin"))

	if len(replier.sent) != 1 {
		t.Fatalf("answered %d times, want 1", len(replier.sent))
	}
	text := replier.sent[0].Text
	for _, want := range []string{"หอพักทดสอบ", "609", "123 ถนนทดสอบ"} {
		if !strings.Contains(text, want) {
			t.Errorf("the answer omits %q:\n%s", want, text)
		}
	}
}

// Messaged the Official Account before ever opening the app. There is no
// account to resolve, and the answer says what to do rather than nothing.
func TestWebhookAnswersSomeoneWithNoAccount(t *testing.T) {
	fake := d1test.New(t,
		d1test.Answer{Changes: 1},
		d1test.Answer{}, // no identity
	)
	api, replier := newWebhookAPI(t, fake)

	deliver(t, api, textEvent("01EVENT", "U_stranger", "สวัสดี"))

	if len(replier.sent) != 1 || !strings.Contains(replier.sent[0].Text, "เข้าสู่ระบบ") {
		t.Errorf("answer = %+v", replier.sent)
	}
}

// INV-03. The tenant a quick reply names arrives from the client, so it is
// checked against the sender's own memberships before anything is written.
func TestWebhookChecksTheTenantAQuickReplyNames(t *testing.T) {
	fake := d1test.New(t,
		d1test.Answer{Changes: 1},
		d1test.Answer{Rows: []map[string]any{{
			"account_id": "01ACCOUNT", "tenant_id": "01TENANT", "tenant_name": "หอพักทดสอบ",
		}}},
		// ResolveTenancyIn finds nothing: not a member of the named tenant.
		d1test.Answer{},
	)
	api, replier := newWebhookAPI(t, fake)

	e := map[string]any{
		"type": "postback", "mode": "active", "webhookEventId": "01EVENT",
		"replyToken": "a-reply-token",
		"source":     map[string]any{"type": "user", "userId": "U_resident"},
		"postback":   map[string]any{"data": selectPrefix + "01SOMEONEELSES"},
	}
	body, _ := json.Marshal(map[string]any{"destination": "Uoa", "events": []any{e}})
	deliver(t, api, string(body))

	// The resolve was asked to check the claimed tenant against membership.
	resolve := fake.Calls[2]
	requireSQL(t, resolve.SQL, "FROM membership m", "AND m.tenant_id = ?2")
	if resolve.Params[1] != "01SOMEONEELSES" {
		t.Errorf("the claim was not the thing checked: %#v", resolve.Params)
	}

	// Nothing was written, and the answer gives away nothing about whether
	// that tenant exists.
	for _, c := range fake.Calls {
		if strings.Contains(c.SQL, "conversation_context") && strings.Contains(c.SQL, "INSERT") {
			t.Error("a forged choice was remembered")
		}
	}
	if len(replier.sent) != 1 || !strings.Contains(replier.sent[0].Text, "ยังไม่ได้ผูกกับห้องพัก") {
		t.Errorf("answer = %+v", replier.sent)
	}
}

// A group chat is not where a tenancy is discussed, and an answer there would
// show one resident's business to everyone in the group.
func TestWebhookIgnoresGroupsAndStandbyEvents(t *testing.T) {
	for _, name := range []string{"group", "standby"} {
		t.Run(name, func(t *testing.T) {
			e := map[string]any{
				"type": "message", "mode": "active", "webhookEventId": "01EVENT",
				"replyToken": "a-reply-token",
				"source":     map[string]any{"type": "user", "userId": "U_resident"},
				"message":    map[string]any{"type": "text", "id": "1", "text": "ADMIN"},
			}
			if name == "group" {
				e["source"] = map[string]any{"type": "group", "userId": "U_resident"}
			} else {
				e["mode"] = "standby"
			}
			body, _ := json.Marshal(map[string]any{"destination": "Uoa", "events": []any{e}})

			fake := d1test.New(t)
			api, replier := newWebhookAPI(t, fake)
			if rec := deliver(t, api, string(body)); rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if len(fake.Calls) != 0 || len(replier.sent) != 0 {
				t.Errorf("acted on a %s event: %#v %#v", name, fake.Calls, replier.sent)
			}
		})
	}
}

// Saving the endpoint in the LINE console sends a signed delivery with no
// events. It has to answer 200 or the console refuses to enable the webhook.
func TestWebhookAcceptsTheConsolesVerification(t *testing.T) {
	fake := d1test.New(t)
	api, _ := newWebhookAPI(t, fake)
	if rec := deliver(t, api, `{"destination":"Uoa","events":[]}`); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// requireSQL fails unless every fragment appears in the statement.
func requireSQL(t *testing.T, sql string, fragments ...string) {
	t.Helper()
	normalised := strings.Join(strings.Fields(sql), " ")
	for _, want := range fragments {
		if !strings.Contains(normalised, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("statement is missing %q\ngot: %s", want, normalised)
		}
	}
}
