// Command api serves the dorm.place backend.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/playxdev/dormapi/internal/auth"
	"github.com/playxdev/dormapi/internal/config"
	"github.com/playxdev/dormapi/internal/d1"
	"github.com/playxdev/dormapi/internal/httpx"
	"github.com/playxdev/dormapi/internal/line"
	"github.com/playxdev/dormapi/internal/mail"
	"github.com/playxdev/dormapi/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg)
	slog.SetDefault(log)

	db := d1.New(d1.Config{
		AccountID:  cfg.CloudflareAccountID,
		DatabaseID: cfg.D1DatabaseID,
		APIToken:   cfg.CloudflareAPIToken,
	})

	// Fail at startup rather than on the first user request: bad credentials
	// or a wrong database ID should never reach a tenant as a vague error.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.Ping(ctx); err != nil {
		return err
	}

	// No mail transport configured: log what would have been sent rather than
	// fail. Development needs no mailbox, and production without one is a
	// deployment error that should be visible in the logs rather than a
	// process that refuses to start.
	var sender mail.Sender = mail.Log{Logger: log}
	if cfg.MailFrom != "" && cfg.MailAPIToken != "" {
		sender = &mail.Cloudflare{
			AccountID: cfg.CloudflareAccountID,
			Token:     cfg.MailAPIToken,
			From:      cfg.MailFrom,
			FromName:  cfg.MailFromName,
		}
	} else {
		log.Warn("mail transport not configured: verification and recovery links will not be delivered")
	}

	api := &httpx.API{
		Store:      store.New(db, cfg.BackofficeURL),
		Verifier:   line.NewVerifier(cfg.LineChannelID),
		Issuer:     auth.NewIssuer(cfg.JWTSecret, 12*time.Hour),
		Mail:       sender,
		Log:        log,
		APIBaseURL: cfg.APIBaseURL,
		AppLIFFURL: cfg.AppLIFFURL,
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.Routes(cfg.AllowedOrigins),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case sig := <-shutdown:
		log.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	if cfg.IsProduction() {
		return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
