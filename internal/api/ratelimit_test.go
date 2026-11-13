package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/CheeseBurger7/llm-usage-meter/internal/api"
	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ratelimit"
)

// newLimiter allows burst requests per key and then, for the length of a
// test, nothing: one token takes 100 seconds to refill.
func newLimiter(t *testing.T, burst int) *api.KeyLimiter {
	t.Helper()
	l, err := ratelimit.New[auth.KeyID](0.01, burst)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func send(h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRateLimit(t *testing.T) {
	keys := fakeKeys{
		"key-a":    {OrgID: testOrg, KeyID: auth.KeyID{1}, Scope: auth.ScopeIngest},
		"key-b":    {OrgID: testOrg, KeyID: auth.KeyID{2}, Scope: auth.ScopeIngest},
		"read-key": {OrgID: testOrg, KeyID: auth.KeyID{3}, Scope: auth.ScopeRead},
	}
	store := &fakeIngester{res: ingest.Created}
	h := api.NewRouter(store, keys, newLimiter(t, 2), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Rejected requests spend nothing: they never reach the limiter.
	for range 5 {
		if rec := send(h, http.MethodPost, "/v1/events", "", validBody); rec.Code != http.StatusUnauthorized {
			t.Fatalf("no key: status = %d, want 401", rec.Code)
		}
		if rec := send(h, http.MethodPost, "/v1/events", "read-key", validBody); rec.Code != http.StatusForbidden {
			t.Fatalf("read key: status = %d, want 403", rec.Code)
		}
	}

	// The single and batch routes draw on the same bucket.
	if rec := send(h, http.MethodPost, "/v1/events", "key-a", validBody); rec.Code != http.StatusCreated {
		t.Fatalf("first request: status = %d, want 201", rec.Code)
	}
	if rec := send(h, http.MethodPost, "/v1/events/batch", "key-a", batchBody(validBody)); rec.Code != http.StatusOK {
		t.Fatalf("second request: status = %d, want 200", rec.Code)
	}
	calls := store.calls

	for _, path := range []string{"/v1/events", "/v1/events/batch"} {
		rec := send(h, http.MethodPost, path, "key-a", validBody)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("%s over the limit: status = %d, want 429", path, rec.Code)
		}
		secs, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		if err != nil || secs < 1 || secs > 100 {
			t.Fatalf("Retry-After = %q, want whole seconds between 1 and 100", rec.Header().Get("Retry-After"))
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type = %q", ct)
		}
		if !strings.Contains(rec.Body.String(), `"error":"rate limit exceeded`) {
			t.Fatalf("body = %s", rec.Body)
		}
	}
	if store.calls != calls {
		t.Fatalf("a limited request reached the store (%d calls, want %d)", store.calls, calls)
	}

	// Another key of the same org has its own bucket.
	if rec := send(h, http.MethodPost, "/v1/events", "key-b", validBody); rec.Code != http.StatusCreated {
		t.Fatalf("second key: status = %d, want 201", rec.Code)
	}
	// Routes without a key are not limited.
	if rec := send(h, http.MethodGet, "/healthz", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz: status = %d, want 200", rec.Code)
	}
}

func TestRetryAfterRoundsUp(t *testing.T) {
	// One token per 100 seconds. By the time the second request arrives the
	// wait is a little under 100s, and it must be reported as 100, not 99: a
	// client that waits the advertised time must not be refused again.
	keys := fakeKeys{"key-a": {OrgID: testOrg, KeyID: auth.KeyID{1}, Scope: auth.ScopeIngest}}
	h := api.NewRouter(&fakeIngester{res: ingest.Created}, keys, newLimiter(t, 1), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if rec := send(h, http.MethodPost, "/v1/events", "key-a", validBody); rec.Code != http.StatusCreated {
		t.Fatalf("first request: status = %d, want 201", rec.Code)
	}
	rec := send(h, http.MethodPost, "/v1/events", "key-a", validBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "100" {
		t.Fatalf("Retry-After = %q, want 100", got)
	}
}
