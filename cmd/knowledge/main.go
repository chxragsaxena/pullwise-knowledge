// Command knowledge runs the PullWise knowledge service (repo indexing and search).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chxragsaxena/pullwise-knowledge/internal/api"
	"github.com/chxragsaxena/pullwise-knowledge/internal/auth"
	"github.com/chxragsaxena/pullwise-knowledge/internal/config"
	"github.com/chxragsaxena/pullwise-knowledge/internal/embed"
	"github.com/chxragsaxena/pullwise-knowledge/internal/store"
	"github.com/chxragsaxena/pullwise-knowledge/internal/tlsutil"
	"github.com/chxragsaxena/pullwise-knowledge/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("knowledge service failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("invalid configuration:\n%w", err)
	}
	log := slog.Default()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL, cfg.DatabaseSchema)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.Migrate(ctx, db, migrations.Files); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	verifier, err := auth.NewVerifier(cfg.JWTSecret, cfg.JWTIssuer, cfg.JWTAudience)
	if err != nil {
		return err
	}
	handler := api.New(store.New(db), embed.NewCohereEmbedder(cfg.CohereKey), verifier, log).Handler()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      6 * time.Minute, // indexing embeds many chunks and may wait out rate limits
		IdleTimeout:       60 * time.Second,
	}

	tlsOn := cfg.TLSCertFile != ""
	if tlsOn {
		if srv.TLSConfig, err = tlsutil.ServerConfig(cfg.TLSCertFile, cfg.TLSKeyFile, cfg.TLSClientCAFile); err != nil {
			return err
		}
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "tls", tlsOn, "mutual_tls", cfg.TLSClientCAFile != "")
		if tlsOn {
			errCh <- srv.ListenAndServeTLS("", "") // certificates are in srv.TLSConfig
		} else {
			log.Warn("TLS is off: only run this on a private network")
			errCh <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Info("shutting down")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
