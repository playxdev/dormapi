package httpx

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/playxdev/dormapi/internal/auth"
	"github.com/playxdev/dormapi/internal/d1/d1test"
	"github.com/playxdev/dormapi/internal/line"
	appmail "github.com/playxdev/dormapi/internal/mail"
	"github.com/playxdev/dormapi/internal/store"
)

// stubVerifier answers as LINE would, without asking it.
type stubVerifier struct {
	identity *line.Identity
	err      error
	calls    int
}

func (s *stubVerifier) Verify(context.Context, string) (*line.Identity, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.identity, nil
}

// recordingMail keeps what would have been sent.
type recordingMail struct {
	sent []appmail.Message
	err  error
}

func (m *recordingMail) Send(_ context.Context, msg appmail.Message) error {
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, msg)
	return nil
}

func newAPI(t *testing.T, fake *d1test.Server, verifier IdentityVerifier, mailer appmail.Sender) *API {
	t.Helper()
	issuer := auth.NewIssuer([]byte("test-secret-at-least-32-bytes-long!!"), 0)
	return &API{
		Store:      store.New(fake.Client(), "https://backoffice.example"),
		Verifier:   verifier,
		Issuer:     issuer,
		Mail:       mailer,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		APIBaseURL: "https://api.example",
		AppLIFFURL: "https://liff.line.me/2011358311-IAdUIyFx",
	}
}

func postJSON(h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.9:5000"
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// The three answers /recovery/request can give must be one answer. Anything
// that varies with whether the address is known turns this endpoint into a way
// to ask "does this person rent here?", which the system must not answer to a
// stranger who has typed an address.
func TestRequestRecoveryAnswersTheSameForEveryAddress(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		answers []d1test.Answer
		mails   int
	}{
		{
			name:    "a verified address",
			body:    `{"email":"tenant@example.com"}`,
			answers: []d1test.Answer{{Rows: []map[string]any{{"id": "user-1"}}}, {Changes: 1}},
			mails:   1,
		},
		{
			name:    "an address nobody has verified",
			body:    `{"email":"stranger@example.com"}`,
			answers: []d1test.Answer{{}},
			mails:   0,
		},
		{
			name:    "not an address at all",
			body:    `{"email":"nonsense"}`,
			answers: nil,
			mails:   0,
		},
	}

	var bodies []string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := d1test.New(t, c.answers...)
			mailer := &recordingMail{}
			api := newAPI(t, fake, &stubVerifier{}, mailer)

			rec := postJSON(api.requestRecovery, "/recovery/request", c.body)
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			bodies = append(bodies, rec.Body.String())

			if len(mailer.sent) != c.mails {
				t.Fatalf("sent %d mails, want %d", len(mailer.sent), c.mails)
			}
			if c.mails == 1 {
				msg := mailer.sent[0]
				if !strings.Contains(msg.Text, "?recovery=") {
					t.Error("the mail carries no recovery link")
				}
				// The link opens the app, not this service: the rebind needs a
				// LINE ID token, and only the app can produce one.
				if !strings.Contains(msg.Text, "liff.line.me") {
					t.Error("the recovery link does not open the app")
				}
				// Only the hash is written down, so a leaked database is a
				// list of hashes rather than a set of working links.
				raw := tokenFromLink(t, msg.Text, "?recovery=")
				issued := fake.Calls[1]
				for i, p := range issued.Params {
					if p == raw {
						t.Errorf("param %d is the raw token from the mail", i+1)
					}
				}
				if issued.Params[3] != hashToken(raw) {
					t.Errorf("token_hash = %#v, want the SHA-256 of the mailed token", issued.Params[3])
				}
			}
		})
	}

	for i := 1; i < len(bodies); i++ {
		if bodies[i] != bodies[0] {
			t.Errorf("answers differ by address:\n%s\n%s", bodies[0], bodies[i])
		}
	}
}

func TestRequestRecoverySaysNothingWhenTheMailFails(t *testing.T) {
	// A failure to send is this service's problem, and reporting it would
	// report that the address is known. Logged, never told.
	fake := d1test.New(t, d1test.Answer{Rows: []map[string]any{{"id": "user-1"}}}, d1test.Answer{Changes: 1})
	api := newAPI(t, fake, &stubVerifier{}, &recordingMail{err: io.ErrUnexpectedEOF})

	rec := postJSON(api.requestRecovery, "/recovery/request", `{"email":"tenant@example.com"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "sent_if_known") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestRebindRequiresBothProofs(t *testing.T) {
	for _, c := range []struct {
		name string
		body string
	}{
		{"no recovery token", `{"id_token":"an-id-token"}`},
		{"no id token", `{"recovery_token":"a-token"}`},
		{"neither", `{}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := d1test.New(t)
			api := newAPI(t, fake, &stubVerifier{identity: &line.Identity{UserID: "U_new"}}, &recordingMail{})

			rec := postJSON(api.rebindRecovery, "/recovery/rebind", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if len(fake.Calls) != 0 {
				t.Errorf("touched the database with only half the proof: %#v", fake.Calls)
			}
		})
	}
}

