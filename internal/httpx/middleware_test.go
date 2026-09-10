package httpx

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func call(h http.Handler, method, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1/me", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var allowed = []string{"https://dorm.playxdev.com", "http://localhost:5173"}

func ok() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "reached")
	})
}

// The MINI App is served from another origin, so this header is the difference
// between the app working and the app reporting "cannot reach the system".
func TestCORSAllowsAConfiguredOrigin(t *testing.T) {
	for _, origin := range allowed {
		t.Run(origin, func(t *testing.T) {
			rec := call(CORS(allowed)(ok()), http.MethodGet, origin)
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != origin {
				t.Errorf("Allow-Origin = %q, want %q", got, origin)
			}
			// Without Vary, a shared cache can serve one origin's allowance to
			// another.
			if rec.Header().Get("Vary") != "Origin" {
				t.Errorf("Vary = %q", rec.Header().Get("Vary"))
			}
			if rec.Body.String() != "reached" {
				t.Error("the request did not reach the handler")
			}
		})
	}
}

// An origin nobody configured gets no allowance. It must not be echoed back:
// echoing the request's own origin allows everyone, which is the whole
// vulnerability this header exists to prevent.
func TestCORSGivesAnUnknownOriginNothing(t *testing.T) {
	for _, origin := range []string{
		"https://evil.example",
		// Close enough to fool a prefix or suffix check, and neither is used.
		"https://dorm.playxdev.com.evil.example",
		"https://notdorm.playxdev.com",
		"http://dorm.playxdev.com",
	} {
		t.Run(origin, func(t *testing.T) {
			rec := call(CORS(allowed)(ok()), http.MethodGet, origin)
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("Allow-Origin = %q, want none", got)
			}
		})
	}
}

// A same-origin or server-to-server call sends no Origin at all. It must not
// be answered with an empty allowance, and it must still reach the handler —
// `GET /healthz` is polled by a platform that sends no Origin.
func TestCORSIgnoresARequestWithNoOrigin(t *testing.T) {
	rec := call(CORS(allowed)(ok()), http.MethodGet, "")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q, want none", got)
	}
	if rec.Body.String() != "reached" {
		t.Error("a request without an Origin was blocked")
	}
}

// The preflight is answered here and never reaches the handler, which would
// otherwise see a method it has no route for.
func TestCORSAnswersThePreflightItself(t *testing.T) {
	rec := call(CORS(allowed)(ok()), http.MethodOptions, allowed[0])
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
	if rec.Body.String() != "" {
		t.Error("the preflight reached the handler")
	}
	// The app sends its session token, so the header it sends has to be one
	// the preflight permits.
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Errorf("Allow-Headers = %q", rec.Header().Get("Access-Control-Allow-Headers"))
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("Allow-Methods = %q", rec.Header().Get("Access-Control-Allow-Methods"))
	}
}

// A preflight from an origin nobody configured is still answered 204, and
// still carries no allowance. The browser refuses the real request, which is
// the correct outcome, and no handler ran.
func TestCORSPreflightFromAnUnknownOriginCarriesNoAllowance(t *testing.T) {
	rec := call(CORS(allowed)(ok()), http.MethodOptions, "https://evil.example")
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("an unknown origin was allowed through the preflight")
	}
	if rec.Body.String() != "" {
		t.Error("the preflight reached the handler")
	}
}

// A panic must not drop the connection, and the stack must not reach the
// caller: it names files, line numbers and sometimes values.
func TestRecoverAnswers500WithoutTheStack(t *testing.T) {
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("a nil map somewhere in the repository")
	})

	rec := call(Recover(discard())(panicking), http.MethodGet, "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "internal_error") {
		t.Errorf("body = %q, want the stable code", body)
	}
	if strings.Contains(body, "nil map") || strings.Contains(body, "goroutine") {
		t.Errorf("the panic reached the caller: %q", body)
	}
}

func TestRecoverLeavesAWorkingHandlerAlone(t *testing.T) {
	rec := call(Recover(discard())(ok()), http.MethodGet, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "reached" {
		t.Errorf("status = %d body = %q", rec.Code, rec.Body.String())
	}
}

// Every line of the log carries this, so a report of "I could not pay" can be
// followed through the system.
func TestRequestIDIsGeneratedAndReturned(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	rec := call(h, http.MethodGet, "")
	if seen == "" {
		t.Fatal("no request id reached the handler")
	}
	if rec.Header().Get("X-Request-Id") != seen {
		t.Errorf("header = %q, handler saw %q", rec.Header().Get("X-Request-Id"), seen)
	}
}

// A caller that brings its own id keeps it, so one id follows a request across
// the MINI App, this service and the logs.
func TestAnIncomingRequestIDIsKept(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("X-Request-Id", "from-the-app")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "from-the-app" {
		t.Errorf("id = %q, want the one the caller sent", seen)
	}
	if rec.Header().Get("X-Request-Id") != "from-the-app" {
		t.Errorf("header = %q", rec.Header().Get("X-Request-Id"))
	}
}

func TestRequestIDsDoNotRepeat(t *testing.T) {
	seen := make(map[string]bool, 64)
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	for i := 0; i < 64; i++ {
		id := call(h, http.MethodGet, "").Header().Get("X-Request-Id")
		if seen[id] {
			t.Fatalf("request id %q was issued twice", id)
		}
		seen[id] = true
	}
}

// The log line is what a support question is answered from, and it must never
// be what a credential leaks through.
func TestTheRequestLogCarriesNoCredential(t *testing.T) {
	var out strings.Builder
	logger := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h := RequestID(Logger(logger)(ok()))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer a-session-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	line := out.String()
	if !strings.Contains(line, "/api/v1/me") || !strings.Contains(line, "request_id") {
		t.Errorf("the log is missing what it is for:\n%s", line)
	}
	if strings.Contains(line, "a-session-token") || strings.Contains(line, "Bearer") {
		t.Errorf("the log carries the session token:\n%s", line)
	}
}
