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

	"github.com/CheeseBurger7/llm-usage-meter/internal/api"
	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
	"github.com/CheeseBurger7/llm-usage-meter/internal/storage"
)

// startServer runs the router, with no rate limit, against a migrated
// Postgres container.
func startServer(t *testing.T) (*httptest.Server, *auth.Service, *pgxpool.Pool) {
	t.Helper()
	return startServerWithLimit(t, nil)
}

// startServerWithLimit is startServer with a rate limit on the ingest routes.
func startServerWithLimit(t *testing.T, limit *api.KeyLimiter) (*httptest.Server, *auth.Service, *pgxpool.Pool) {
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

	keys := auth.New(pool)
	srv := httptest.NewServer(api.NewRouter(ingest.NewStore(pool), keys, limit, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)
	return srv, keys, pool
}

func TestIngestEndpointIdempotency(t *testing.T) {
	srv, keys, pool := startServer(t)
	ctx := context.Background()

	org, err := keys.CreateOrg(ctx, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	newKey := func(scope auth.Scope) auth.NewKey {
		t.Helper()
		k, err := keys.CreateKey(ctx, org.ID, scope)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	ingestKey, readKey, revokedKey := newKey(auth.ScopeIngest), newKey(auth.ScopeRead), newKey(auth.ScopeIngest)
	if _, err := keys.RevokeByPrefix(ctx, revokedKey.Prefix); err != nil {
		t.Fatal(err)
	}

	send := func(key, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/events", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
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
		name, key, body string
		want            int
	}{
		// Rejected requests come first: none of them may store the event, or
		// "first send" below would not be 201.
		{"no key", "", validBody, http.StatusUnauthorized},
		{"malformed key", "not-a-key", validBody, http.StatusUnauthorized},
		// Well-formed, so it gets as far as the database lookup.
		{"unknown key", "lum_ingest_" + strings.Repeat("0", 64), validBody, http.StatusUnauthorized},
		{"revoked key", revokedKey.Plaintext, validBody, http.StatusUnauthorized},
		{"read key", readKey.Plaintext, validBody, http.StatusForbidden},
		{"first send", ingestKey.Plaintext, validBody, http.StatusCreated},
		{"retry", ingestKey.Plaintext, validBody, http.StatusOK},
		{"changed payload", ingestKey.Plaintext, changed, http.StatusConflict},
		{"invalid", ingestKey.Plaintext, `{"event_id":"e2"}`, http.StatusBadRequest},
	} {
		if got := send(step.key, step.body); got != step.want {
			t.Fatalf("%s: status = %d, want %d", step.name, got, step.want)
		}
	}

	// A second org reuses the event_id with a different payload. In the first
	// org that was a 409; here it is a new event, stored under the key's org.
	globex, err := keys.CreateOrg(ctx, "Globex")
	if err != nil {
		t.Fatal(err)
	}
	globexKey, err := keys.CreateKey(ctx, globex.ID, auth.ScopeIngest)
	if err != nil {
		t.Fatal(err)
	}
	if got := send(globexKey.Plaintext, changed); got != http.StatusCreated {
		t.Fatalf("second org, same event_id: status = %d, want 201", got)
	}
	if got := send(globexKey.Plaintext, changed); got != http.StatusOK {
		t.Fatalf("second org retry: status = %d, want 200", got)
	}
	for _, want := range []struct {
		org   auth.OrgID
		input int64
	}{{org.ID, 10}, {globex.ID, 11}} {
		var input int64
		err := pool.QueryRow(ctx, `SELECT input_tokens FROM events WHERE org_id = $1 AND event_id = 'e1'`,
			want.org.String()).Scan(&input)
		if err != nil || input != want.input {
			t.Fatalf("org %s: input_tokens = %d, %v; want %d", want.org, input, err, want.input)
		}
	}

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz without a key: status = %d, want 200", resp.StatusCode)
	}
}
