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

	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/checks"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/config"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/db"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/events"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/httpapi"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/jobs"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/organizer"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/storage"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/submissions"
	"github.com/abhishek-pandey7/Deadline-Proof-Hackathon-Submission-System/internal/webui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
}

func run() error {
	envFile := flag.String("env-file", "", "optional KEY=VALUE file; real environment variables win")
	checkWorkers := flag.Int("check-workers", 2, "concurrent check jobs (the free tier has 0.1 CPU)")
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
	if cfg.StorageCapBytes > 0 {
		if err := submissions.SetStorageCap(ctx, pool, cfg.StorageCapBytes); err != nil {
			return err
		}
	}
	if l, err := submissions.ReadLedger(ctx, pool); err == nil {
		log.Info("storage ledger", "used_bytes", l.UsedBytes, "cap_bytes", l.CapBytes, "uploaded_bytes_total", l.UploadedBytesTotal)
	}
	if cfg.IsDevelopment() {
		if err := store.EnsureBucket(ctx); err != nil {
			return err
		}
	}

	// The check workers and the stale-upload sweep run in this same process,
	// which fits the single free-tier instance. River enqueues and works jobs
	// through Postgres, so a second instance would share the work safely.
	pdf := checks.FindPoppler()
	log.Info("pdf previews", "renderer_installed", pdf != nil)
	sweep := &submissions.SweepWorker{}
	workers := river.NewWorkers()
	river.AddWorker(workers, &checks.Worker{
		DB:  pool,
		Log: log,
		Runner: &checks.Runner{
			Store:   store,
			Links:   checks.NewLinkChecker(checks.LinkOptions{}),
			PDF:     pdf,
			Archive: checks.DefaultArchiveLimits,
		},
	})
	river.AddWorker(workers, sweep)
	measure := &submissions.MeasureWorker{Bucket: store}
	river.AddWorker(workers, measure)
	periodic := []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(time.Minute),
			func() (river.JobArgs, *river.InsertOpts) { return jobs.ExpireStaleUploads{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true}),
		river.NewPeriodicJob(river.PeriodicInterval(10*time.Minute),
			func() (river.JobArgs, *river.InsertOpts) { return jobs.MeasureStorage{}, nil },
			&river.PeriodicJobOpts{RunOnStart: true}),
	}
	queue, err := river.NewClient(riverpgxv5.New(pool), checks.ClientConfig(workers, *checkWorkers, checks.JobTimeout, 5*time.Minute, periodic))
	if err != nil {
		return fmt.Errorf("river client: %w", err)
	}
	subs := submissions.New(pool, store, queue, log, submissions.Options{})
	sweep.Service = subs
	measure.Service = subs

	if err := queue.Start(ctx); err != nil {
		return fmt.Errorf("start workers: %w", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := queue.Stop(stopCtx); err != nil {
			log.Warn("stop workers", "err", err)
		}
	}()

	// One LISTEN connection feeds every live stream. DATABASE_URL must be the
	// direct (non-pooler) host for LISTEN to work on Neon.
	hub := events.NewHub(cfg.DatabaseURL, submissions.EventsChannel, log)
	go hub.Run(ctx)

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.New(httpapi.Deps{
			DB:            pool,
			Log:           log,
			Submissions:   subs,
			Events:        hub,
			Organizer:     organizer.Service{DB: pool},
			UI:            webui.FS(),
			StorageOrigin: cfg.Storage.Endpoint,
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
