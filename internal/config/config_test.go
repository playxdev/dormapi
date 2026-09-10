package config

import (
	"strings"
	"testing"
)

// A complete environment, as a deployment would set it.
func full() map[string]string {
	return map[string]string{
		"LINE_CHANNEL_ID":       "2011358311",
		"LINE_PROVIDER_ID":      "1234567890",
		"CLOUDFLARE_ACCOUNT_ID": "an-account",
		"D1_DATABASE_ID":        "a-database",
		"CLOUDFLARE_API_TOKEN":  "a-token",
		"JWT_SECRET":            strings.Repeat("s", 32),
		"PII_PEPPER":            "ZGV2LW9ubHktcGlpLXBlcHBlci0zMi1ieXRlcyEhISE=",
		"ALLOWED_ORIGINS":       "https://dorm.playxdev.com",
	}
}

func withEnv(t *testing.T, env map[string]string) {
	t.Helper()
	// Every variable this package reads, cleared first, so a value left in the
	// developer's own shell cannot make a test pass that would fail in CI.
	for _, key := range []string{
		"APP_ENV", "ADDR", "PORT", "ALLOWED_ORIGINS", "API_BASE_URL", "APP_LIFF_URL",
		"MAIL_FROM", "MAIL_FROM_NAME", "MAIL_API_TOKEN", "BACKOFFICE_URL",
		"LINE_CHANNEL_ID", "LINE_PROVIDER_ID", "LINE_CHANNEL_SECRET", "LINE_MESSAGING_TOKEN",
		"CLOUDFLARE_ACCOUNT_ID", "D1_DATABASE_ID", "CLOUDFLARE_API_TOKEN",
		"JWT_SECRET", "PII_PEPPER",
	} {
		t.Setenv(key, "")
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
}

func TestLoadAcceptsACompleteEnvironment(t *testing.T) {
	withEnv(t, full())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LineProviderID != "1234567890" || cfg.LineChannelID != "2011358311" {
		t.Errorf("cfg = %+v", cfg)
	}
	if len(cfg.AllowedOrigins) != 1 {
		t.Errorf("origins = %#v", cfg.AllowedOrigins)
	}
	if cfg.Env != "development" || cfg.IsProduction() {
		t.Errorf("env = %q", cfg.Env)
	}
}

// One variable at a time is how a deployment turns into an afternoon: fix the
// first, redeploy, learn about the second. Load names them all at once.
func TestLoadReportsEveryMissingValueTogether(t *testing.T) {
	withEnv(t, map[string]string{})

	_, err := Load()
	if err == nil {
		t.Fatal("an empty environment was accepted")
	}
	for _, want := range []string{
		"LINE_CHANNEL_ID", "LINE_PROVIDER_ID", "CLOUDFLARE_ACCOUNT_ID",
		"D1_DATABASE_ID", "CLOUDFLARE_API_TOKEN", "JWT_SECRET", "PII_PEPPER",
		"ALLOWED_ORIGINS",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s:\n%v", want, err)
		}
	}
}

// Each of these is a secret or a scope with no safe default. Absent, the
// process must refuse to start rather than serve wrongly.
func TestEverySecretIsRequired(t *testing.T) {
	for _, key := range []string{
		"LINE_CHANNEL_ID", "LINE_PROVIDER_ID", "CLOUDFLARE_ACCOUNT_ID",
		"D1_DATABASE_ID", "CLOUDFLARE_API_TOKEN", "JWT_SECRET", "PII_PEPPER",
		"ALLOWED_ORIGINS",
	} {
		t.Run(key, func(t *testing.T) {
			env := full()
			delete(env, key)
			withEnv(t, env)

			_, err := Load()
			if err == nil {
				t.Fatalf("started without %s", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("the error does not name %s:\n%v", key, err)
			}
		})
	}
}

// A short signing secret is worse than an absent one: the process starts and
// issues sessions anybody can forge.
func TestAShortJWTSecretIsRefused(t *testing.T) {
	env := full()
	env["JWT_SECRET"] = strings.Repeat("s", 31)
	withEnv(t, env)

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatalf("err = %v, want a 31-byte secret refused", err)
	}
}

