package d1

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// answer serves one canned D1 response and records what was asked for.
func answer(t *testing.T, body string) (*Client, *[]map[string]any) {
	t.Helper()
	var seen []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		req["_path"] = r.URL.Path
		req["_auth"] = r.Header.Get("Authorization")
		seen = append(seen, req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	return New(Config{
		AccountID: "acct", DatabaseID: "db", APIToken: "a-token", BaseURL: srv.URL,
	}), &seen
}

const oneRow = `{"success":true,"result":[{"success":true,
	"results":[{"id":"01ROW","total":525000}],"meta":{"changes":1}}]}`

func TestQuerySendsParametersSeparatelyFromTheSQL(t *testing.T) {
	client, seen := answer(t, oneRow)

	res, err := client.Query(context.Background(),
		`SELECT id, total FROM invoice WHERE tenant_id = ?1 AND invoice_id = ?2`,
		"01TENANT", "01INVOICE")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	req := (*seen)[0]
	if !strings.Contains(req["sql"].(string), "WHERE tenant_id = ?1") {
		t.Errorf("sql = %v", req["sql"])
	}
	params, _ := req["params"].([]any)
	if len(params) != 2 || params[0] != "01TENANT" || params[1] != "01INVOICE" {
		t.Errorf("params = %#v", params)
	}
	// Never interpolated into the statement. That is the whole reason the
	// repository layer is allowed to take strings from a request at all.
	if strings.Contains(req["sql"].(string), "01TENANT") {
		t.Error("a parameter was built into the SQL")
	}

	if len(res.Results) != 1 || res.Results[0]["id"] != "01ROW" {
		t.Errorf("results = %#v", res.Results)
	}
	// SQLite integers arrive as JSON numbers, which decode to float64. Money is
	// stored in satang precisely so the values stay exactly representable.
	if res.Results[0]["total"] != float64(525000) {
		t.Errorf("total = %#v", res.Results[0]["total"])
	}
	if res.Meta.Changes != 1 {
		t.Errorf("changes = %d", res.Meta.Changes)
	}
}

func TestQueryAddressesTheConfiguredDatabaseAndAuthenticates(t *testing.T) {
	client, seen := answer(t, oneRow)
	if _, err := client.Query(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("Query: %v", err)
	}

	req := (*seen)[0]
	if req["_path"] != "/accounts/acct/d1/database/db/query" {
		t.Errorf("path = %v", req["_path"])
	}
	if req["_auth"] != "Bearer a-token" {
		t.Errorf("authorization = %v", req["_auth"])
	}
}

// A statement with no parameters must still send an empty array. D1 rejects a
// request whose `params` is null.
func TestAParameterlessQuerySendsAnEmptyArray(t *testing.T) {
	client, seen := answer(t, oneRow)
	if _, err := client.Query(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	params, ok := (*seen)[0]["params"].([]any)
	if !ok || params == nil {
		t.Errorf("params = %#v, want an empty array", (*seen)[0]["params"])
	}
	if len(params) != 0 {
		t.Errorf("params = %#v, want empty", params)
	}
}

// D1 reports its own failures in the body with HTTP 200. Reading only the
// status code would treat a rejected statement as a success with no rows —
// which is exactly what a guarded write reads to decide it lost.
func TestAFailureInTheBodyIsAnError(t *testing.T) {
	client, _ := answer(t, `{"success":false,"errors":[
		{"code":7500,"message":"UNIQUE constraint failed: payment.idempotency_key"}]}`)

	_, err := client.Query(context.Background(), "INSERT INTO payment VALUES (?1)", "x")
	if err == nil {
		t.Fatal("a rejected statement was reported as success")
	}
	var d1err *Error
	if !errors.As(err, &d1err) {
		t.Fatalf("err = %T, want *d1.Error", err)
	}
	if d1err.Code != 7500 {
		t.Errorf("code = %d", d1err.Code)
	}
	// The repository layer recognises a retry by this text, so it has to
	// survive into the error's message.
	if !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Errorf("err = %v", err)
	}
}

func TestAFailureWithNoDetailIsStillAnError(t *testing.T) {
	client, _ := answer(t, `{"success":false,"errors":[]}`)
	if _, err := client.Query(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("an unsuccessful response was accepted")
	}
}

func TestANonJSONResponseIsAnError(t *testing.T) {
	client, _ := answer(t, `<html>502 Bad Gateway</html>`)
	if _, err := client.Query(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("an HTML error page was decoded as a result")
	}
}

// A success carrying no result at all. Returning a nil *Result would panic in
// the caller on the next line.
func TestAnEmptyResultSetIsAnErrorNotANilResult(t *testing.T) {
	client, _ := answer(t, `{"success":true,"result":[]}`)
	res, err := client.Query(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("a response with no result was accepted")
	}
	if res != nil {
		t.Errorf("res = %#v, want nil alongside the error", res)
	}
}

// The constraint the whole schema is shaped around: D1 refuses parameters
// whenever more than one statement is sent, so Batch takes none. Accepting
// them would mean building SQL by concatenation, which is the injection this
// design exists to make impossible.
func TestBatchRefusesParameters(t *testing.T) {
	client, seen := answer(t, oneRow)

	_, err := client.Batch(context.Background(), []Statement{
		{SQL: "DELETE FROM webhook_event_seen WHERE processed_at < '2026-01-01'"},
		{SQL: "UPDATE invoice SET total = ?1", Params: []any{1}},
	})
	if !errors.Is(err, ErrParamsInBatch) {
		t.Fatalf("err = %v, want ErrParamsInBatch", err)
	}
	if len(*seen) != 0 {
		t.Errorf("a batch with parameters was sent anyway: %#v", *seen)
	}
}

func TestBatchJoinsStatementsIntoOneRequest(t *testing.T) {
	client, seen := answer(t, oneRow)

	_, err := client.Batch(context.Background(), []Statement{
		{SQL: "PRAGMA foreign_keys = ON;"},
		{SQL: "  SELECT 1  "},
	})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("sent %d requests, want 1 — a batch is atomic only as one", len(*seen))
	}
	sql := (*seen)[0]["sql"].(string)
	if sql != "PRAGMA foreign_keys = ON; SELECT 1;" {
		t.Errorf("sql = %q", sql)
	}
}

func TestAnEmptyBatchSendsNothing(t *testing.T) {
	client, seen := answer(t, oneRow)
	res, err := client.Batch(context.Background(), nil)
	if err != nil || res != nil {
		t.Fatalf("Batch(nil) = %#v, %v", res, err)
	}
	if len(*seen) != 0 {
		t.Errorf("an empty batch was sent: %#v", *seen)
	}
}

// Configuration is the failure that matters: a wrong database id leaves the
// service running and answering, with every query failing.
func TestPingUsesTheCheapestQueryThereIs(t *testing.T) {
	client, seen := answer(t, oneRow)
	if err := client.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if (*seen)[0]["sql"] != "SELECT 1" {
		t.Errorf("sql = %v", (*seen)[0]["sql"])
	}
}

func TestPingReportsAnUnreachableDatabase(t *testing.T) {
	client, _ := answer(t, `{"success":false,"errors":[{"code":7404,"message":"not found"}]}`)
	if err := client.Ping(context.Background()); err == nil {
		t.Fatal("Ping succeeded against a database that answered not found")
	}
}

func TestACancelledContextStopsTheRequest(t *testing.T) {
	client, _ := answer(t, oneRow)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.Query(ctx, "SELECT 1"); err == nil {
		t.Fatal("a cancelled request was sent anyway")
	}
}
