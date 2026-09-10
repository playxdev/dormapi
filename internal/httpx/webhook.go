package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/playxdev/dormapi/internal/line"
	"github.com/playxdev/dormapi/internal/repo"
)

// maxWebhookBody caps what will be read from the endpoint. LINE batches at
// most a handful of events; anything of this size is not one of them.
const maxWebhookBody = 1 << 20

// selectPrefix marks the quick-reply data a tenant chooser sends back.
const selectPrefix = "select_tenant:"

// The one command the Official Account's rich menu sends as text. It has been
// arriving and going unanswered since the menu was published.
const adminCommand = "ADMIN"

// lineWebhook receives deliveries from the LINE Messaging API.
//
// # Why it answers 200 to almost everything
//
// LINE redelivers on any non-2xx. An event this service cannot handle is not
// going to become handleable on the third attempt, so answering anything but
// 200 turns one unknown event into a retry loop. The single exception is a bad
// signature, which is not LINE at all.
//
// # Why it does its work before answering
//
// The volume is a handful of messages a day and a reply token expires in
// minutes, so there is nothing to gain from a queue and something to lose: a
// failure after answering 200 would be invisible, because LINE would consider
// the event delivered.
func (a *API) lineWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	events, err := line.ParseWebhook(a.LineChannelSecret, r.Header.Get("X-Line-Signature"), body)
	if err != nil {
		if errors.Is(err, line.ErrBadSignature) {
			// Not from LINE, or not intact. Nothing is read out of the body,
			// and nothing is logged from it: it is attacker-controlled.
			a.Log.WarnContext(r.Context(), "webhook signature rejected",
				"request_id", RequestIDFrom(r.Context()))
			writeError(w, http.StatusUnauthorized, "bad_signature")
			return
		}
		a.Log.ErrorContext(r.Context(), "webhook could not be read",
			"request_id", RequestIDFrom(r.Context()), "error", err)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	for _, event := range events {
		a.handleLineEvent(r.Context(), event)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) handleLineEvent(ctx context.Context, event line.WebhookEvent) {
	// A standby event belongs to another module in a hand-over setup. Acting
	// on it would answer over whoever is actually handling the chat.
	if event.Mode == "standby" {
		return
	}
	// Groups and rooms are not where a tenancy is discussed, and a reply there
	// would show one resident's business to everyone in the group.
	if event.Source.Type != "user" || event.Source.UserID == "" {
		return
	}

	fresh, err := a.Repo.MarkWebhookEvent(ctx, "LINE", event.WebhookID)
	if err != nil {
		a.Log.ErrorContext(ctx, "webhook idempotency check failed",
			"request_id", RequestIDFrom(ctx), "error", err)
		return
	}
	if !fresh {
		return
	}

	switch event.Type {
	case "follow":
		a.reply(ctx, event, line.Message{Text: msgWelcome})
	case "message":
		if event.Message.Type != "text" {
			return
		}
		a.handleLineText(ctx, event, event.Message.Text)
	case "postback":
		a.handleLineChoice(ctx, event)
	}
}

// handleLineText answers a message, once it knows which operator it is about.
//
// The routing is STANDARD §4.3.1, and the rule that shapes it is that a person
// may rent from two operators: with more than one membership this service must
// ask rather than guess, because guessing means answering with one operator's
// business in a chat about the other's.
func (a *API) handleLineText(ctx context.Context, event line.WebhookEvent, text string) {
	accountID, operators, err := a.Repo.Inbound(ctx, event.Source.UserID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			// Messaged the account before ever opening the app.
			a.reply(ctx, event, line.Message{Text: msgNoAccount})
			return
		}
		a.Log.ErrorContext(ctx, "webhook inbound lookup failed",
			"request_id", RequestIDFrom(ctx), "error", err)
		return
	}

	switch len(operators) {
	case 0:
		a.reply(ctx, event, line.Message{Text: msgNoRoom})
		return
	case 1:
		a.answerFor(ctx, event, accountID, operators[0].TenantID, text)
		return
	}

	// More than one. An unexpired context from a few minutes ago is what keeps
	// this from asking on every message.
	if tenantID, err := a.Repo.ConversationTenant(ctx, accountID, repo.ChannelLINE); err == nil {
		a.answerFor(ctx, event, accountID, tenantID, text)
		return
	} else if !errors.Is(err, repo.ErrNotFound) {
		a.Log.ErrorContext(ctx, "conversation context failed",
			"request_id", RequestIDFrom(ctx), "error", err)
		return
	}

	// Ask. The names on these buttons are the operators this one person deals
	// with, shown only to them — and deliberately not logged, because a log an
	// operator can read would show them a competitor's name beside their own
	// resident (STANDARD §4.3.1).
	choices := make([]line.Choice, 0, len(operators))
	for _, op := range operators {
		choices = append(choices, line.Choice{Label: op.Name, Data: selectPrefix + op.TenantID})
	}
	a.reply(ctx, event, line.Message{Text: msgWhichOperator, Choices: choices})
}

