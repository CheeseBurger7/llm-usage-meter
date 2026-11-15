package proxy

import (
	"bytes"
	"compress/gzip"
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
)

// Strings that must never reach a log line or the metering service.
const (
	secretKey        = "sk-ant-test-PROVIDER-KEY"
	secretPrompt     = "PROMPT-TEXT-what-is-the-launch-code"
	secretCompletion = "COMPLETION-TEXT-the-launch-code-is"
)

const requestBody = `{"model":"claude-opus-5-5","max_tokens":64,"messages":[{"role":"user","content":"` + secretPrompt + `"}]}`

// messageBody has the shape of the 200 example in the provider's "Create a
// Message" API reference, including fields the proxy must ignore.
const messageBody = `{
  "id": "msg_013Zva2CMHLNnXjNJJKqJ2EF",
  "container": null,
  "content": [{"citations": null, "text": "` + secretCompletion + `", "type": "text"}],
  "diagnostics": null,
  "model": "claude-opus-5-5",
  "role": "assistant",
  "stop_details": null,
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "type": "message",
  "usage": {
    "cache_creation": {"ephemeral_1h_input_tokens": 0, "ephemeral_5m_input_tokens": 2051},
    "cache_creation_input_tokens": 2051,
    "cache_read_input_tokens": 1024,
    "inference_geo": "global",
    "input_tokens": 2095,
    "output_tokens": 503,
    "output_tokens_details": {"thinking_tokens": 0},
    "server_tool_use": {"web_fetch_requests": 2, "web_search_requests": 0},
    "service_tier": "standard",
    "speed": "standard"
  }
}`

type fakeRecorder struct {
	mu     sync.Mutex
	events []client.Event
}

func (f *fakeRecorder) Record(ev client.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *fakeRecorder) all() []client.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]client.Event(nil), f.events...)
}

// seen is what the fake upstream received.
type seen struct {
	mu     sync.Mutex
	method string
	uri    string
	header http.Header
	body   string
}

func (s *seen) get() (string, string, http.Header, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.method, s.uri, s.header, s.body
}

type fixture struct {
	proxy    *httptest.Server
	p        *Proxy
	rec      *fakeRecorder
	upstream *seen
	logs     *bytes.Buffer
}

