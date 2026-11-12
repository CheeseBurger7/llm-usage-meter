package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/CheeseBurger7/llm-usage-meter/internal/api"
	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
)

// fakeKeys knows a fixed set of keys.
type fakeKeys map[string]auth.Identity

func (f fakeKeys) Verify(_ context.Context, key string) (auth.Identity, error) {
	id, ok := f[key]
	if !ok {
		return auth.Identity{}, auth.ErrInvalidKey
	}
	return id, nil
}

func TestRouterAuth(t *testing.T) {
	keys := fakeKeys{
		"ingest-key": {OrgID: auth.OrgID{1}, Scope: auth.ScopeIngest},
		"read-key":   {OrgID: auth.OrgID{1}, Scope: auth.ScopeRead},
	}
	tests := []struct {
		name, method, path, key string
		want                    int
		wantIngest              bool
	}{
		{"healthz needs no key", http.MethodGet, "/healthz", "", http.StatusOK, false},
		{"events without a key", http.MethodPost, "/v1/events", "", http.StatusUnauthorized, false},
		{"events with an unknown key", http.MethodPost, "/v1/events", "nope", http.StatusUnauthorized, false},
		{"events with a read key", http.MethodPost, "/v1/events", "read-key", http.StatusForbidden, false},
		{"events with an ingest key", http.MethodPost, "/v1/events", "ingest-key", http.StatusCreated, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &countingIngester{}
			h := api.NewRouter(store, keys, slog.New(slog.NewTextHandler(io.Discard, nil)))
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(validBody))
			if tt.key != "" {
				req.Header.Set("Authorization", "Bearer "+tt.key)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			// A rejected request must not reach the store.
			if (store.calls > 0) != tt.wantIngest {
				t.Fatalf("store calls = %d, want ingest = %v", store.calls, tt.wantIngest)
			}
		})
	}
}

type countingIngester struct{ calls int }

func (c *countingIngester) Ingest(context.Context, ingest.Event) (ingest.Result, error) {
	c.calls++
	return ingest.Created, nil
}
