package storage

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// newMigrationDB starts an empty Postgres and returns a pool and a goose
// provider, so a test can stop at any schema version. The test is skipped when
// Docker is not available.
func newMigrationDB(t *testing.T) (*pgxpool.Pool, *goose.Provider) {
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

	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationsFS())
	if err != nil {
		t.Fatal(err)
	}
	return pool, provider
}

// Migration 4 makes events org-scoped. A database that already holds events
// from before orgs existed must still upgrade: those rows cannot be attributed
// to an org, so the migration removes them and the rollups derived from them.
func TestEventsOrgScopeMigrationOnExistingData(t *testing.T) {
	ctx := context.Background()
	pool, provider := newMigrationDB(t)

	count := func(table string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// The schema as it was before this migration, with one event and its rollup.
	if _, err := provider.UpTo(ctx, 3); err != nil {
		t.Fatalf("migrate to 3: %v", err)
	}
	exec(`INSERT INTO events (event_id, tenant_id, model, input_tokens, output_tokens, latency_ms, event_time)
	      VALUES ('old', 't', 'm', 1, 1, 1, now())`)
	exec(`INSERT INTO hourly_usage (tenant_id, model, hour_start, event_count, input_tokens, output_tokens,
	                                latency_ms_sum, latency_ms_max, source_ingested_at)
	      VALUES ('t', 'm', date_trunc('hour', now()), 1, 1, 1, 1, 1, now())`)

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up over existing rows: %v", err)
	}
	if e, h := count("events"), count("hourly_usage"); e != 0 || h != 0 {
		t.Fatalf("after upgrade: events = %d, hourly_usage = %d; want both 0", e, h)
	}

	// The new key: one event_id, two orgs.
	var acme, globex string
	if err := pool.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ('Acme') RETURNING id::text`).Scan(&acme); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ('Globex') RETURNING id::text`).Scan(&globex); err != nil {
		t.Fatal(err)
	}
	const insert = `INSERT INTO events (org_id, event_id, tenant_id, provider, model, input_tokens, output_tokens, latency_ms, event_time)
	                VALUES ($1, 'same', 't', 'p', 'm', 1, 1, 1, now())`
	exec(insert, acme)
	exec(insert, globex)
	if _, err := pool.Exec(ctx, insert, acme); err == nil {
		t.Fatal("the same event_id was stored twice in one org")
	}

	// Down and up again, so the Down section is exercised too.
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("migrate down to 3: %v", err)
	}
	exec(`INSERT INTO events (event_id, tenant_id, model, input_tokens, output_tokens, latency_ms, event_time)
	      VALUES ('old-again', 't', 'm', 1, 1, 1, now())`)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up a second time: %v", err)
	}
}

