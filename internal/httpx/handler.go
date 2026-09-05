package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/playxdev/dormapi/internal/auth"
	"github.com/playxdev/dormapi/internal/line"
	"github.com/playxdev/dormapi/internal/store"
)

type API struct {
	Store    *store.Store
	Verifier *line.Verifier
	Issuer   *auth.Issuer
	Log      *slog.Logger
}

func (a *API) Routes(allowedOrigins []string) http.Handler {
	r := chi.NewRouter()

	r.Use(RequestID)
	r.Use(Recover(a.Log))
	r.Use(Logger(a.Log))
	r.Use(CORS(allowedOrigins))

	r.Get("/healthz", a.health)

	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/auth/line", a.authLine)

		r.Group(func(r chi.Router) {
			r.Use(a.requireSession)
			r.Get("/me", a.me)
			r.Get("/me/invoices", a.listInvoices)
			r.Get("/me/invoices/{invoiceID}", a.getInvoice)
			r.Get("/me/invoices/{invoiceID}/payment", a.getPaymentInfo)
			r.Post("/me/invoices/{invoiceID}/payments", a.reportPayment)
			r.Get("/me/repairs", a.listRepairs)
			r.Post("/me/repairs", a.createRepair)
			r.Get("/me/repairs/{repairID}", a.getRepair)
			r.Get("/me/announcements", a.listAnnouncements)
			r.Get("/me/announcements/{announcementID}", a.getAnnouncement)
			r.Post("/me/announcements/{announcementID}/read", a.readAnnouncement)
			r.Get("/me/meters", a.listMeters)
			r.Get("/invites/{code}", a.getInvite)
			r.Post("/invites/{code}/claim", a.claimInvite)
		})
	})

	return r
}

// health reports whether this instance can serve, which means reaching D1.
//
// Answering "ok" without checking is worse than having no health check: the
// service ran for days on a database ID that had been deleted, reporting
// healthy the whole time, because the only D1 check was at startup.
//
// The query is `SELECT 1`, so this is cheap enough for a platform to poll.
func (a *API) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := a.Store.Ping(ctx); err != nil {
		a.Log.Error("health: database unreachable", "error", err)
		// The error text can name the account and database; the status code
		// and the log carry everything the caller is entitled to.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "degraded",
			"detail": "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type authLineRequest struct {
	IDToken string `json:"id_token"`
}

type authLineResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// authLine exchanges a LINE ID token for a session token, creating the user on
// first sign-in.
//
// It deliberately does not report whether the user is new, nor whether they
// have a tenancy. Resolving the tenancy is GET /me's job, which keeps the two
// concerns — who you are, and what you may see — separate.
func (a *API) authLine(w http.ResponseWriter, r *http.Request) {
	var req authLineRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	identity, err := a.Verifier.Verify(r.Context(), req.IDToken)
	if err != nil {
		if errors.Is(err, line.ErrInvalidToken) {
			writeError(w, http.StatusUnauthorized, "invalid_id_token")
			return
		}
		// LINE was unreachable. That is our outage, not a rejected user.
		a.Log.ErrorContext(r.Context(), "line verification failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusBadGateway, "identity_provider_unavailable")
		return
	}

	user, err := a.Store.UserByLineSubject(r.Context(), identity.UserID, identity.DisplayName)
	if err != nil {
		a.Log.ErrorContext(r.Context(), "upsert user failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	token, expires, err := a.Issuer.Issue(user.ID)
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

type meResponse struct {
	UserID       string `json:"user_id"`
	TenantID     string `json:"tenant_id"`
	ContractID   string `json:"contract_id"`
	PropertyID   string `json:"property_id"`
	PropertyName string `json:"property_name"`
	RoomID       string `json:"room_id"`
	DisplayName  string `json:"display_name"`
}

// me returns the caller's authorised context.
//
// Every field is derived from the session, never from the request. A client
// that sends its own property_id or room_id is ignored.
func (a *API) me(w http.ResponseWriter, r *http.Request) {
	userID := userIDFrom(r.Context())

	c, err := a.Store.ContextForUser(r.Context(), userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Authenticated, but not linked to a room yet. The MINI App shows
			// "your account is not linked to a dormitory".
			writeError(w, http.StatusNotFound, "tenancy_not_found")
			return
		}
		a.Log.ErrorContext(r.Context(), "resolve context failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	// property_id and room_id keep their names on the wire even though the
	// schema calls them buildings and room numbers: the MINI App and the design
	// document both speak of properties and rooms, and renaming the contract
	// would break a deployed client for no gain.
	writeJSON(w, http.StatusOK, meResponse{
		UserID:       c.User.ID,
		TenantID:     c.TenantID,
		ContractID:   c.ContractID,
		PropertyID:   c.BuildingID,
		PropertyName: c.BuildingName,
		RoomID:       c.RoomNumber,
		DisplayName:  c.User.Name,
	})
}

func (a *API) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		raw, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || raw == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		userID, err := a.Issuer.Verify(raw)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeyUserID, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError sends a stable machine-readable code. It never carries an
// internal message: the frontend maps codes to Thai copy of its own, and a
// leaked SQL or LINE error would only reach the user as noise.
func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

// listInvoices returns every invoice for the caller's tenancy.
//
// Amounts are integer satang throughout the API. The client formats them; the
// server never sends a pre-formatted currency string, so a display change does
// not need a deploy here.
func (a *API) listInvoices(w http.ResponseWriter, r *http.Request) {
	invoices, err := a.Store.InvoicesForUser(r.Context(), userIDFrom(r.Context()))
	if err != nil {
		a.Log.ErrorContext(r.Context(), "list invoices failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	var outstanding int64
	for _, inv := range invoices {
		if inv.DueSatang > 0 {
			outstanding += inv.DueSatang
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"invoices":           invoices,
		"outstanding_satang": outstanding,
	})
}

func (a *API) getInvoice(w http.ResponseWriter, r *http.Request) {
	invoice, err := a.Store.InvoiceForUser(r.Context(),
		userIDFrom(r.Context()), chi.URLParam(r, "invoiceID"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "invoice_not_found")
			return
		}
		a.Log.ErrorContext(r.Context(), "get invoice failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, invoice)
}

func (a *API) listRepairs(w http.ResponseWriter, r *http.Request) {
	tickets, err := a.Store.TicketsForUser(r.Context(), userIDFrom(r.Context()))
	if err != nil {
		a.Log.ErrorContext(r.Context(), "list repairs failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repairs": tickets})
}

func (a *API) getRepair(w http.ResponseWriter, r *http.Request) {
	ticket, err := a.Store.TicketForUser(r.Context(),
		userIDFrom(r.Context()), chi.URLParam(r, "repairID"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "repair_not_found")
			return
		}
		a.Log.ErrorContext(r.Context(), "get repair failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, ticket)
}

// listMeters returns the water and electricity readings taken during the
// caller's own tenancy.
func (a *API) listMeters(w http.ResponseWriter, r *http.Request) {
	meters, err := a.Store.MetersForUser(r.Context(), userIDFrom(r.Context()))
	if err != nil {
		a.Log.ErrorContext(r.Context(), "list meters failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"meters": meters})
}

// listAnnouncements returns the notice board of every building the caller
// rents in, pinned notices first.
//
// unread_count is computed here rather than asked of the database: the list is
// a handful of rows and already in hand, and a second D1 round trip to count
// them would cost more than the loop.
func (a *API) listAnnouncements(w http.ResponseWriter, r *http.Request) {
	announcements, err := a.Store.AnnouncementsForUser(r.Context(), userIDFrom(r.Context()))
	if err != nil {
		a.Log.ErrorContext(r.Context(), "list announcements failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	unread := 0
	for _, n := range announcements {
		if !n.Read {
			unread++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"announcements": announcements,
		"unread_count":  unread,
	})
}

func (a *API) getAnnouncement(w http.ResponseWriter, r *http.Request) {
	announcement, err := a.Store.AnnouncementForUser(r.Context(),
		userIDFrom(r.Context()), chi.URLParam(r, "announcementID"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "announcement_not_found")
			return
		}
		a.Log.ErrorContext(r.Context(), "get announcement failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, announcement)
}

// readAnnouncement marks one notice as opened by the caller.
//
// Repeating it is deliberately not an error: the app marks on every view, and
// only the first one writes a row.
func (a *API) readAnnouncement(w http.ResponseWriter, r *http.Request) {
	err := a.Store.MarkAnnouncementRead(r.Context(),
		userIDFrom(r.Context()), chi.URLParam(r, "announcementID"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": "read"})
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "announcement_not_found")
	default:
		a.Log.ErrorContext(r.Context(), "mark announcement read failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
	}
}

type createRepairRequest struct {
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Priority string `json:"priority"`
}

// createRepair files a request against the caller's own tenancy.
//
// The body carries what is being reported, never where: property, room and
// tenancy are read from the database using the session's user ID.
func (a *API) createRepair(w http.ResponseWriter, r *http.Request) {
	var req createRepairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	ticket, err := a.Store.CreateTicket(r.Context(),
		userIDFrom(r.Context()), req.Title, req.Detail, req.Priority)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalid):
			writeError(w, http.StatusBadRequest, "invalid_request")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "tenancy_not_found")
		default:
			a.Log.ErrorContext(r.Context(), "create repair failed",
				"request_id", RequestIDFrom(r.Context()), "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error")
		}
		return
	}

	writeJSON(w, http.StatusCreated, ticket)
}

// getInvite returns the terms a tenant is about to accept.
//
// A session is required even though the invite is not yet theirs: knowing who
// is looking is what makes "you already claimed this" distinguishable from
// "someone else did".
func (a *API) getInvite(w http.ResponseWriter, r *http.Request) {
	preview, err := a.Store.InviteByCode(r.Context(),
		userIDFrom(r.Context()), strings.ToUpper(strings.TrimSpace(chi.URLParam(r, "code"))))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "invite_not_found")
			return
		}
		a.Log.ErrorContext(r.Context(), "get invite failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// claimInvite binds the caller to the contract's room.
//
// The claim carries no terms of its own. What the tenant agreed to is copied
// from the contract server-side, so the confirmation cannot be replayed with
// different numbers than the ones that were shown.
func (a *API) claimInvite(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(chi.URLParam(r, "code")))

	err := a.Store.ClaimInvite(r.Context(), userIDFrom(r.Context()), code)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, map[string]string{"status": "claimed"})
	case errors.Is(err, store.ErrAlreadyClaimed):
		writeError(w, http.StatusConflict, "invite_already_claimed")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "invite_not_found")
	default:
		a.Log.ErrorContext(r.Context(), "claim invite failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
	}
}

// getPaymentInfo returns the QR payloads for one invoice.
func (a *API) getPaymentInfo(w http.ResponseWriter, r *http.Request) {
	info, err := a.Store.PaymentInfoForInvoice(r.Context(),
		userIDFrom(r.Context()), chi.URLParam(r, "invoiceID"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "invoice_not_found")
			return
		}
		a.Log.ErrorContext(r.Context(), "payment info failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

type reportPaymentRequest struct {
	AmountSatang   int64  `json:"amount_satang"`
	Method         string `json:"method"`
	Ref            string `json:"ref"`
	IdempotencyKey string `json:"idempotency_key"`
}

// reportPayment records the tenant's claim that they paid.
//
// The amount is whatever they say they transferred, not the invoice total: a
// dormitory that lets tenants pay in instalments needs part-payments to be
// reportable. The owner verifies against their statement, and only then does it
// count.
func (a *API) reportPayment(w http.ResponseWriter, r *http.Request) {
	var req reportPaymentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	err := a.Store.ReportPayment(r.Context(), userIDFrom(r.Context()),
		chi.URLParam(r, "invoiceID"), req.AmountSatang, req.Method, req.Ref, req.IdempotencyKey)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending_verification"})
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "invoice_not_found")
	default:
		a.Log.ErrorContext(r.Context(), "report payment failed",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error")
	}
}
