// A D1 endpoint serving the real schema from real SQLite.
//
// The scripted fake in d1test proves what a statement says. It cannot prove
// that the statement is valid against the schema — a column that does not
// exist, a CHECK the value fails, a foreign key with no parent all answer
// exactly as well as a correct query does. Those are the failures this service
// would meet in production and nowhere before it, because Go cannot run on
// Workers and there is no local D1 for it to reach.
//
// So: the migrations the backoffice owns, applied to an in-process SQLite
// database, behind an HTTP server speaking the shape Cloudflare's REST API
// speaks. Same SQL, same parameters, same JSON on the way back.
//
// The schema is read from the dormplace checkout rather than copied here. A
// copy would drift, and a test passing against last month's schema is worse
// than no test at all — see schemaDir.
//
// This lives in a _test.go file on purpose: it is the only thing that pulls
// modernc.org/sqlite in, and a test dependency has no business being reachable
// from the binary that ships.
package repo

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/playxdev/dormapi/internal/d1"

	_ "modernc.org/sqlite"
)

// schemaDir locates the migrations the backoffice owns.
//
// The default is the sibling checkout this project is laid out with
// (works/dorm/{api,backoffice,mini}); DORMPLACE_MIGRATIONS overrides it. When
// neither exists the caller skips: a checkout of this repository alone cannot
// know the schema, and inventing one would test this file rather than the
// service.
func schemaDir() (string, bool) {
	if dir := os.Getenv("DORMPLACE_MIGRATIONS"); dir != "" {
		if _, err := os.Stat(dir); err == nil {
			return dir, true
		}
		return "", false
	}
	root, ok := moduleRoot()
	if !ok {
		return "", false
	}
	dir := filepath.Join(filepath.Dir(root), "backoffice", "migrations")
	if _, err := os.Stat(dir); err == nil {
		return dir, true
	}
	return "", false
}

// moduleRoot walks up from the test's working directory, which Go sets to the
// package under test, until it finds the go.mod above it.
func moduleRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// Server is an HTTP endpoint speaking D1's wire format over real SQLite.
type liveDB struct {
	t  *testing.T
	db *sql.DB

	// Calls records every statement in order, as d1test does, so a test can
	// assert on the shape of a write as well as on its effect.
	Calls  []liveCall
	server *httptest.Server
}

// Call is one statement as it arrived over the wire.
type liveCall struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params"`
}

// New starts a server over a fresh database with the schema applied.
//
// Skips the test when the migrations are not reachable.
func newLiveDB(t *testing.T) *liveDB {
	t.Helper()

	dir, ok := schemaDir()
	if !ok {
		t.Skip("dormplace migrations not found: set DORMPLACE_MIGRATIONS to the backoffice's migrations directory")
	}

	db, err := sql.Open("sqlite", "file:d1sql?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// One connection, so the shared in-memory database is not torn down when a
	// pooled connection is closed, and so writes are visible to the next read.
	db.SetMaxOpenConns(1)

	// The schema notes require this on every connection. Without it a foreign
	// key with no parent inserts happily and the test proves nothing.
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}

	for _, file := range migrations(t, dir) {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(file), err)
		}
	}

	s := &liveDB{t: t, db: db}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(func() {
		s.server.Close()
		db.Close()
	})
	return s
}

func migrations(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations in %s", dir)
	}
	sort.Strings(files)
	return files
}

// Exec runs a statement outside the wire, for seeding. It is not recorded in
// Calls: a test asserts on what the service sent, not on what the fixture did.
func (s *liveDB) Exec(query string, args ...any) {
	s.t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		s.t.Fatalf("seed: %v\n%s", err, query)
	}
}

// Row reads one row outside the wire, for asserting on what a write left
// behind. Nil when nothing matched.
func (s *liveDB) Row(query string, args ...any) map[string]any {
	s.t.Helper()
	rows, err := s.db.Query(query, args...)
	if err != nil {
		s.t.Fatalf("read back: %v\n%s", err, query)
	}
	defer rows.Close()

	out, err := scan(rows)
	if err != nil {
		s.t.Fatalf("read back: %v", err)
	}
	if len(out) == 0 {
		return nil
	}
	return out[0]
}

// returning matches the statements that answer with rows despite being writes.
var returning = regexp.MustCompile(`(?i)\bRETURNING\b`)

func (s *liveDB) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Fatalf("read request body: %v", err)
	}
	var c liveCall
	if err := json.Unmarshal(body, &c); err != nil {
		s.t.Fatalf("decode request body: %v", err)
	}
	s.Calls = append(s.Calls, c)

	rows, changes, err := s.run(c)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		// D1 reports its own failures in the body with 200, and the client
		// turns them into a *d1.Error. A UNIQUE violation has to arrive that
		// way or the code that recognises one never runs.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"errors":  []map[string]any{{"code": 7500, "message": err.Error()}},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"result": []map[string]any{{
			"success": true,
			"results": rows,
			"meta":    map[string]any{"changes": changes},
		}},
	})
}

func (s *liveDB) run(c liveCall) ([]map[string]any, int64, error) {
	trimmed := strings.TrimSpace(c.SQL)
	reads := strings.HasPrefix(strings.ToUpper(trimmed), "SELECT") || returning.MatchString(trimmed)

	if !reads {
		res, err := s.db.Exec(c.SQL, c.Params...)
		if err != nil {
			return nil, 0, err
		}
		changes, _ := res.RowsAffected()
		return []map[string]any{}, changes, nil
	}

	rows, err := s.db.Query(c.SQL, c.Params...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out, err := scan(rows)
	if err != nil {
		return nil, 0, err
	}
	// A RETURNING statement's changes are the rows it handed back, which is
	// what the guarded writes here read to decide whether they won.
	return out, int64(len(out)), nil
}

func scan(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	out := []map[string]any{}
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(any)
		}
		if err := rows.Scan(cells...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, name := range cols {
			row[name] = normalise(*cells[i].(*any))
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// normalise puts a value into the shape D1's JSON would arrive in. A driver
// hands back []byte for TEXT and int64 for INTEGER; the real client decodes
// JSON, where those are a string and a float64.
func normalise(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case int64:
		return float64(t)
	default:
		return v
	}
}

// Client is a d1.Client pointed at this server.
func (s *liveDB) Client() *d1.Client {
	return d1.New(d1.Config{
		AccountID:  "acct",
		DatabaseID: "db",
		APIToken:   "test-token",
		BaseURL:    s.server.URL,
	})
}
