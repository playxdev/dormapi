// Package d1test serves D1's wire format to a test.
//
// The store's assumptions are the SQL it sends, the parameters it sends with
// it, and the number of changed rows it reads back. A double that answered
// method calls instead of HTTP would carry none of them, so this is an HTTP
// server speaking the real shape.
package d1test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/playxdev/dormapi/internal/d1"
)

// Server stands in for Cloudflare's D1 endpoint.
type Server struct {
	t *testing.T

	// Calls records every statement in order. Order is part of the contract
	// wherever one write guards another - an audit row before the change it
	// describes, a token spent before the account it unlocks moves.
	Calls []Call

	// answers are returned one per request, in order. A request past the end
	// gets an empty successful result, which is what an UPDATE matching
	// nothing looks like.
	answers []Answer
	server  *httptest.Server
}

// Call is one statement as it arrived over the wire.
type Call struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params"`
}

// Answer is what the server returns for one statement.
type Answer struct {
	Rows    []map[string]any
	Changes int64

	// Failure, when set, is returned as a D1 API error instead of a result.
	Failure string
}

// New starts a server that answers the given statements in order.
func New(t *testing.T, answers ...Answer) *Server {
	t.Helper()
	f := &Server{t: t, answers: answers}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *Server) serve(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		f.t.Errorf("Authorization = %q, want bearer test-token", got)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Fatalf("read request body: %v", err)
	}
	var c Call
	if err := json.Unmarshal(body, &c); err != nil {
		f.t.Fatalf("decode request body: %v", err)
	}
	n := len(f.Calls)
	f.Calls = append(f.Calls, c)

	w.Header().Set("Content-Type", "application/json")
	if n < len(f.answers) && f.answers[n].Failure != "" {
		json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"errors":  []map[string]any{{"code": 7500, "message": f.answers[n].Failure}},
		})
		return
	}

	var a Answer
	if n < len(f.answers) {
		a = f.answers[n]
	}
	if a.Rows == nil {
		a.Rows = []map[string]any{}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"result": []map[string]any{{
			"success": true,
			"results": a.Rows,
			"meta":    map[string]any{"changes": a.Changes},
		}},
	})
}

// Client is a d1.Client pointed at this server.
func (f *Server) Client() *d1.Client {
	return d1.New(d1.Config{
		AccountID:  "acct",
		DatabaseID: "db",
		APIToken:   "test-token",
		BaseURL:    f.server.URL,
	})
}
