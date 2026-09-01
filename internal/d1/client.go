// Package d1 is a client for Cloudflare D1 over the REST API.
//
// D1 is designed to be reached through a Workers binding. This service runs
// outside Workers, so every call is an HTTPS request to the Cloudflare API.
// Two consequences shape the API offered here:
//
//   - Each call costs a network round trip. Avoid N+1 query patterns; prefer
//     one statement that joins over several that do not.
//   - D1 rejects BEGIN/COMMIT/SAVEPOINT. Atomicity comes from sending several
//     statements in a single request instead: the whole batch commits or none
//     of it does. Batch is therefore the only transaction primitive available.
package d1

import (
	"bytes"
	"context"
	"encoding/json"
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
	http       *http.Client
}

// Config carries the credentials needed to reach a D1 database. The token must
// be a scoped API token with D1 edit permission — never a global API key.
type Config struct {
	AccountID  string
	DatabaseID string
	APIToken   string
	Timeout    time.Duration
}

func New(cfg Config) *Client {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	return &Client{
		accountID:  cfg.AccountID,
		databaseID: cfg.DatabaseID,
		token:      cfg.APIToken,
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

// Batch runs several statements in one request. The batch is atomic: if any
// statement fails, none of them take effect.
//
// This is the only way to get transactional behaviour out of D1 from outside
// Workers, so any multi-step write that must not half-apply belongs here.
//
// Parameters are shared across the whole batch and numbered continuously, so
// callers must offset placeholders accordingly. Where that becomes awkward,
// prefer inlining literals that are not user input, or restructure the write
// so it needs fewer statements.
func (c *Client) Batch(ctx context.Context, stmts []Statement) ([]Result, error) {
	if len(stmts) == 0 {
		return nil, nil
	}

	var sql strings.Builder
	params := make([]any, 0, len(stmts))
	for i, s := range stmts {
		if i > 0 {
			sql.WriteString("; ")
		}
		sql.WriteString(strings.TrimSuffix(strings.TrimSpace(s.SQL), ";"))
		params = append(params, s.Params...)
	}
	sql.WriteString(";")

	return c.exec(ctx, sql.String(), params)
}

func (c *Client) exec(ctx context.Context, sql string, params []any) ([]Result, error) {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"sql": sql, "params": params})
	if err != nil {
		return nil, fmt.Errorf("d1: encode request: %w", err)
	}

	url := fmt.Sprintf("%s/accounts/%s/d1/database/%s/query", apiBase, c.accountID, c.databaseID)
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
