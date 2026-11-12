package rollup_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
	"github.com/CheeseBurger7/llm-usage-meter/internal/rollup"
	"github.com/CheeseBurger7/llm-usage-meter/internal/storage"
)

// newPool starts a throwaway Postgres and applies migrations. The test is
// skipped when Docker is not available.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("meter"),
		postgres.WithUsername("meter"),
		postgres.WithPassword("meter"),
		// Postgres restarts once during init; this waits for the second "ready"
		// log line and the port, so the first connection cannot hit the restart.
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := storage.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

type usage struct {
	Count, In, Out, LatSum, LatMax int64
}

func usageFor(t *testing.T, pool *pgxpool.Pool, tenant, model string, hour time.Time) (usage, bool) {
	t.Helper()
	var u usage
	err := pool.QueryRow(context.Background(),
		`SELECT event_count, input_tokens, output_tokens, latency_ms_sum, latency_ms_max
		 FROM hourly_usage WHERE tenant_id=$1 AND model=$2 AND hour_start=$3`,
		tenant, model, hour).Scan(&u.Count, &u.In, &u.Out, &u.LatSum, &u.LatMax)
	return u, err == nil
}

// newOrg creates the org that a test's events are stored under.
func newOrg(t *testing.T, pool *pgxpool.Pool) auth.OrgID {
	t.Helper()
	org, err := auth.New(pool).CreateOrg(context.Background(), "Acme")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return org.ID
}

// orgStore ingests every event under one org.
type orgStore struct {
	*ingest.Store
	org auth.OrgID
}

func newOrgStore(t *testing.T, pool *pgxpool.Pool) orgStore {
	return orgStore{Store: ingest.NewStore(pool), org: newOrg(t, pool)}
}

func addEvent(t *testing.T, s orgStore, id, tenant, model string, at time.Time, in, out, lat int64) {
	t.Helper()
	_, err := s.Ingest(context.Background(), s.org, ingest.Event{
		EventID: id, TenantID: tenant, Model: model,
		InputTokens: in, OutputTokens: out, LatencyMs: lat, Timestamp: at,
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", id, err)
	}
}

func TestRollupIsIdempotentAndPicksUpLateEvents(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	store := newOrgStore(t, pool)
	roller := rollup.New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))

	h10 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	h11 := h10.Add(time.Hour)
	addEvent(t, store, "e1", "a", "m1", h10.Add(5*time.Minute), 100, 50, 200)
	addEvent(t, store, "e2", "a", "m1", h10.Add(59*time.Minute), 10, 5, 400)
	addEvent(t, store, "e3", "a", "m1", h11, 1, 1, 10) // boundary belongs to the next hour
	addEvent(t, store, "e4", "b", "m1", h10.Add(time.Minute), 7, 3, 30)

	for i := 0; i < 2; i++ { // second run must change nothing
		if err := roller.Rollup(ctx, h10, h11.Add(time.Hour)); err != nil {
			t.Fatalf("rollup run %d: %v", i, err)
		}
		if got, _ := usageFor(t, pool, "a", "m1", h10); got != (usage{2, 110, 55, 600, 400}) {
			t.Fatalf("run %d: tenant a hour 10 = %+v", i, got)
		}
		if got, _ := usageFor(t, pool, "a", "m1", h11); got != (usage{1, 1, 1, 10, 10}) {
			t.Fatalf("run %d: tenant a hour 11 = %+v", i, got)
		}
		if got, _ := usageFor(t, pool, "b", "m1", h10); got != (usage{1, 7, 3, 30, 30}) {
			t.Fatalf("run %d: tenant b hour 10 = %+v", i, got)
		}
	}

	addEvent(t, store, "late", "a", "m1", h10.Add(30*time.Minute), 1000, 0, 100)
	if err := roller.Rollup(ctx, h10, h11); err != nil {
		t.Fatal(err)
	}
	if got, _ := usageFor(t, pool, "a", "m1", h10); got != (usage{3, 1110, 55, 700, 400}) {
		t.Fatalf("after late event: %+v", got)
	}
}

func TestBackfillStaleFillsMissingAndOutdatedHours(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	store := newOrgStore(t, pool)
	roller := rollup.New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for h := 0; h < 5; h++ {
		addEvent(t, store, fmt.Sprintf("e%d", h), "a", "m1", base.Add(time.Duration(h)*time.Hour+time.Minute), 10, 10, 10)
	}

	n, err := roller.BackfillStale(ctx)
	if err != nil || n != 5 {
		t.Fatalf("first backfill = %d, %v; want 5 hours", n, err)
	}
	if n, err := roller.BackfillStale(ctx); err != nil || n != 0 {
		t.Fatalf("second backfill = %d, %v; want nothing stale", n, err)
	}

	// A late event into an already rolled-up hour makes only that hour stale.
	addEvent(t, store, "late", "a", "m1", base.Add(2*time.Hour+30*time.Minute), 5, 5, 5)
	n, err = roller.BackfillStale(ctx)
	if err != nil || n != 1 {
		t.Fatalf("backfill after late event = %d, %v; want 1", n, err)
	}
	if got, _ := usageFor(t, pool, "a", "m1", base.Add(2*time.Hour)); got.Count != 2 {
		t.Fatalf("hour 2 count = %d, want 2", got.Count)
	}

	// A missed window is repaired by deleting its rows and re-rolling it.
	if _, err := pool.Exec(ctx, `DELETE FROM hourly_usage WHERE hour_start >= $1`, base.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := roller.Rollup(ctx, base.Add(3*time.Hour), base.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := usageFor(t, pool, "a", "m1", base.Add(4*time.Hour)); !ok {
		t.Fatal("hour 4 not restored by explicit window rollup")
	}
}
