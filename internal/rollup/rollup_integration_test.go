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
	Count, In, Out, LatSum, LatMax, CacheRead, CacheWrite int64
}

// bucket is the hourly_usage key without the hour.
type bucket struct {
	Org                              auth.OrgID
	Tenant, Provider, Model, Feature string
}

func usageAt(t *testing.T, pool *pgxpool.Pool, b bucket, hour time.Time) (usage, bool) {
	t.Helper()
	var u usage
	err := pool.QueryRow(context.Background(),
		`SELECT event_count, input_tokens, output_tokens, latency_ms_sum, latency_ms_max,
		        cache_read_tokens, cache_write_tokens
		 FROM hourly_usage
		 WHERE org_id=$1 AND tenant_id=$2 AND provider=$3 AND model=$4 AND feature=$5 AND hour_start=$6`,
		b.Org, b.Tenant, b.Provider, b.Model, b.Feature, hour).
		Scan(&u.Count, &u.In, &u.Out, &u.LatSum, &u.LatMax, &u.CacheRead, &u.CacheWrite)
	return u, err == nil
}

// usageFor reads the bucket that addEvent writes to: provider "anthropic", no feature.
func usageFor(t *testing.T, pool *pgxpool.Pool, s orgStore, tenant, model string, hour time.Time) (usage, bool) {
	t.Helper()
	return usageAt(t, pool, bucket{Org: s.org, Tenant: tenant, Provider: "anthropic", Model: model}, hour)
}

func rowCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM hourly_usage`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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
		EventID: id, TenantID: tenant, Provider: "anthropic", Model: model,
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
		if got, _ := usageFor(t, pool, store, "a", "m1", h10); got != (usage{2, 110, 55, 600, 400, 0, 0}) {
			t.Fatalf("run %d: tenant a hour 10 = %+v", i, got)
		}
		if got, _ := usageFor(t, pool, store, "a", "m1", h11); got != (usage{1, 1, 1, 10, 10, 0, 0}) {
			t.Fatalf("run %d: tenant a hour 11 = %+v", i, got)
		}
		if got, _ := usageFor(t, pool, store, "b", "m1", h10); got != (usage{1, 7, 3, 30, 30, 0, 0}) {
			t.Fatalf("run %d: tenant b hour 10 = %+v", i, got)
		}
	}

	addEvent(t, store, "late", "a", "m1", h10.Add(30*time.Minute), 1000, 0, 100)
	if err := roller.Rollup(ctx, h10, h11); err != nil {
		t.Fatal(err)
	}
	if got, _ := usageFor(t, pool, store, "a", "m1", h10); got != (usage{3, 1110, 55, 700, 400, 0, 0}) {
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
	if got, _ := usageFor(t, pool, store, "a", "m1", base.Add(2*time.Hour)); got.Count != 2 {
		t.Fatalf("hour 2 count = %d, want 2", got.Count)
	}

	// A missed window is repaired by deleting its rows and re-rolling it.
	if _, err := pool.Exec(ctx, `DELETE FROM hourly_usage WHERE hour_start >= $1`, base.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := roller.Rollup(ctx, base.Add(3*time.Hour), base.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, ok := usageFor(t, pool, store, "a", "m1", base.Add(4*time.Hour)); !ok {
		t.Fatal("hour 4 not restored by explicit window rollup")
	}
}

// Two orgs that use the same tenant_id and model must not share a bucket, and
// provider and feature each split a bucket. Cache tokens are summed per bucket.
func TestRollupKeepsOrgsProvidersAndFeaturesApart(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	acme, globex := newOrgStore(t, pool), newOrgStore(t, pool)
	roller := rollup.New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))

	h := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	add := func(s orgStore, id, provider, feature string, in, cacheRead, cacheWrite int64) {
		t.Helper()
		_, err := s.Ingest(ctx, s.org, ingest.Event{
			EventID: id, TenantID: "t", Provider: provider, Model: "m", Feature: feature,
			InputTokens: in, OutputTokens: 1, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
			LatencyMs: 10, Timestamp: h.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("ingest %s: %v", id, err)
		}
	}
	// The same event_id, tenant, provider, model and feature in both orgs.
	add(acme, "e1", "anthropic", "chat", 100, 30, 4)
	add(acme, "e2", "anthropic", "chat", 10, 3, 1)
	add(globex, "e1", "anthropic", "chat", 7, 0, 0)
	// Same org and tenant, different provider, different feature, and no feature.
	add(acme, "e3", "openai", "chat", 20, 5, 0)
	add(acme, "e4", "anthropic", "search", 40, 0, 9)
	add(acme, "e5", "anthropic", "", 50, 1, 1)

	want := map[bucket]usage{
		{acme.org, "t", "anthropic", "m", "chat"}:   {2, 110, 2, 20, 10, 33, 5},
		{globex.org, "t", "anthropic", "m", "chat"}: {1, 7, 1, 10, 10, 0, 0},
		{acme.org, "t", "openai", "m", "chat"}:      {1, 20, 1, 10, 10, 5, 0},
		{acme.org, "t", "anthropic", "m", "search"}: {1, 40, 1, 10, 10, 0, 9},
		{acme.org, "t", "anthropic", "m", ""}:       {1, 50, 1, 10, 10, 1, 1},
	}
	for i := 0; i < 2; i++ { // second run must change nothing
		if err := roller.Rollup(ctx, h, h.Add(time.Hour)); err != nil {
			t.Fatalf("rollup run %d: %v", i, err)
		}
		if n := rowCount(t, pool); n != len(want) {
			t.Fatalf("run %d: hourly_usage has %d rows, want %d", i, n, len(want))
		}
		for b, w := range want {
			if got, ok := usageAt(t, pool, b, h); !ok || got != w {
				t.Fatalf("run %d: bucket %+v = %+v (found %v), want %+v", i, b, got, ok, w)
			}
		}
	}
}

// A late event in one org makes the hour stale even though the other org's
// bucket for the same tenant and model is up to date, and re-rolling the hour
// leaves the other org's numbers unchanged.
func TestBackfillStaleSeesEachOrgSeparately(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)
	acme, globex := newOrgStore(t, pool), newOrgStore(t, pool)
	roller := rollup.New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))

	h := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	addEvent(t, acme, "e1", "t", "m", h.Add(time.Minute), 10, 1, 5)
	if n, err := roller.BackfillStale(ctx); err != nil || n != 1 {
		t.Fatalf("first backfill = %d, %v; want 1 hour", n, err)
	}

	// Globex has no rollup row yet for a bucket that Acme already has.
	addEvent(t, globex, "e1", "t", "m", h.Add(2*time.Minute), 7, 1, 5)
	if n, err := roller.BackfillStale(ctx); err != nil || n != 1 {
		t.Fatalf("backfill after the second org's event = %d, %v; want 1 hour", n, err)
	}
	if got, _ := usageFor(t, pool, acme, "t", "m", h); got != (usage{1, 10, 1, 5, 5, 0, 0}) {
		t.Fatalf("acme = %+v", got)
	}
	if got, _ := usageFor(t, pool, globex, "t", "m", h); got != (usage{1, 7, 1, 5, 5, 0, 0}) {
		t.Fatalf("globex = %+v", got)
	}
	if n, err := roller.BackfillStale(ctx); err != nil || n != 0 {
		t.Fatalf("third backfill = %d, %v; want nothing stale", n, err)
	}
}