func TestRebindDoesNotSpendTheTokenOnABadIDToken(t *testing.T) {
	// Order matters here. Consuming first would let anyone burn a tenant's
	// recovery link by replaying it with a token LINE rejects — and the tenant
	// would have to ask for another without ever knowing why.
	fake := d1test.New(t)
	api := newAPI(t, fake, &stubVerifier{err: line.ErrInvalidToken}, &recordingMail{})

	rec := postJSON(api.rebindRecovery, "/recovery/rebind",
		`{"recovery_token":"a-token","id_token":"forged"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if len(fake.Calls) != 0 {
		t.Errorf("the recovery token was spent for a rejected login: %#v", fake.Calls)
	}
}

func TestRebindRejectsASpentToken(t *testing.T) {
	// The guarded UPDATE matches nothing, which is what a link used twice, an
	// expired one and a forged one all look like.
	fake := d1test.New(t, d1test.Answer{})
	api := newAPI(t, fake, &stubVerifier{identity: &line.Identity{UserID: "U_new"}}, &recordingMail{})

	rec := postJSON(api.rebindRecovery, "/recovery/rebind",
		`{"recovery_token":"already-used","id_token":"an-id-token"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if len(fake.Calls) != 1 {
		t.Errorf("ran %d statements, want the rebind to stop at the spent token", len(fake.Calls))
	}
}

func TestRebindMovesTheIdentityAndIssuesASession(t *testing.T) {
	fake := d1test.New(t,
		// consume the recovery token
		d1test.Answer{Rows: []map[string]any{{"user_id": "user-1"}}},
		// read the LINE subject the account has now
		d1test.Answer{Rows: []map[string]any{{"subject": "U_old"}}},
		// audit, then move
		d1test.Answer{Changes: 1},
		d1test.Answer{Changes: 1},
	)
	api := newAPI(t, fake, &stubVerifier{identity: &line.Identity{UserID: "U_new"}}, &recordingMail{})

	rec := postJSON(api.rebindRecovery, "/recovery/rebind",
		`{"recovery_token":"a-token","id_token":"an-id-token"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Token == "" || body.ExpiresAt == "" {
		t.Fatalf("no session issued: %s", rec.Body.String())
	}

	if len(fake.Calls) != 4 {
		t.Fatalf("ran %d statements, want 4: %#v", len(fake.Calls), fake.Calls)
	}
	if !strings.Contains(fake.Calls[2].SQL, "identity_audit_logs") {
		t.Error("the rebind was not audited before the identity moved")
	}
	if !strings.Contains(fake.Calls[3].SQL, "UPDATE identities") {
		t.Error("the identity was never moved")
	}
}

func TestRebindOnTheSameAccountChangesNothing(t *testing.T) {
	// A tenant who opens their own recovery link on the account they still
	// have. Nothing to move, and moving it would write an audit row saying an
	// account changed hands when it did not.
	fake := d1test.New(t,
		d1test.Answer{Rows: []map[string]any{{"user_id": "user-1"}}},
		d1test.Answer{Rows: []map[string]any{{"subject": "U_same"}}},
	)
	api := newAPI(t, fake, &stubVerifier{identity: &line.Identity{UserID: "U_same"}}, &recordingMail{})

	rec := postJSON(api.rebindRecovery, "/recovery/rebind",
		`{"recovery_token":"a-token","id_token":"an-id-token"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(fake.Calls) != 2 {
		t.Errorf("ran %d statements, want no audit and no move: %#v", len(fake.Calls), fake.Calls)
	}
}

// tokenFromLink pulls the token out of a link in a mail body.
func tokenFromLink(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no %q in the mail", marker)
	}
	rest := body[i+len(marker):]
	if end := strings.IndexAny(rest, "\n\"< &"); end >= 0 {
		rest = rest[:end]
	}
	if rest == "" {
		t.Fatal("the link carries an empty token")
	}
	return rest
}
