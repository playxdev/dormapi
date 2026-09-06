package httpx

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"

	appmail "github.com/playxdev/dormapi/internal/mail"
	"github.com/playxdev/dormapi/internal/store"
)

// How long each kind of link stays usable.
//
// Verification is generous because it competes with nothing: the tenant may
// finish onboarding and open their inbox that evening. Recovery is short
// because it is the one link that hands an account to whoever holds it.
const (
	verifyTokenTTL   = 24 * time.Hour
	recoveryTokenTTL = 15 * time.Minute
)

// newToken returns a link token and the hash to store for it.
//
// 32 bytes from crypto/rand, URL-safe, and only the hash is ever written down,
// so a leaked database is a list of hashes rather than a set of live links.
func newToken() (raw, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("httpx: generate token: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(raw))
	return raw, hex.EncodeToString(sum[:]), nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func validEmail(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 254 {
		return "", false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return "", false
	}
	return s, true
}

type emailRequest struct {
	Email string `json:"email"`
}

// setEmail attaches an address to the caller's account and sends a
// verification link.
//
// The address is what makes recovery possible, and nothing else in the system
// does. A tenant without one is locked out permanently the day they lose their
// LINE account, so this is offered after the terms rather than before them —
// it must never stand between a tenant and the room they are trying to reach.
func (a *API) setEmail(w http.ResponseWriter, r *http.Request) {
	var req emailRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	address, ok := validEmail(req.Email)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_email")
		return
	}

	userID := userIDFrom(r.Context())
	if err := a.Store.SetEmail(r.Context(), userID, address); err != nil {
		if errors.Is(err, store.ErrEmailTaken) {
			writeError(w, http.StatusConflict, "email_taken")
			return
		}
		a.Log.ErrorContext(r.Context(), "set email failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	if err := a.sendVerification(r.Context(), userID, address); err != nil {
		// The address is saved either way. Failing the request here would
		// leave the tenant unable to tell that, and retrying is harmless.
		a.Log.ErrorContext(r.Context(), "send verification failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusBadGateway, "mail_unavailable")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "verification_sent"})
}

func (a *API) sendVerification(ctx context.Context, userID, address string) error {
	raw, hash, err := newToken()
	if err != nil {
		return err
	}
	if err := a.Store.IssueAuthToken(ctx, "email_verify", userID, hash, address,
		time.Now().Add(verifyTokenTTL), ""); err != nil {
		return err
	}

	link := a.APIBaseURL + "/api/v1/email/verify?token=" + raw
	return a.Mail.Send(ctx, appmail.Message{
		To:      address,
		Subject: "ยืนยันอีเมลของคุณ · dorm.place",
		Text: "ยืนยันอีเมลนี้เพื่อใช้กู้คืนบัญชี dorm.place ของคุณ\n\n" + link +
			"\n\nลิงก์นี้ใช้ได้ 24 ชั่วโมง หากคุณไม่ได้ขอ ไม่ต้องดำเนินการใด ๆ",
		HTML: `<p>ยืนยันอีเมลนี้เพื่อใช้กู้คืนบัญชี dorm.place ของคุณ</p>` +
			`<p><a href="` + link + `">ยืนยันอีเมล</a></p>` +
			`<p>ลิงก์นี้ใช้ได้ 24 ชั่วโมง หากคุณไม่ได้ขอ ไม่ต้องดำเนินการใด ๆ</p>`,
	})
}

// verifyEmail is opened from the inbox, so it answers with a page rather than
// JSON. Nobody reads this URL with a client.
func (a *API) verifyEmail(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		writePage(w, http.StatusBadRequest, "ลิงก์ไม่ถูกต้อง", "กรุณาขอลิงก์ยืนยันใหม่จากแอป")
		return
	}

	userID, err := a.Store.ConsumeAuthToken(r.Context(), "email_verify", hashToken(token))
	if err != nil {
		// Expired, already used, or never existed. All three mean the same
		// thing to the person holding it.
		writePage(w, http.StatusBadRequest, "ลิงก์หมดอายุ", "กรุณาขอลิงก์ยืนยันใหม่จากแอป")
		return
	}
	if err := a.Store.MarkEmailVerified(r.Context(), userID); err != nil {
		a.Log.ErrorContext(r.Context(), "mark verified failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writePage(w, http.StatusInternalServerError, "เกิดข้อผิดพลาด", "กรุณาลองใหม่อีกครั้ง")
		return
	}
	writePage(w, http.StatusOK, "ยืนยันอีเมลเรียบร้อย",
		"ถ้าคุณเปลี่ยนบัญชี LINE ในอนาคต ใช้อีเมลนี้กู้คืนห้องพักของคุณได้")
}

// requestRecovery starts the flow that hands an account to a new LINE login.
//
// It answers 200 whether or not the address is known, with the same body. Any
// difference — a status code, a message, a response time worth measuring —
// turns this endpoint into a way to ask "does this person rent here?", which is
// a question the system must not answer to a stranger.
func (a *API) requestRecovery(w http.ResponseWriter, r *http.Request) {
	var req emailRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	accepted := map[string]string{"status": "sent_if_known"}

	address, ok := validEmail(req.Email)
	if !ok {
		writeJSON(w, http.StatusOK, accepted)
		return
	}

	userID, err := a.Store.UserByVerifiedEmail(r.Context(), address)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.Log.ErrorContext(r.Context(), "recovery lookup failed",
				"request_id", RequestIDFrom(r.Context()), "error", err)
		}
		writeJSON(w, http.StatusOK, accepted)
		return
	}

	raw, hash, err := newToken()
	if err == nil {
		err = a.Store.IssueAuthToken(r.Context(), "recovery", userID, hash, address,
			time.Now().Add(recoveryTokenTTL), "")
	}
	if err == nil {
		link := a.AppLIFFURL + "?recovery=" + raw
		err = a.Mail.Send(r.Context(), appmail.Message{
			To:      address,
			Subject: "กู้คืนบัญชี dorm.place",
			Text: "เปิดลิงก์นี้บนมือถือที่ติดตั้ง LINE บัญชีใหม่ของคุณ\n\n" + link +
				"\n\nลิงก์ใช้ได้ 15 นาที และใช้ได้ครั้งเดียว หากคุณไม่ได้ขอ ไม่ต้องดำเนินการใด ๆ",
			HTML: `<p>เปิดลิงก์นี้บนมือถือที่ติดตั้ง LINE บัญชีใหม่ของคุณ</p>` +
				`<p><a href="` + link + `">กู้คืนบัญชี</a></p>` +
				`<p>ลิงก์ใช้ได้ 15 นาที และใช้ได้ครั้งเดียว หากคุณไม่ได้ขอ ไม่ต้องดำเนินการใด ๆ</p>`,
		})
	}
	if err != nil {
		// Logged, never told: the caller learns nothing about this address.
		a.Log.ErrorContext(r.Context(), "recovery send failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
	}
	writeJSON(w, http.StatusOK, accepted)
}

type rebindRequest struct {
	RecoveryToken string `json:"recovery_token"`
	IDToken       string `json:"id_token"`
}

// rebindRecovery points an existing account at the LINE account presenting the
// token.
//
// Two proofs are required and neither is sufficient alone: the recovery link,
// which says the holder reads the verified address, and a LINE ID token, which
// says the new account is real and is here now.
func (a *API) rebindRecovery(w http.ResponseWriter, r *http.Request) {
	var req rebindRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if req.RecoveryToken == "" || req.IDToken == "" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	identity, err := a.Verifier.Verify(r.Context(), req.IDToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid_id_token")
		return
	}

	// The token is spent before anything else changes. A rebind that fails
	// afterwards costs the tenant one more email; a token that survives a
	// failed rebind is a live link to someone else's account.
	userID, err := a.Store.ConsumeAuthToken(r.Context(), "recovery", hashToken(req.RecoveryToken))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "recovery_token_invalid")
		return
	}

	old, err := a.Store.LineSubjectFor(r.Context(), userID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		a.Log.ErrorContext(r.Context(), "read old subject failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if old == identity.UserID {
		// Already this account. Nothing to move; issue a session and stop.
		a.issueSession(w, r, userID)
		return
	}

	if err := a.Store.RebindLine(r.Context(), userID, old, identity.UserID,
		"line_rebind", "", RequestIDFrom(r.Context())); err != nil {
		a.Log.ErrorContext(r.Context(), "rebind failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	a.issueSession(w, r, userID)
}

func (a *API) issueSession(w http.ResponseWriter, r *http.Request, userID string) {
	token, expires, err := a.Issuer.Issue(userID)
	if err != nil {
		a.Log.ErrorContext(r.Context(), "issue token failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, authLineResponse{
		Token:     token,
		ExpiresAt: expires.UTC().Format(time.RFC3339),
	})
}

// writePage answers a browser that arrived from an inbox. Deliberately plain:
// no stylesheet to fetch, nothing to track, and readable with images off.
func writePage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html lang="th"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow"><title>%s · dorm.place</title></head>
<body style="font-family:system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#f3f7f4;color:#101914">
<main style="text-align:center;padding:2rem;max-width:22rem">
<h1 style="font-size:1.25rem;color:#009245">%s</h1><p style="line-height:1.7">%s</p>
</main></body></html>`, title, title, detail)
}