// newFixture starts a fake upstream answering with respond and a proxy in
// front of it.
func newFixture(t *testing.T, respond http.HandlerFunc, opts ...func(*Config)) *fixture {
	t.Helper()
	s := &seen{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.method, s.uri, s.header, s.body = r.Method, r.RequestURI, r.Header.Clone(), string(body)
		s.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(up.Close)

	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	rec := &fakeRecorder{}
	cfg := Config{Upstream: u, Recorder: rec, Log: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	for _, o := range opts {
		o(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return &fixture{proxy: srv, p: p, rec: rec, upstream: s, logs: &logs.buf}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func jsonMessage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Request-Id", "req_123")
	_, _ = io.WriteString(w, messageBody)
}

// post sends a Messages request through the proxy. The transport does not
// ask for compression, so the bytes compared are the bytes on the wire.
func (f *fixture) post(t *testing.T, path string, header map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.proxy.URL+path, strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", secretKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	hc := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	return resp, string(body)
}

// waitStats waits until the proxy has accounted for n candidate calls. The
// usage block is read after the last byte went to the caller, so the caller
// can have its answer slightly before the event exists.
func (f *fixture) waitStats(t *testing.T, want Stats) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := f.p.Stats()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stats = %+v, want %+v", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

var tags = map[string]string{TenantHeader: "tenant-a", FeatureHeader: "chat"}

func TestForwardsUnchangedAndRecordsUsage(t *testing.T) {
	f := newFixture(t, jsonMessage)
	start := time.Now()
	resp, body := f.post(t, "/v1/messages?beta=true", tags)

	// The caller gets the provider's answer as it was.
	if resp.StatusCode != http.StatusOK || body != messageBody {
		t.Fatalf("status %d, body changed: %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Request-Id"); got != "req_123" {
		t.Fatalf("response header Request-Id = %q", got)
	}

	// The provider gets the caller's request as it was, minus the tags.
	method, uri, header, upBody := f.upstream.get()
	if method != http.MethodPost || uri != "/v1/messages?beta=true" || upBody != requestBody {
		t.Fatalf("upstream got %s %s %q", method, uri, upBody)
	}
	if header.Get("X-Api-Key") != secretKey || header.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("provider headers were not forwarded: %v", header)
	}
	if header.Get(TenantHeader) != "" || header.Get(FeatureHeader) != "" {
		t.Fatalf("metering headers reached the provider: %v", header)
	}

	f.waitStats(t, Stats{Recorded: 1})
	events := f.rec.all()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	got := events[0]
	if got.Timestamp.Before(start.Add(-time.Second)) || got.Timestamp.After(time.Now()) || got.LatencyMs < 0 {
		t.Fatalf("timestamp %v or latency %d out of range", got.Timestamp, got.LatencyMs)
	}
	got.Timestamp, got.LatencyMs = time.Time{}, 0
	want := client.Event{
		EventID: "msg_013Zva2CMHLNnXjNJJKqJ2EF", TenantID: "tenant-a", Provider: "anthropic",
		Model: "claude-opus-5-5", Feature: "chat",
		InputTokens: 2095, OutputTokens: 503, CacheReadTokens: 1024, CacheWriteTokens: 2051,
	}
	if got != want {
		t.Fatalf("event = %+v, want %+v", got, want)
	}
}

func TestGzipResponseIsForwardedCompressedAndMetered(t *testing.T) {
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	_, _ = io.WriteString(zw, messageBody)
	_ = zw.Close()

	f := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(zipped.Bytes())
	})
	resp, body := f.post(t, "/v1/messages", map[string]string{TenantHeader: "tenant-a", "Accept-Encoding": "gzip"})

	if resp.Header.Get("Content-Encoding") != "gzip" || body != zipped.String() {
		t.Fatal("the compressed response was not forwarded byte for byte")
	}
	f.waitStats(t, Stats{Recorded: 1})
	if ev := f.rec.all()[0]; ev.EventID != "msg_013Zva2CMHLNnXjNJJKqJ2EF" || ev.OutputTokens != 503 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestNullCacheCountsAreZero(t *testing.T) {
	ev, ok := anthropicEvent([]byte(`{"type":"message","id":"msg_1","model":"m",
		"usage":{"input_tokens":7,"output_tokens":3,"cache_creation_input_tokens":null,"cache_read_input_tokens":null}}`))
	if !ok || ev.InputTokens != 7 || ev.OutputTokens != 3 || ev.CacheReadTokens != 0 || ev.CacheWriteTokens != 0 {
		t.Fatalf("event = %+v, ok = %v", ev, ok)
	}
}

// Every case is forwarded exactly as the provider answered; none produces
// an event.
func TestForwardedButNotMetered(t *testing.T) {
	respondWith := func(status int, contentType, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}
	}
	const sse = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	const apiError = `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`

	tests := []struct {
		name    string
		path    string
		header  map[string]string
		respond http.HandlerFunc
		status  int
		body    string
		skipped uint64
		opts    []func(*Config)
	}{
		{name: "provider error", path: "/v1/messages", header: tags,
			respond: respondWith(429, "application/json", apiError), status: 429, body: apiError},
		{name: "stream cut off before the end", path: "/v1/messages", header: tags,
			respond: respondWith(200, "text/event-stream", sse), status: 200, body: sse, skipped: 1},
		{name: "no tenant header", path: "/v1/messages", header: map[string]string{FeatureHeader: "chat"},
			respond: jsonMessage, status: 200, body: messageBody, skipped: 1},
		{name: "another endpoint", path: "/v1/messages/count_tokens", header: tags,
			respond: respondWith(200, "application/json", `{"input_tokens":12}`), status: 200, body: `{"input_tokens":12}`},
		{name: "not JSON", path: "/v1/messages", header: tags,
			respond: respondWith(200, "text/html", "<html>gateway</html>"), status: 200, body: "<html>gateway</html>", skipped: 1},
		{name: "malformed JSON", path: "/v1/messages", header: tags,
			respond: respondWith(200, "application/json", `{"id":"msg_1",`), status: 200, body: `{"id":"msg_1",`, skipped: 1},
		{name: "no usage block", path: "/v1/messages", header: tags,
			respond: respondWith(200, "application/json", `{"type":"message","id":"msg_1","model":"m"}`),
			status:  200, body: `{"type":"message","id":"msg_1","model":"m"}`, skipped: 1},
		{name: "unknown content encoding", path: "/v1/messages", header: tags,
			respond: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", "br")
				_, _ = io.WriteString(w, "not really brotli")
			}, status: 200, body: "not really brotli", skipped: 1},
		{name: "larger than the capture limit", path: "/v1/messages", header: tags,
			respond: jsonMessage, status: 200, body: messageBody, skipped: 1,
			opts: []func(*Config){func(c *Config) { c.MaxCaptureBytes = 64 }}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.respond, tt.opts...)
			resp, body := f.post(t, tt.path, tt.header)
			if resp.StatusCode != tt.status || body != tt.body {
				t.Fatalf("got %d %q, want %d %q", resp.StatusCode, body, tt.status, tt.body)
			}
			f.waitStats(t, Stats{Skipped: tt.skipped})
			if n := len(f.rec.all()); n != 0 {
				t.Fatalf("recorded %d events, want 0", n)
			}
		})
	}
}