// handleLineChoice files the chat under the operator the sender tapped.
//
// The tenant id arrives from the client. It is checked against the sender's
// own memberships before anything is written or answered, so a forged postback
// selects nothing (INV-03, INV-33).
func (a *API) handleLineChoice(ctx context.Context, event line.WebhookEvent) {
	tenantID, ok := strings.CutPrefix(event.Postback.Data, selectPrefix)
	if !ok || tenantID == "" {
		return
	}

	accountID, _, err := a.Repo.Inbound(ctx, event.Source.UserID)
	if err != nil {
		return
	}

	tenancy, err := a.Repo.ResolveTenancyIn(ctx, accountID, tenantID)
	if err != nil {
		// Not a member, or no live lease there. Both answer the same way.
		a.reply(ctx, event, line.Message{Text: msgNoRoom})
		return
	}

	if err := a.Repo.RememberConversationTenant(ctx, accountID, repo.ChannelLINE, tenantID); err != nil {
		a.Log.ErrorContext(ctx, "remember conversation failed",
			"request_id", RequestIDFrom(ctx), "error", err)
	}
	a.replyContact(ctx, event, tenancy)
}

// answerFor replies now that the operator is settled.
func (a *API) answerFor(ctx context.Context, event line.WebhookEvent, accountID, tenantID, text string) {
	tenancy, err := a.Repo.ResolveTenancyIn(ctx, accountID, tenantID)
	if err != nil {
		a.reply(ctx, event, line.Message{Text: msgNoRoom})
		return
	}

	switch strings.ToUpper(strings.TrimSpace(text)) {
	case adminCommand:
		a.replyContact(ctx, event, tenancy)
	default:
		// Anything else is a person typing at a system that does not read.
		// Saying so, and pointing at the app, is better than silence — which
		// is what the rich menu's button has been getting.
		a.reply(ctx, event, line.Message{Text: msgUseTheApp(tenancy.BuildingName, tenancy.RoomNumber)})
	}
}

func (a *API) replyContact(ctx context.Context, event line.WebhookEvent, tenancy *repo.Tenancy) {
	contact, err := a.Repo.Contact(ctx, tenancy)
	if err != nil {
		a.Log.ErrorContext(ctx, "contact lookup failed",
			"request_id", RequestIDFrom(ctx), "error", err)
		return
	}
	a.reply(ctx, event, line.Message{Text: msgContact(contact)})
}

// reply sends, and swallows the failure.
//
// A reply token is single use and expires in minutes, so there is nothing
// useful to do with an error but record it: retrying would spend a stale
// token, and failing the request would make LINE redeliver an event that was
// already handled.
func (a *API) reply(ctx context.Context, event line.WebhookEvent, message line.Message) {
	if a.Messenger == nil || event.ReplyToken == "" {
		return
	}
	if err := a.Messenger.Reply(ctx, event.ReplyToken, message); err != nil {
		a.Log.ErrorContext(ctx, "webhook reply failed",
			"request_id", RequestIDFrom(ctx), "error", err)
	}
}
