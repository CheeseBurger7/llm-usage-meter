package proxy_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CheeseBurger7/llm-usage-meter/client"
	"github.com/CheeseBurger7/llm-usage-meter/internal/api"
	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
	"github.com/CheeseBurger7/llm-usage-meter/internal/ingest"
	"github.com/CheeseBurger7/llm-usage-meter/internal/proxy"
)

const (
	ingestKey  = "lum_ingest_k"
	prompt     = "PROMPT-TEXT-only-the-provider-may-see-this"
	completion = "COMPLETION-TEXT-only-the-caller-may-see-this"
	apiKey     = "sk-ant-test-PROVIDER-KEY"
	response   = `{"id":"msg_e2e","type":"message","role":"assistant","model":"claude-opus-5-5",` +
		`"content":[{"type":"text","text":"` + completion + `"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":11,"output_tokens":22,"cache_creation_input_tokens":33,"cache_read_input_tokens":44}}`
)

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

func fakeProvider(t *testing.T) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func call(t *testing.T, proxyURL string) (int, string, time.Duration) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/messages",
		strings.NewReader(`{"messages":[{"role":"user","content":"`+prompt+`"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set(proxy.TenantHeader, "tenant-a")
	req.Header.Set(proxy.FeatureHeader, "chat")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call through proxy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), time.Since(start)
}

// A call through the proxy ends up as an event in the service, through the
// real client library, router, auth middleware and batch handler. What the
// service receives holds token counts and tags and nothing of the call.
func TestCallThroughProxyReachesTheService(t *testing.T) {
	store := &memStore{events: map[string]ingest.Event{}}
	var mu sync.Mutex
	var received bytes.Buffer // every byte the metering service was sent
	router := api.NewRouter(store, oneKey(ingestKey), nil, slog.New(slog.DiscardHandler))
	meterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = r.Header.Write(&received)
		received.Write(body)
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		router.ServeHTTP(w, r)
	}))
	defer meterSrv.Close()

	meter, err := client.New(meterSrv.URL, ingestKey)
	if err != nil {
		t.Fatal(err)
	}
	p, err := proxy.New(proxy.Config{Upstream: fakeProvider(t), Recorder: meter})
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := httptest.NewServer(p)

	status, body, _ := call(t, proxySrv.URL)
	if status != http.StatusOK || body != response {
		t.Fatalf("got %d %q", status, body)
	}
	// Closing the server waits for the handler, which records the event.
	proxySrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := meter.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, ok := store.events["msg_e2e"]
	if !ok {
		t.Fatalf("no event stored; client stats %+v", meter.Stats())
	}
	if got.Timestamp.IsZero() {
		t.Fatal("event has no timestamp")
	}
	got.Timestamp, got.LatencyMs = time.Time{}, 0
	want := ingest.Event{
		EventID: "msg_e2e", TenantID: "tenant-a", Provider: "anthropic", Model: "claude-opus-5-5",
		Feature: "chat", InputTokens: 11, OutputTokens: 22, CacheReadTokens: 44, CacheWriteTokens: 33,
	}
	if got != want {
		t.Fatalf("stored event = %+v, want %+v", got, want)
	}

	mu.Lock()
	sent := received.String()
	mu.Unlock()
	for _, secret := range []string{prompt, completion, apiKey} {
		if strings.Contains(sent, secret) {
			t.Fatalf("the metering service was sent %q", secret)
		}
	}
}

// With the metering service unreachable the call still succeeds, at once.
func TestFailsOpenWhenMeteringIsDown(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	meterURL := down.URL
	down.Close() // connections to it are now refused

	meter, err := client.New(meterURL, ingestKey, client.WithFlushInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	p, err := proxy.New(proxy.Config{Upstream: fakeProvider(t), Recorder: meter})
	if err != nil {
		t.Fatal(err)
	}
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	for range 3 {
		status, body, took := call(t, proxySrv.URL)
		if status != http.StatusOK || body != response {
			t.Fatalf("got %d %q", status, body)
		}
		if took > 2*time.Second {
			t.Fatalf("the call took %v with metering down", took)
		}
	}

	// The events are lost, and counted as lost, once Close gives up.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := meter.Close(ctx); err == nil {
		t.Fatal("Close reported success with the service down")
	}
	if got := meter.Stats(); got.Sent != 0 || got.Dropped != 3 {
		t.Fatalf("client stats = %+v, want 3 dropped", got)
	}
}