// Migration 5 adds provider, the cache token counts and feature. Unlike
// migration 4 it keeps existing events: they get provider 'unknown', zero cache
// tokens and no feature. New rows must name a provider.
func TestEventsUsageFieldsMigrationOnExistingData(t *testing.T) {
	ctx := context.Background()
	pool, provider := newMigrationDB(t)

	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatalf("migrate to 4: %v", err)
	}
	var org string
	if err := pool.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ('Acme') RETURNING id::text`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	const insertOld = `INSERT INTO events (org_id, event_id, tenant_id, model, input_tokens, output_tokens, latency_ms, event_time)
	                   VALUES ($1, $2, 't', 'm', 7, 1, 1, now())`
	if _, err := pool.Exec(ctx, insertOld, org, "old"); err != nil {
		t.Fatal(err)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up over existing rows: %v", err)
	}

	var (
		prov, feature      string
		input, read, write int64
	)
	err := pool.QueryRow(ctx, `SELECT provider, feature, input_tokens, cache_read_tokens, cache_write_tokens
	                           FROM events WHERE org_id = $1 AND event_id = 'old'`, org).
		Scan(&prov, &feature, &input, &read, &write)
	if err != nil {
		t.Fatalf("existing event after upgrade: %v", err)
	}
	if prov != "unknown" || feature != "" || input != 7 || read != 0 || write != 0 {
		t.Fatalf("existing event = provider %q, feature %q, input %d, cache %d/%d; want unknown, empty, 7, 0/0",
			prov, feature, input, read, write)
	}

	// The 'unknown' default was only for the backfill.
	if _, err := pool.Exec(ctx, insertOld, org, "no-provider"); err == nil {
		t.Fatal("an event without a provider was stored")
	}
	const insertNew = `INSERT INTO events (org_id, event_id, tenant_id, provider, model, input_tokens, output_tokens,
	                                       cache_read_tokens, cache_write_tokens, latency_ms, event_time)
	                   VALUES ($1, $2, 't', 'p', 'm', 1, 1, $3, $4, 1, now())`
	if _, err := pool.Exec(ctx, insertNew, org, "new", 3, 2); err != nil {
		t.Fatalf("insert with the new columns: %v", err)
	}
	if _, err := pool.Exec(ctx, insertNew, org, "neg-read", -1, 0); err == nil {
		t.Fatal("negative cache_read_tokens was stored")
	}
	if _, err := pool.Exec(ctx, insertNew, org, "neg-write", 0, -1); err == nil {
		t.Fatal("negative cache_write_tokens was stored")
	}

	// Down and up again, so the Down section is exercised too. Down keeps the rows.
	if _, err := provider.DownTo(ctx, 4); err != nil {
		t.Fatalf("migrate down to 4: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up a second time: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE provider = 'unknown'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("events with provider 'unknown' after down and up = %d; want 2", n)
	}
}

// Migration 6 makes hourly_usage per org and adds provider, feature and the
// cache token sums. Existing rollup rows cannot be attributed to an org, so they
// are removed; the events they were computed from are kept, so the rows can be
// rebuilt.
func TestHourlyUsageOrgDimensionsMigrationOnExistingData(t *testing.T) {
	ctx := context.Background()
	pool, provider := newMigrationDB(t)

	count := func(table string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, err := provider.UpTo(ctx, 5); err != nil {
		t.Fatalf("migrate to 5: %v", err)
	}
	var acme, globex string
	if err := pool.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ('Acme') RETURNING id::text`).Scan(&acme); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO orgs (name) VALUES ('Globex') RETURNING id::text`).Scan(&globex); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (org_id, event_id, tenant_id, provider, model, input_tokens, output_tokens, latency_ms, event_time)
	                             VALUES ($1, 'e', 't', 'p', 'm', 1, 1, 1, now())`, acme); err != nil {
		t.Fatal(err)
	}
	const insertOld = `INSERT INTO hourly_usage (tenant_id, model, hour_start, event_count, input_tokens, output_tokens,
	                                             latency_ms_sum, latency_ms_max, source_ingested_at)
	                   VALUES ('t', 'm', date_trunc('hour', now()), 1, 1, 1, 1, 1, now())`
	if _, err := pool.Exec(ctx, insertOld); err != nil {
		t.Fatal(err)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up over existing rows: %v", err)
	}
	if e, h := count("events"), count("hourly_usage"); e != 1 || h != 0 {
		t.Fatalf("after upgrade: events = %d, hourly_usage = %d; want 1 and 0", e, h)
	}

	// The new key: the same tenant, model and hour in two orgs, and under a
	// second provider and a second feature in one org.
	const insertNew = `INSERT INTO hourly_usage (org_id, tenant_id, provider, model, feature, hour_start, event_count,
	                                             input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
	                                             latency_ms_sum, latency_ms_max, source_ingested_at)
	                   VALUES ($1, 't', $2, 'm', $3, '2026-01-01T00:00:00Z', 1, 1, 1, 0, 0, 1, 1, now())`
	for _, row := range [][]any{
		{acme, "p", ""}, {globex, "p", ""}, {acme, "p2", ""}, {acme, "p", "chat"},
	} {
		if _, err := pool.Exec(ctx, insertNew, row...); err != nil {
			t.Fatalf("insert %v: %v", row, err)
		}
	}
	if _, err := pool.Exec(ctx, insertNew, acme, "p", ""); err == nil {
		t.Fatal("the same bucket was stored twice")
	}
	if _, err := pool.Exec(ctx, insertOld); err == nil {
		t.Fatal("a rollup row without an org was stored")
	}

	// Down and up again, so the Down section is exercised too. Events survive both.
	if _, err := provider.DownTo(ctx, 5); err != nil {
		t.Fatalf("migrate down to 5: %v", err)
	}
	if _, err := pool.Exec(ctx, insertOld); err != nil {
		t.Fatalf("insert with the old columns after down: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("migrate up a second time: %v", err)
	}
	if e, h := count("events"), count("hourly_usage"); e != 1 || h != 0 {
		t.Fatalf("after down and up: events = %d, hourly_usage = %d; want 1 and 0", e, h)
	}
}
