package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/CheeseBurger7/llm-usage-meter/internal/api"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
	"github.com/CheeseBurger7/llm-usage-meter/internal/storage"
)

func TestIngestEndpointIdempotency(t *testing.T) {
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

	srv := httptest.NewServer(api.NewRouter(ingest.NewStore(pool), slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)

	send := func(body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+"/v1/events", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}

	changed := strings.Replace(validBody, `"input_tokens":10`, `"input_tokens":11`, 1)
	for _, step := range []struct {
		name, body string
		want       int
	}{
		{"first send", validBody, http.StatusCreated},
		{"retry", validBody, http.StatusOK},
		{"changed payload", changed, http.StatusConflict},
		{"invalid", `{"event_id":"e2"}`, http.StatusBadRequest},
	} {
		if got := send(step.body); got != step.want {
			t.Fatalf("%s: status = %d, want %d", step.name, got, step.want)
		}
	}
}
