package client_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/CheeseBurger7/llm-usage-meter/client"
	"github.com/CheeseBurger7/llm-usage-meter/internal/api"
	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
)

// memStore keeps events in memory behind the service's real router, auth
// middleware and batch handler, with the store's created/duplicate rule.
type memStore struct {
	mu     sync.Mutex
	events map[string]ingest.Event
}

func (m *memStore) Ingest(_ context.Context, _ auth.OrgID, ev ingest.Event) (ingest.Result, error) {
	if err := ev.Validate(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.events[ev.EventID]; ok {
		return ingest.Duplicate, nil
	}
	m.events[ev.EventID] = ev
	return ingest.Created, nil
}

type oneKey string

func (k oneKey) Verify(_ context.Context, key string) (auth.Identity, error) {
	if key != string(k) {
		return auth.Identity{}, auth.ErrInvalidKey
	}
	return auth.Identity{Scope: auth.ScopeIngest}, nil
}

// The client's wire format must be what the service decodes. The handler
// rejects unknown fields, so a renamed or added JSON field fails here.
func TestClientSpeaksTheServiceBatchFormat(t *testing.T) {
	store := &memStore{events: map[string]ingest.Event{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(api.NewRouter(store, oneKey("lum_ingest_k"), nil, log))
	defer srv.Close()

	c, err := client.New(srv.URL, "lum_ingest_k")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	full := client.Event{
		EventID: "full", TenantID: "tenant-a", Provider: "anthropic", Model: "claude-sonnet",
		Feature: "chat", InputTokens: 100, OutputTokens: 50, CacheReadTokens: 30,
		CacheWriteTokens: 20, LatencyMs: 420, Timestamp: when,
	}
	c.Record(full)
	c.Record(full) // an identical re-send is a duplicate, still delivered
	c.Record(client.Event{EventID: "minimal", TenantID: "t", Provider: "p", Model: "m"})
	c.Record(client.Event{EventID: "no-model", TenantID: "t", Provider: "p"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got, want := c.Stats(), (client.Stats{Sent: 3, Rejected: 1}); got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
	want := ingest.Event{
		EventID: "full", TenantID: "tenant-a", Provider: "anthropic", Model: "claude-sonnet",
		Feature: "chat", InputTokens: 100, OutputTokens: 50, CacheReadTokens: 30,
		CacheWriteTokens: 20, LatencyMs: 420, Timestamp: when,
	}
	if got := store.events["full"]; got != want {
		t.Fatalf("stored event = %+v, want %+v", got, want)
	}
	if _, ok := store.events["minimal"]; !ok {
		t.Fatal("the event without optional fields was not stored")
	}
}

// A wrong key is refused for good: the batch is not retried.
func TestClientWithAWrongKeyCountsRejected(t *testing.T) {
	store := &memStore{events: map[string]ingest.Event{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(api.NewRouter(store, oneKey("lum_ingest_k"), nil, log))
	defer srv.Close()

	c, err := client.New(srv.URL, "lum_ingest_other")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Record(client.Event{EventID: "e1", TenantID: "t", Provider: "p", Model: "m"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := c.Stats(), (client.Stats{Rejected: 1}); got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
	if len(store.events) != 0 {
		t.Fatalf("stored %d events with a wrong key", len(store.events))
	}
}
