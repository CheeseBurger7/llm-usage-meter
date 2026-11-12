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

// Migration 4 makes events org-scoped. A database that already holds events
// from before orgs existed must still upgrade: those rows cannot be attributed
// to an org, so the migration removes them and the rollups derived from them.
func TestEventsOrgScopeMigrationOnExistingData(t *testing.T) {
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
	const insert = `INSERT INTO events (org_id, event_id, tenant_id, model, input_tokens, output_tokens, latency_ms, event_time)
	                VALUES ($1, 'same', 't', 'm', 1, 1, 1, now())`
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
