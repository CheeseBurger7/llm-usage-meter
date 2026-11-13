package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CheeseBurger7/llm-usage-meter/internal/api"
	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
)

func TestBatchEndpoint(t *testing.T) {
	srv, keys, pool := startServer(t)
	ctx := context.Background()

	newOrgKey := func(name string, scope auth.Scope) (auth.OrgID, string) {
		t.Helper()
		org, err := keys.CreateOrg(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		k, err := keys.CreateKey(ctx, org.ID, scope)
		if err != nil {
			t.Fatal(err)
		}
		return org.ID, k.Plaintext
	}
	acme, acmeKey := newOrgKey("Acme", auth.ScopeIngest)
	globex, globexKey := newOrgKey("Globex", auth.ScopeIngest)
	readKey, err := keys.CreateKey(ctx, acme, auth.ScopeRead)
	if err != nil {
		t.Fatal(err)
	}

	send := func(path, key, body string) (int, []batchResult) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
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
		defer func() { _ = resp.Body.Close() }()
		var out struct {
			Results []batchResult `json:"results"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp.StatusCode, out.Results
	}
	statuses := func(rs []batchResult) string {
		s := make([]string, len(rs))
		for i, r := range rs {
			s[i] = r.Status
		}
		return strings.Join(s, ",")
	}
	countEvents := func(org auth.OrgID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE org_id = $1`, org.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Rejected batches store nothing.
	if code, _ := send("/v1/events/batch", "", batchBody(eventJSON("a"))); code != http.StatusUnauthorized {
		t.Fatalf("no key: status = %d, want 401", code)
	}
	if code, _ := send("/v1/events/batch", readKey.Plaintext, batchBody(eventJSON("a"))); code != http.StatusForbidden {
		t.Fatalf("read key: status = %d, want 403", code)
	}
	if n := countEvents(acme); n != 0 {
		t.Fatalf("rejected batches stored %d events", n)
	}

	// "pre" is stored through the single endpoint first, so the batch sees an
	// event that already exists.
	if code, _ := send("/v1/events", acmeKey, eventJSON("pre")); code != http.StatusCreated {
		t.Fatalf("single send: status = %d, want 201", code)
	}

	changed := func(id string) string {
		return strings.Replace(eventJSON(id), `"input_tokens":10`, `"input_tokens":11`, 1)
	}
	mixed := batchBody(
		eventJSON("a"),
		eventJSON("pre"),         // identical to the stored event
		changed("pre"),           // same id, different payload
		`{"event_id":"missing"}`, // fails validation
		`{"event_id":"typed","input_tokens":"ten"}`, // fails decoding
		eventJSON("a"), // repeated inside the batch
		changed("a"),   // repeated inside the batch, different payload
		eventJSON("b"), // still stored after the bad ones
	)
	code, results := send("/v1/events/batch", acmeKey, mixed)
	if code != http.StatusOK {
		t.Fatalf("mixed batch: status = %d, want 200", code)
	}
	if got, want := statuses(results), "created,duplicate,conflict,invalid,invalid,duplicate,conflict,created"; got != want {
		t.Fatalf("mixed batch:\n got %s\nwant %s", got, want)
	}
	if results[3].Fields["tenant_id"] != "required" {
		t.Fatalf("validation result has fields %v", results[3].Fields)
	}
	if n := countEvents(acme); n != 3 {
		t.Fatalf("acme has %d events, want 3 (pre, a, b)", n)
	}

	// Re-sending the whole batch is safe: nothing new is stored and the
	// conflicting payloads did not overwrite anything.
	code, results = send("/v1/events/batch", acmeKey, mixed)
	if got, want := statuses(results), "duplicate,duplicate,conflict,invalid,invalid,duplicate,conflict,duplicate"; code != http.StatusOK || got != want {
		t.Fatalf("re-sent batch: status %d\n got %s\nwant %s", code, got, want)
	}
	var input int64
	if err := pool.QueryRow(ctx, `SELECT input_tokens FROM events WHERE org_id = $1 AND event_id = 'a'`, acme.String()).Scan(&input); err != nil || input != 10 {
		t.Fatalf("event a: input_tokens = %d, %v; want 10", input, err)
	}

	// Another org's batch reuses the same ids: all new, stored under its own org.
	code, results = send("/v1/events/batch", globexKey, batchBody(changed("a"), changed("pre")))
	if got := statuses(results); code != http.StatusOK || got != "created,created" {
		t.Fatalf("second org: status %d, results %s", code, got)
	}
	if a, g := countEvents(acme), countEvents(globex); a != 3 || g != 2 {
		t.Fatalf("events: acme %d, globex %d; want 3 and 2", a, g)
	}

	// A full batch of 500 is stored completely.
	full := make([]string, api.MaxBatchEvents)
	for i := range full {
		full[i] = eventJSON(fmt.Sprintf("full-%d", i))
	}
	start := time.Now()
	code, results = send("/v1/events/batch", globexKey, batchBody(full...))
	t.Logf("batch of %d events took %s", api.MaxBatchEvents, time.Since(start))
	if code != http.StatusOK || len(results) != api.MaxBatchEvents {
		t.Fatalf("full batch: status %d, %d results", code, len(results))
	}
	for i, r := range results {
		if r.Status != "created" {
			t.Fatalf("full batch result %d = %+v", i, r)
		}
	}
	if g := countEvents(globex); g != 2+api.MaxBatchEvents {
		t.Fatalf("globex has %d events, want %d", g, 2+api.MaxBatchEvents)
	}
}