func TestUpstreamDownAnswers502(t *testing.T) {
	f := newFixture(t, jsonMessage, func(c *Config) {
		c.Upstream = &url.URL{Scheme: "http", Host: "127.0.0.1:1"} // nothing listens here
	})
	resp, _ := f.post(t, "/v1/messages", tags)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := f.p.Stats(); got != (Stats{}) {
		t.Fatalf("stats = %+v, want zero", got)
	}
}

func TestHealthzIsAnsweredLocally(t *testing.T) {
	f := newFixture(t, func(http.ResponseWriter, *http.Request) {
		t.Error("/healthz was forwarded to the provider")
	})
	resp, err := http.Get(f.proxy.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// Neither the provider key nor any part of a request or response body may
// appear in a log line, including when a call cannot be metered or fails.
func TestLogsNeverContainBodiesOrKeys(t *testing.T) {
	f := newFixture(t, jsonMessage)
	f.post(t, "/v1/messages?key="+secretKey, tags) // metered
	f.post(t, "/v1/messages?key="+secretKey, nil)  // no tenant: skipped
	f.post(t, "/v1/other?key="+secretKey, tags)    // not a metered path
	f.waitStats(t, Stats{Recorded: 1, Skipped: 1})

	down := newFixture(t, jsonMessage, func(c *Config) {
		c.Upstream = &url.URL{Scheme: "http", Host: "127.0.0.1:1"}
	})
	down.post(t, "/v1/messages?key="+secretKey, tags)

	for name, logs := range map[string]string{"up": f.logs.String(), "down": down.logs.String()} {
		if logs == "" {
			t.Fatalf("%s: expected some log output", name)
		}
		for _, secret := range []string{secretKey, secretPrompt, secretCompletion, "tenant-a"} {
			if strings.Contains(logs, secret) {
				t.Fatalf("%s: log contains %q:\n%s", name, secret, logs)
			}
		}
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	ok := &url.URL{Scheme: "https", Host: "api.example.com"}
	for name, cfg := range map[string]Config{
		"no upstream":  {Recorder: &fakeRecorder{}},
		"no host":      {Upstream: &url.URL{Scheme: "https"}, Recorder: &fakeRecorder{}},
		"wrong scheme": {Upstream: &url.URL{Scheme: "ftp", Host: "x"}, Recorder: &fakeRecorder{}},
		"no recorder":  {Upstream: ok},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New accepted the config", name)
		}
	}
}
