package api_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/rollup"
)

// isoEvent is one event of the isolation test. Every event falls in the same
// hour and has the same provider, model and (empty) feature, so org_id is the
// only thing that can keep the two orgs' rows apart.
func isoEvent(id, tenant string, input int) string {
	return fmt.Sprintf(`{"event_id":%q,"tenant_id":%q,"provider":"p","model":"m",`+
		`"input_tokens":%d,"output_tokens":1,"latency_ms":1,"timestamp":"2026-01-02T03:04:05Z"}`,
		id, tenant, input)
}

// TestTwoOrgIsolation is the milestone 1 acceptance test (docs/DESIGN.md,
// "Two orgs can ingest with their own keys and never see each other's rows").
//
// Two orgs send events through both ingest routes with their own keys, reusing
// the same event ids and tenant ids with different token counts, and the
// rollup runs. Every row of events and hourly_usage is then read back, so a
// row stored or summed under the wrong org shows up as a wrong id or a wrong
// total, not just a wrong count.
func TestTwoOrgIsolation(t *testing.T) {
	srv, keys, pool := startServer(t)
	ctx := context.Background()

	newOrg := func(name string) (auth.OrgID, auth.NewKey) {
		t.Helper()
		org, err := keys.CreateOrg(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		k, err := keys.CreateKey(ctx, org.ID, auth.ScopeIngest)
		if err != nil {
			t.Fatal(err)
		}
		return org.ID, k
	}
	acme, acmeKey := newOrg("Acme")
	globex, globexKey := newOrg("Globex")

	send := func(path, key, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	mustSend := func(path, key, body string, want int) {
		t.Helper()
		if got := send(path, key, body); got != want {
			t.Fatalf("POST %s: status = %d, want %d", path, got, want)
		}
	}

	// Both orgs use event ids e1 to e3 and the tenant "shared". Acme's token
	// counts are 1s and 10s, Globex's are 1000s, so a sum that mixes the orgs
	// cannot equal either expected total.
	mustSend("/v1/events", acmeKey.Plaintext, isoEvent("e1", "shared", 1), http.StatusCreated)
	mustSend("/v1/events/batch", acmeKey.Plaintext, batchBody(
		isoEvent("e2", "shared", 2),
		isoEvent("e3", "acme-only", 10),
	), http.StatusOK)
	mustSend("/v1/events", globexKey.Plaintext, isoEvent("e1", "shared", 1000), http.StatusCreated)
	mustSend("/v1/events/batch", globexKey.Plaintext, batchBody(
		isoEvent("e2", "globex-only", 2000),
		isoEvent("e3", "shared", 4000),
	), http.StatusOK)

	roller := rollup.New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runRollup := func(wantHours int) {
		t.Helper()
		n, err := roller.BackfillStale(ctx)
		if err != nil {
			t.Fatalf("rollup: %v", err)
		}
		if n != wantHours {
			t.Fatalf("rollup processed %d hours, want %d", n, wantHours)
		}
	}
	// All six events share one hour, whichever org made it stale.
	runRollup(1)

	wantEvents := map[auth.OrgID][]string{
		acme:   {"e1 shared 1", "e2 shared 2", "e3 acme-only 10"},
		globex: {"e1 shared 1000", "e2 globex-only 2000", "e3 shared 4000"},
	}
	wantUsage := map[auth.OrgID][]string{
		acme:   {"acme-only count=1 input=10", "shared count=2 input=3"},
		globex: {"globex-only count=1 input=2000", "shared count=2 input=5000"},
	}
	check := func(when string) {
		t.Helper()
		gotEvents := rowsByOrg(t, pool,
			`SELECT org_id, event_id || ' ' || tenant_id || ' ' || input_tokens
			 FROM events ORDER BY event_id`)
		if !reflect.DeepEqual(gotEvents, wantEvents) {
			t.Fatalf("%s: events\n got %v\nwant %v", when, gotEvents, wantEvents)
		}
		gotUsage := rowsByOrg(t, pool,
			`SELECT org_id, tenant_id || ' count=' || event_count || ' input=' || input_tokens
			 FROM hourly_usage ORDER BY tenant_id`)
		if !reflect.DeepEqual(gotUsage, wantUsage) {
			t.Fatalf("%s: hourly_usage\n got %v\nwant %v", when, gotUsage, wantUsage)
		}
	}
	check("after ingest and rollup")

	// A read key belongs to the org but may not ingest, on either route.
	readKey, err := keys.CreateKey(ctx, acme, auth.ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	mustSend("/v1/events", readKey.Plaintext, isoEvent("r1", "shared", 7), http.StatusForbidden)
	mustSend("/v1/events/batch", readKey.Plaintext, batchBody(isoEvent("r2", "shared", 7)), http.StatusForbidden)

	// Revoking Globex's key stops Globex on both routes and leaves Acme's key
	// working: Acme re-sending e1 is still a duplicate of its own event, not
	// a conflict with Globex's e1.
	if n, err := keys.RevokeByPrefix(ctx, globexKey.Prefix); err != nil || n != 1 {
		t.Fatalf("revoke: %d keys, %v; want 1", n, err)
	}
	mustSend("/v1/events", globexKey.Plaintext, isoEvent("x1", "shared", 7), http.StatusUnauthorized)
	mustSend("/v1/events/batch", globexKey.Plaintext, batchBody(isoEvent("x2", "shared", 7)), http.StatusUnauthorized)
	mustSend("/v1/events", acmeKey.Plaintext, isoEvent("e1", "shared", 1), http.StatusOK)

	// Nothing above was stored, so no hour is stale and every row is unchanged.
	runRollup(0)
	check("after rejected requests")

	// A forced recompute of the hour yields the same rows for both orgs.
	hour := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	if err := roller.Rollup(ctx, hour, hour.Add(time.Hour)); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	check("after recompute")
}

// rowsByOrg runs a query that returns (org_id, text) and groups the text by
// org, in query order. Rows of an org the test did not create would appear
// under their own key and fail the comparison.
func rowsByOrg(t *testing.T, pool *pgxpool.Pool, query string) map[auth.OrgID][]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[auth.OrgID][]string{}
	for rows.Next() {
		var org [16]byte
		var s string
		if err := rows.Scan(&org, &s); err != nil {
			t.Fatal(err)
		}
		out[org] = append(out[org], s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
