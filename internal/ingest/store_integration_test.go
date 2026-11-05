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
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
	"github.com/CheeseBurger7/llm-usage-meter/internal/storage"
)

// newStore starts a throwaway Postgres, applies migrations and returns a Store.
// The test is skipped when Docker is not available.
func newStore(t *testing.T) *ingest.Store {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("meter"),
		postgres.WithUsername("meter"),
		postgres.WithPassword("meter"),
		postgres.BasicWaitStrategies(),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
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
	return ingest.NewStore(pool)
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
	s := newStore(t)
	ctx := context.Background()

	if got, err := s.Ingest(ctx, event()); err != nil || got != ingest.Created {
		t.Fatalf("first ingest = %v, %v; want Created", got, err)
	}
	if got, err := s.Ingest(ctx, event()); err != nil || got != ingest.Duplicate {
		t.Fatalf("second ingest = %v, %v; want Duplicate", got, err)
	}

	// Same instant expressed in another zone is still a duplicate.
	ev := event()
	ev.Timestamp = ev.Timestamp.In(time.FixedZone("x", 5*3600))
	if got, err := s.Ingest(ctx, ev); err != nil || got != ingest.Duplicate {
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
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Ingest(ctx, event()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			ev := event()
			mutate(&ev)
			if _, err := s.Ingest(ctx, ev); !errors.Is(err, ingest.ErrConflict) {
				t.Fatalf("err = %v; want ErrConflict", err)
			}
		})
	}
}

func TestIngestRejectsInvalid(t *testing.T) {
	s := newStore(t)
	ev := event()
	ev.TenantID = ""
	var verr *ingest.ValidationError
	if _, err := s.Ingest(context.Background(), ev); !errors.As(err, &verr) {
		t.Fatalf("err = %v; want *ValidationError", err)
	}
}

func TestIngestConcurrentSameEventID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const workers = 20
	results := make(chan ingest.Result, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Ingest(ctx, event())
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
