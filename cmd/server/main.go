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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CheeseBurger7/llm-usage-meter/internal/api"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
	"github.com/CheeseBurger7/llm-usage-meter/internal/rollup"
	"github.com/CheeseBurger7/llm-usage-meter/internal/storage"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	run := serve
	if len(os.Args) > 1 && os.Args[1] == "backfill" {
		run = backfill
	}
	if err := run(log); err != nil {
		log.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func serve(log *slog.Logger) error {
	dsn, err := databaseURL()
	if err != nil {
		return err
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := storage.Migrate(ctx, pool); err != nil {
		return err
	}

	// Fill hours missed while the service was down before taking over the schedule.
	roller := rollup.New(pool, log)
	n, err := roller.BackfillStale(ctx)
	if err != nil {
		return fmt.Errorf("startup backfill: %w", err)
	}
	log.Info("startup backfill complete", "hours", n)
	rollupDone := make(chan struct{})
	go func() {
		defer close(rollupDone)
		roller.Run(ctx, rollupInterval())
	}()

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewRouter(ingest.NewStore(pool), log),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		stop()
		<-rollupDone
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	<-rollupDone
	return err
}

// backfill recomputes the rollups for an explicit window:
//
//	server backfill --from 2026-01-01T00:00:00Z --to 2026-01-02T00:00:00Z
func backfill(log *slog.Logger) error {
	fs := flag.NewFlagSet("backfill", flag.ExitOnError)
	from := fs.String("from", "", "window start, RFC 3339 (widened down to the hour)")
	to := fs.String("to", "", "window end, RFC 3339 (exclusive, widened up to the hour)")
	_ = fs.Parse(os.Args[2:])

	start, err := time.Parse(time.RFC3339, *from)
	if err != nil {
		return fmt.Errorf("--from: %w", err)
	}
	end, err := time.Parse(time.RFC3339, *to)
	if err != nil {
		return fmt.Errorf("--to: %w", err)
	}
	if !end.After(start) {
		return errors.New("--to must be after --from")
	}
	dsn, err := databaseURL()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := storage.Migrate(ctx, pool); err != nil {
		return err
	}

	if err := rollup.New(pool, log).Rollup(ctx, start, end); err != nil {
		return err
	}
	log.Info("backfill complete", "from", start, "to", end)
	return nil
}

func databaseURL() (string, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return "", errors.New("DATABASE_URL is required")
	}
	return dsn, nil
}

func rollupInterval() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("ROLLUP_INTERVAL")); err == nil && d > 0 {
		return d
	}
	return time.Minute
}
