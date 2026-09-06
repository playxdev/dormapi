// Package d1 is a client for Cloudflare D1 over the REST API.
//
// D1 is designed to be reached through a Workers binding. This service runs
// outside Workers, so every call is an HTTPS request to the Cloudflare API.
// Two consequences shape the API offered here:
//
//   - Each call costs a network round trip. Avoid N+1 query patterns; prefer
//     one statement that joins over several that do not.
//   - D1 rejects BEGIN/COMMIT/SAVEPOINT, and the REST API refuses parameters
//     alongside multiple statements. Several statements in one request are
//     atomic, but only unparameterised ones — so no write carrying user input
//     can span more than a single statement. Every such write here is expressed
//     as one statement for that reason.
package d1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const apiBase = "https://api.cloudflare.com/client/v4"

// Client talks to one D1 database.
type Client struct {
	accountID  string
	databaseID string
	token      string
	baseURL    string
	http       *http.Client
}

// Config carries the credentials needed to reach a D1 database. The token must
// be a scoped API token with D1 edit permission — never a global API key.
type Config struct {
	AccountID  string
	DatabaseID string
	APIToken   string
	Timeout    time.Duration

	// BaseURL replaces Cloudflare's API root. Empty everywhere but in tests,
	// which point it at a server speaking D1's wire format — the SQL text, the
	// parameter list and the shape of the answer are what the store's
	// behaviour actually rests on, and a fake that does not carry them tests
	// nothing.
	BaseURL string
}

func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	base := cfg.BaseURL
	if base == "" {
		base = apiBase
	}
	return &Client{
		accountID:  cfg.AccountID,
		databaseID: cfg.DatabaseID,
		token:      cfg.APIToken,
		baseURL:    strings.TrimSuffix(base, "/"),
		http:       &http.Client{Timeout: timeout},
	}
}

// Meta describes what one statement did.
type Meta struct {
	Changes      int64   `json:"changes"`
	LastRowID    int64   `json:"last_row_id"`
	RowsRead     int64   `json:"rows_read"`
	RowsWritten  int64   `json:"rows_written"`
	Duration     float64 `json:"duration"`
	ServedByColo string  `json:"served_by_colo"`
}

// Result is the outcome of a single statement. D1 returns one Result per
// statement, in the order the statements were sent.
type Result struct {
	Results []map[string]any `json:"results"`
	Success bool             `json:"success"`
	Meta    Meta             `json:"meta"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type apiResponse struct {
	Result   []Result   `json:"result"`
	Success  bool       `json:"success"`
	Errors   []apiError `json:"errors"`
	Messages []any      `json:"messages"`
}

// Error is a failure reported by D1 itself, as opposed to a transport failure.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("d1: %s (code %d)", e.Message, e.Code)
}

// Query runs one statement and returns its result.
//
// Parameters are positional and referenced as ?1, ?2, ... in the SQL. Never
// build SQL by concatenating values.
func (c *Client) Query(ctx context.Context, sql string, params ...any) (*Result, error) {
	results, err := c.exec(ctx, sql, params)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("d1: no result returned")
	}
	return &results[0], nil
}

// Statement is one entry in a batch.
type Statement struct {
	SQL    string
	Params []any
}

// ErrParamsInBatch reports the D1 limitation that makes Batch unsuitable for
// writes carrying user input.
var ErrParamsInBatch = errors.New("d1: batch statements cannot take parameters")

// Batch runs several statements in one request, atomically: if any fails, none
// take effect.
//
// D1 refuses parameters when more than one statement is sent, so Batch accepts
// only unparameterised SQL and rejects anything else rather than tempting a
// caller into building SQL by concatenation. That confines it to schema and
// maintenance work.
//
// Application writes therefore have no multi-statement transaction available.
// Each one must be a single statement — which is why balances are derived from
// append-only rows rather than maintained as a running total.
func (c *Client) Batch(ctx context.Context, stmts []Statement) ([]Result, error) {
	if len(stmts) == 0 {
		return nil, nil
	}

	var sql strings.Builder
	for i, s := range stmts {
		if len(s.Params) > 0 {
			return nil, ErrParamsInBatch
		}
		if i > 0 {
			sql.WriteString("; ")
		}
		sql.WriteString(strings.TrimSuffix(strings.TrimSpace(s.SQL), ";"))
	}
	sql.WriteString(";")

	return c.exec(ctx, sql.String(), nil)
}

func (c *Client) exec(ctx context.Context, sql string, params []any) ([]Result, error) {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"sql": sql, "params": params})
	if err != nil {
		return nil, fmt.Errorf("d1: encode request: %w", err)
	}

	url := fmt.Sprintf("%s/accounts/%s/d1/database/%s/query", c.baseURL, c.accountID, c.databaseID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("d1: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("d1: request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("d1: read response: %w", err)
	}

	var out apiResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("d1: decode response (status %d): %w", resp.StatusCode, err)
	}

	if !out.Success {
		if len(out.Errors) > 0 {
			return nil, &Error{Code: out.Errors[0].Code, Message: out.Errors[0].Message}
		}
		return nil, fmt.Errorf("d1: request unsuccessful (status %d)", resp.StatusCode)
	}

	return out.Result, nil
}

// Ping verifies that the credentials and database ID are usable.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Query(ctx, "SELECT 1")
	return err
}
