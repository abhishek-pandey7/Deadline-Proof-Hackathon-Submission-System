// Command server runs the submission API (and, later, its workers).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/config"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/db"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/httpapi"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/storage"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
}

func run() error {
	envFile := flag.String("env-file", "", "optional KEY=VALUE file; real environment variables win")
	flag.Parse()
	if *envFile != "" {
		if err := config.LoadEnvFile(*envFile); err != nil {
			return err
		}
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	var handler slog.Handler = slog.NewJSONHandler(os.Stdout, nil)
	if cfg.IsDevelopment() {
		handler = slog.NewTextHandler(os.Stdout, nil)
	}
	log := slog.New(handler)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL, 20)
	if err != nil {
		return err
	}
	defer pool.Close()

	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	log.Info("migrations done", "applied", applied)

	store, err := storage.New(cfg.Storage)
	if err != nil {
		return err
	}
	if cfg.IsDevelopment() {
		if err := store.EnsureBucket(ctx); err != nil {
			return err
		}
	}

	// Insert-only for now: the check workers run from the same binary later.
	queue, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		return fmt.Errorf("river client: %w", err)
	}
	subs := submissions.New(pool, store, queue, log, submissions.Options{})

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.New(httpapi.Deps{
			DB:            pool,
			Log:           log,
			Submissions:   subs,
			SecureCookies: !cfg.IsDevelopment(),
			TrustProxy:    !cfg.IsDevelopment(),
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// No WriteTimeout: live status uses long-lived SSE streams.
		IdleTimeout: 120 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr, "env", cfg.Env)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
	}
	return nil
}