// The webhook is optional. Without the secret its endpoint is not registered
// and without the token it cannot reply, but neither stops the service from
// serving residents.
func TestTheWebhookSecretsAreOptional(t *testing.T) {
	withEnv(t, full())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LineChannelSecret != "" || cfg.LineMessagingToken != "" {
		t.Errorf("cfg = %+v, want both empty", cfg)
	}

	env := full()
	env["LINE_CHANNEL_SECRET"] = "a-secret"
	env["LINE_MESSAGING_TOKEN"] = "a-token"
	withEnv(t, env)
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LineChannelSecret != "a-secret" || cfg.LineMessagingToken != "a-token" {
		t.Errorf("cfg = %+v", cfg)
	}
}

// Managed platforms inject PORT and route traffic to it; binding anywhere else
// fails their health check. ADDR stays available for local use and wins when
// both are set.
func TestListenAddress(t *testing.T) {
	cases := []struct {
		name, addr, port, want string
	}{
		{"neither", "", "", ":8080"},
		{"PORT from the platform", "", "8080", ":8080"},
		{"PORT is not always 8080", "", "3000", ":3000"},
		{"ADDR set locally", "127.0.0.1:9000", "", "127.0.0.1:9000"},
		{"ADDR wins", "127.0.0.1:9000", "3000", "127.0.0.1:9000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := full()
			if c.addr != "" {
				env["ADDR"] = c.addr
			}
			if c.port != "" {
				env["PORT"] = c.port
			}
			withEnv(t, env)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Addr != c.want {
				t.Errorf("Addr = %q, want %q", cfg.Addr, c.want)
			}
		})
	}
}

// The MINI App is served from another origin, so a missing entry here blocks
// every call in the browser — and the app reports that as "cannot reach the
// system", indistinguishable from the API being down.
func TestAllowedOriginsIsSplitAndTrimmed(t *testing.T) {
	env := full()
	env["ALLOWED_ORIGINS"] = " https://a.example , https://b.example ,, "
	withEnv(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"https://a.example", "https://b.example"}
	if len(cfg.AllowedOrigins) != len(want) {
		t.Fatalf("origins = %#v, want %#v", cfg.AllowedOrigins, want)
	}
	for i := range want {
		if cfg.AllowedOrigins[i] != want[i] {
			t.Errorf("origin %d = %q, want %q", i, cfg.AllowedOrigins[i], want[i])
		}
	}
}

// A trailing slash on a base URL becomes a double slash in a mailed link,
// which some clients then break across a line.
func TestBaseURLsLoseTheirTrailingSlash(t *testing.T) {
	env := full()
	env["API_BASE_URL"] = "https://api.example/"
	env["APP_LIFF_URL"] = "https://liff.line.me/2011358311-IAdUIyFx/"
	env["BACKOFFICE_URL"] = "https://backoffice.example/"
	withEnv(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIBaseURL != "https://api.example" {
		t.Errorf("APIBaseURL = %q", cfg.APIBaseURL)
	}
	if cfg.AppLIFFURL != "https://liff.line.me/2011358311-IAdUIyFx" {
		t.Errorf("AppLIFFURL = %q", cfg.AppLIFFURL)
	}
	if cfg.BackofficeURL != "https://backoffice.example" {
		t.Errorf("BackofficeURL = %q", cfg.BackofficeURL)
	}
}

// A separately scoped mail token is better — one that leaks cannot also read
// the database — but falling back keeps a deployment that has not split them
// from silently sending nothing.
func TestTheMailTokenFallsBackToTheCloudflareToken(t *testing.T) {
	withEnv(t, full())
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MailAPIToken != "a-token" {
		t.Errorf("MailAPIToken = %q, want the Cloudflare token", cfg.MailAPIToken)
	}

	env := full()
	env["MAIL_API_TOKEN"] = "a-mail-token"
	withEnv(t, env)
	cfg, _ = Load()
	if cfg.MailAPIToken != "a-mail-token" {
		t.Errorf("MailAPIToken = %q, want the one set explicitly", cfg.MailAPIToken)
	}
}

func TestProductionIsRecognised(t *testing.T) {
	env := full()
	env["APP_ENV"] = "production"
	withEnv(t, env)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.IsProduction() {
		t.Error("APP_ENV=production is not production")
	}
}
