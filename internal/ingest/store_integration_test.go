package ingest_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
	"github.com/CheeseBurger7/llm-usage-meter/internal/storage"
)

// newStore starts a throwaway Postgres, applies migrations and returns a Store,
// an org to ingest under, and the pool for direct assertions. The test is
// skipped when Docker is not available.
func newStore(t *testing.T) (*ingest.Store, auth.OrgID, *pgxpool.Pool) {
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
	return ingest.NewStore(pool), newOrg(t, pool, "Acme"), pool
}

func newOrg(t *testing.T, pool *pgxpool.Pool, name string) auth.OrgID {
	t.Helper()
	org, err := auth.New(pool).CreateOrg(context.Background(), name)
	if err != nil {
		t.Fatalf("create org %s: %v", name, err)
	}
	return org.ID
}

func event() ingest.Event {
	return ingest.Event{
		EventID:      "evt-1",
		TenantID:     "tenant-a",
		Model:        "claude-sonnet",
		InputTokens:  100,
		OutputTokens: 50,
		LatencyMs:    420,
		Timestamp:    time.Date(2026, 11, 1, 12, 0, 0, 123456789, time.UTC),
	}
}

func TestIngestCreatedThenDuplicate(t *testing.T) {
	s, org, _ := newStore(t)
	ctx := context.Background()

	if got, err := s.Ingest(ctx, org, event()); err != nil || got != ingest.Created {
		t.Fatalf("first ingest = %v, %v; want Created", got, err)
	}
	if got, err := s.Ingest(ctx, org, event()); err != nil || got != ingest.Duplicate {
		t.Fatalf("second ingest = %v, %v; want Duplicate", got, err)
	}

	// Same instant expressed in another zone is still a duplicate.
	ev := event()
	ev.Timestamp = ev.Timestamp.In(time.FixedZone("x", 5*3600))
	if got, err := s.Ingest(ctx, org, ev); err != nil || got != ingest.Duplicate {
		t.Fatalf("zone-shifted ingest = %v, %v; want Duplicate", got, err)
	}
}

func TestIngestConflictOnAnyDifferingField(t *testing.T) {
	mutations := map[string]func(*ingest.Event){
		"tenant_id":     func(e *ingest.Event) { e.TenantID = "tenant-b" },
		"model":         func(e *ingest.Event) { e.Model = "other" },
		"input_tokens":  func(e *ingest.Event) { e.InputTokens++ },
		"output_tokens": func(e *ingest.Event) { e.OutputTokens++ },
		"latency_ms":    func(e *ingest.Event) { e.LatencyMs++ },
		"timestamp":     func(e *ingest.Event) { e.Timestamp = e.Timestamp.Add(time.Second) },
	}
	s, org, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.Ingest(ctx, org, event()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			ev := event()
			mutate(&ev)
			if _, err := s.Ingest(ctx, org, ev); !errors.Is(err, ingest.ErrConflict) {
				t.Fatalf("err = %v; want ErrConflict", err)
			}
		})
	}
}

func TestIngestRejectsInvalid(t *testing.T) {
	s, org, _ := newStore(t)
	ev := event()
	ev.TenantID = ""
	var verr *ingest.ValidationError
	if _, err := s.Ingest(context.Background(), org, ev); !errors.As(err, &verr) {
		t.Fatalf("err = %v; want *ValidationError", err)
	}
}

// The same event_id in two orgs is two separate events: neither insert, retry
// nor conflict check in one org can see the other org's row.
func TestIngestSameEventIDInTwoOrgs(t *testing.T) {
	s, acme, pool := newStore(t)
	globex := newOrg(t, pool, "Globex")
	ctx := context.Background()

	acmeEvent := event()
	// Same event_id, different payload. Within one org this would be a conflict.
	globexEvent := event()
	globexEvent.TenantID = "tenant-z"
	globexEvent.InputTokens = 7

	if got, err := s.Ingest(ctx, acme, acmeEvent); err != nil || got != ingest.Created {
		t.Fatalf("acme first ingest = %v, %v; want Created", got, err)
	}
	if got, err := s.Ingest(ctx, globex, globexEvent); err != nil || got != ingest.Created {
		t.Fatalf("globex first ingest = %v, %v; want Created", got, err)
	}

	// Each org's retry matches its own row.
	if got, err := s.Ingest(ctx, acme, acmeEvent); err != nil || got != ingest.Duplicate {
		t.Fatalf("acme retry = %v, %v; want Duplicate", got, err)
	}
	if got, err := s.Ingest(ctx, globex, globexEvent); err != nil || got != ingest.Duplicate {
		t.Fatalf("globex retry = %v, %v; want Duplicate", got, err)
	}

	// An org sending the other org's payload conflicts with its own row.
	if _, err := s.Ingest(ctx, acme, globexEvent); !errors.Is(err, ingest.ErrConflict) {
		t.Fatalf("acme sending globex payload: err = %v; want ErrConflict", err)
	}

	type row struct {
		tenant string
		input  int64
	}
	stored := map[string]row{}
	rows, err := pool.Query(ctx, `SELECT org_id::text, tenant_id, input_tokens FROM events WHERE event_id = $1`, acmeEvent.EventID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var org string
		var r row
		if err := rows.Scan(&org, &r.tenant, &r.input); err != nil {
			t.Fatal(err)
		}
		stored[org] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := map[string]row{
		acme.String():   {acmeEvent.TenantID, acmeEvent.InputTokens},
		globex.String(): {globexEvent.TenantID, globexEvent.InputTokens},
	}
	if len(stored) != len(want) || stored[acme.String()] != want[acme.String()] || stored[globex.String()] != want[globex.String()] {
		t.Fatalf("stored = %+v; want %+v", stored, want)
	}
}

// An event cannot be stored under an org that does not exist.
func TestIngestUnknownOrg(t *testing.T) {
	s, _, pool := newStore(t)
	ctx := context.Background()

	if _, err := s.Ingest(ctx, auth.OrgID{0xFF}, event()); err == nil {
		t.Fatal("ingest under an unknown org succeeded")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("events stored = %d; want 0", n)
	}
}

func TestIngestConcurrentSameEventID(t *testing.T) {
	s, org, _ := newStore(t)
	ctx := context.Background()

	const workers = 20
	results := make(chan ingest.Result, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Ingest(ctx, org, event())
			if err != nil {
				errs <- err
				return
			}
			results <- r
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("unexpected error: %v", err)
	}
	created := 0
	for r := range results {
		if r == ingest.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("Created count = %d; want exactly 1", created)
	}
}
