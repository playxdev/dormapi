// Package config loads runtime configuration from the environment.
//
// Nothing here has a default that would be unsafe in production: every secret
// is required and the process refuses to start without it.
package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	Env  string
	Addr string

	// AllowedOrigins are the browser origins permitted to call this API.
	AllowedOrigins []string

	// BackofficeURL is where the owner-facing service is served. The tenant
	// never signs in there; the API only needs it to hand out the address of
	// the lease a tenant is about to confirm, which that service renders.
	BackofficeURL string

	// LineChannelID is the numeric prefix of the LIFF ID for the environment
	// being served. It is the `aud` claim every accepted ID token must carry.
	LineChannelID string

	CloudflareAccountID string
	D1DatabaseID        string
	CloudflareAPIToken  string

	// JWTSecret signs the session tokens this service issues. Unrelated to any
	// LINE credential.
	JWTSecret []byte
}

func (c Config) IsProduction() bool { return c.Env == "production" }

// Load reads configuration and reports every missing value at once, rather
// than failing on one variable at a time.
func Load() (Config, error) {
	cfg := Config{
		Env:                 getenv("APP_ENV", "development"),
		Addr:                listenAddr(),
		LineChannelID:       os.Getenv("LINE_CHANNEL_ID"),
		CloudflareAccountID: os.Getenv("CLOUDFLARE_ACCOUNT_ID"),
		D1DatabaseID:        os.Getenv("D1_DATABASE_ID"),
		CloudflareAPIToken:  os.Getenv("CLOUDFLARE_API_TOKEN"),
		JWTSecret:           []byte(os.Getenv("JWT_SECRET")),
		BackofficeURL:       strings.TrimRight(os.Getenv("BACKOFFICE_URL"), "/"),
	}

	origins := getenv("ALLOWED_ORIGINS", "")
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.AllowedOrigins = append(cfg.AllowedOrigins, o)
		}
	}

	var missing []string
	if cfg.LineChannelID == "" {
		missing = append(missing, "LINE_CHANNEL_ID")
	}
	if cfg.CloudflareAccountID == "" {
		missing = append(missing, "CLOUDFLARE_ACCOUNT_ID")
	}
	if cfg.D1DatabaseID == "" {
		missing = append(missing, "D1_DATABASE_ID")
	}
	if cfg.CloudflareAPIToken == "" {
		missing = append(missing, "CLOUDFLARE_API_TOKEN")
	}
	if len(cfg.JWTSecret) < 32 {
		missing = append(missing, "JWT_SECRET (at least 32 bytes)")
	}
	if len(cfg.AllowedOrigins) == 0 {
		missing = append(missing, "ALLOWED_ORIGINS")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing configuration: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

// listenAddr resolves the address to bind.
//
// Managed platforms (DigitalOcean App Platform, Cloud Run, Heroku) inject PORT
// and route traffic to it; binding anywhere else fails their health check. ADDR
// stays available for local use and takes precedence when set explicitly.
func listenAddr() string {
	if addr := os.Getenv("ADDR"); addr != "" {
		return addr
	}
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8080"
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
