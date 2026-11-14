package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeService is a scripted batch endpoint. Each request is answered by the
// next entry of script; once the script runs out, every event is "created".
type fakeService struct {
	t *testing.T

	mu      sync.Mutex
	script  []func(w http.ResponseWriter, events []Event)
	batches [][]Event
	auth    []string
	paths   []string
}

func (f *fakeService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Events []Event `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Errorf("fake service: undecodable body: %v", err)
	}

	f.mu.Lock()
	f.batches = append(f.batches, req.Events)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	var step func(http.ResponseWriter, []Event)
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	}
	f.mu.Unlock()

	if step == nil {
		step = allCreated
	}
	step(w, req.Events)
}

func (f *fakeService) requests() [][]Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]Event(nil), f.batches...)
}

func allCreated(w http.ResponseWriter, events []Event) {
	statuses := make([]string, len(events))
	for i := range statuses {
		statuses[i] = "created"
	}
	results(statuses...)(w, events)
}

func results(statuses ...string) func(http.ResponseWriter, []Event) {
	return func(w http.ResponseWriter, _ []Event) {
		parts := make([]string, len(statuses))
		for i, s := range statuses {
			parts[i] = fmt.Sprintf(`{"event_id":"x","status":%q}`, s)
		}
		_, _ = io.WriteString(w, `{"results":[`+strings.Join(parts, ",")+`]}`)
	}
}

func status(code int, header ...string) func(http.ResponseWriter, []Event) {
	return func(w http.ResponseWriter, _ []Event) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(code)
	}
}

// sleepRecorder replaces the client's backoff wait so tests do not sleep.
type sleepRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) bool {
	s.mu.Lock()
	s.waits = append(s.waits, d)
	s.mu.Unlock()
	return ctx.Err() == nil
}

func (s *sleepRecorder) recorded() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.waits...)
}

// start returns a client pointed at svc. The flush interval is an hour
// unless overridden, so only a full batch or Close sends anything.
func start(t *testing.T, svc http.Handler, opts ...Option) (*Client, *sleepRecorder) {
	t.Helper()
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, "lum_ingest_test", append([]Option{WithFlushInterval(time.Hour)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := &sleepRecorder{}
	c.sleep = rec.sleep
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(ctx)
	})
	return c, rec
}

func closeOK(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func event(id string) Event {
	return Event{
		EventID: id, TenantID: "tenant-a", Provider: "anthropic", Model: "claude-sonnet",
		InputTokens: 100, OutputTokens: 50,
		Timestamp: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func ids(events []Event) string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.EventID
	}
	return strings.Join(out, ",")
}

func wantStats(t *testing.T, c *Client, want Stats) {
	t.Helper()
	if got := c.Stats(); got != want {
		t.Fatalf("stats = %+v, want %+v", got, want)
	}
}

func TestNewRejectsBadArguments(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		key      string
		opts     []Option
	}{
		{"empty endpoint", "", "k", nil},
		{"no scheme", "meter.example.com", "k", nil},
		{"other scheme", "ftp://meter.example.com", "k", nil},
		{"no host", "https://", "k", nil},
		{"unparsable", "http://[::1", "k", nil},
		{"empty key", "https://meter.example.com", "", nil},
		{"zero queue", "https://meter.example.com", "k", []Option{WithQueueSize(0)}},
		{"zero batch", "https://meter.example.com", "k", []Option{WithBatchSize(0)}},
		{"batch over the service limit", "https://meter.example.com", "k", []Option{WithBatchSize(MaxBatchSize + 1)}},
		{"zero flush interval", "https://meter.example.com", "k", []Option{WithFlushInterval(0)}},
		{"zero backoff", "https://meter.example.com", "k", []Option{WithBackoff(0, time.Second)}},
		{"max backoff below min", "https://meter.example.com", "k", []Option{WithBackoff(time.Second, time.Millisecond)}},
		{"nil HTTP client", "https://meter.example.com", "k", []Option{WithHTTPClient(nil)}},
		{"nil logger", "https://meter.example.com", "k", []Option{WithLogger(nil)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.endpoint, tc.key, tc.opts...)
			if err == nil {
				_ = c.Close(context.Background())
				t.Fatal("New succeeded, want an error")
			}
		})
	}
}

func TestSendsBatchToBatchEndpointWithKey(t *testing.T) {
	svc := &fakeService{t: t}
	c, _ := start(t, svc)

	want := event("e1")
	want.Feature = "chat"
	want.CacheReadTokens, want.CacheWriteTokens, want.LatencyMs = 30, 20, 420
	c.Record(want)
	closeOK(t, c)

	reqs := svc.requests()
	if len(reqs) != 1 || len(reqs[0]) != 1 {
		t.Fatalf("requests = %v, want one batch of one event", reqs)
	}
	if got := reqs[0][0]; got != want {
		t.Fatalf("event on the wire = %+v, want %+v", got, want)
	}
	if svc.paths[0] != "POST /v1/events/batch" {
		t.Fatalf("request = %q, want POST /v1/events/batch", svc.paths[0])
	}
	if svc.auth[0] != "Bearer lum_ingest_test" {
		t.Fatalf("Authorization = %q", svc.auth[0])
	}
	wantStats(t, c, Stats{Sent: 1})
}

func TestTrailingSlashInEndpoint(t *testing.T) {
	svc := &fakeService{t: t}
	srv := httptest.NewServer(svc)
	defer srv.Close()

	c, err := New(srv.URL+"/", "k")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Record(event("e1"))
	closeOK(t, c)
	if len(svc.paths) != 1 || svc.paths[0] != "POST /v1/events/batch" {
		t.Fatalf("requests = %v, want one POST /v1/events/batch", svc.paths)
	}
}

func TestRecordFillsInMissingTimestamp(t *testing.T) {
	svc := &fakeService{t: t}
	c, _ := start(t, svc)

	ev := event("e1")
	ev.Timestamp = time.Time{}
	before := time.Now()
	c.Record(ev)
	closeOK(t, c)

	got := svc.requests()[0][0].Timestamp
	if got.Before(before.Add(-time.Second)) || got.After(time.Now().Add(time.Second)) {
		t.Fatalf("timestamp = %v, want about now", got)
	}
}

func TestFullBatchIsSentWithoutWaitingForTheInterval(t *testing.T) {
	svc := &fakeService{t: t}
	c, _ := start(t, svc, WithBatchSize(3))

	for i := range 7 {
		c.Record(event(fmt.Sprintf("e%d", i)))
	}
	// Two full batches go out on their own; the seventh event waits.
	waitFor(t, func() bool { return c.Stats().Sent == 6 })
	wantStats(t, c, Stats{Queued: 1, Sent: 6})

	closeOK(t, c)
	reqs := svc.requests()
	if len(reqs) != 3 || ids(reqs[0]) != "e0,e1,e2" || ids(reqs[1]) != "e3,e4,e5" || ids(reqs[2]) != "e6" {
		t.Fatalf("batches = %v, want [e0 e1 e2] [e3 e4 e5] [e6]", reqs)
	}
}

func TestPartialBatchIsSentAfterTheFlushInterval(t *testing.T) {
	svc := &fakeService{t: t}
	c, _ := start(t, svc, WithFlushInterval(10*time.Millisecond))

	c.Record(event("e1"))
	waitFor(t, func() bool { return c.Stats().Sent == 1 })
}

func TestRetriesWithGrowingBackoffUntilTheServiceAnswers(t *testing.T) {
	svc := &fakeService{t: t, script: []func(http.ResponseWriter, []Event){
		status(500), status(503), status(502), status(408),
	}}
	c, rec := start(t, svc, WithBackoff(100*time.Millisecond, 400*time.Millisecond))

	c.Record(event("e1"))
	closeOK(t, c)

	reqs := svc.requests()
	if len(reqs) != 5 {
		t.Fatalf("got %d requests, want 4 failures and 1 success", len(reqs))
	}
	for i, r := range reqs {
		if ids(r) != "e1" {
			t.Fatalf("request %d carried %q, want the same batch each time", i, ids(r))
		}
	}
	// Each wait is between half of and the whole of min*2^attempt, capped.
	ceilings := []time.Duration{100, 200, 400, 400}
	waits := rec.recorded()
	if len(waits) != len(ceilings) {
		t.Fatalf("waits = %v, want %d of them", waits, len(ceilings))
	}
	for i, w := range waits {
		ceil := ceilings[i] * time.Millisecond
		if w < ceil/2 || w > ceil {
			t.Fatalf("wait %d = %v, want within [%v, %v]", i, w, ceil/2, ceil)
		}
	}
	wantStats(t, c, Stats{Sent: 1})
}

func TestRetriesWhenTheServiceIsUnreachable(t *testing.T) {
	svc := &fakeService{t: t}
	srv := httptest.NewServer(svc)
	defer srv.Close()

	// The first two attempts fail in the transport, before any server.
	var attempts int
	failing := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts <= 2 {
			return nil, fmt.Errorf("dial tcp: connection refused")
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	c, err := New(srv.URL, "k", WithHTTPClient(&http.Client{Transport: failing}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.sleep = (&sleepRecorder{}).sleep

	c.Record(event("e1"))
	closeOK(t, c)
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	wantStats(t, c, Stats{Sent: 1})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHonoursRetryAfterOn429(t *testing.T) {
	svc := &fakeService{t: t, script: []func(http.ResponseWriter, []Event){
		status(429, "Retry-After", "7"),
	}}
	c, rec := start(t, svc)

	c.Record(event("e1"))
	closeOK(t, c)

	waits := rec.recorded()
	if len(waits) != 1 || waits[0] != 7*time.Second {
		t.Fatalf("waits = %v, want one wait of 7s", waits)
	}
	if n := len(svc.requests()); n != 2 {
		t.Fatalf("got %d requests, want 2", n)
	}
	wantStats(t, c, Stats{Sent: 1})
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"":                              0,
		"1":                             time.Second,
		" 3 ":                           3 * time.Second,
		"0":                             0,
		"-5":                            0,
		"soon":                          0,
		"Wed, 21 Oct 2026 07:28:00 GMT": 0,
		"86400":                         maxRetryAfter,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBackoffNeverOverflows(t *testing.T) {
	c := &Client{minBackoff: time.Second, maxBackoff: 30 * time.Second}
	for _, attempt := range []int{0, 5, 31, 32, 62, 63, 64, 1000} {
		if d := c.backoff(attempt); d <= 0 || d > c.maxBackoff {
			t.Fatalf("backoff(%d) = %v, want within (0, %v]", attempt, d, c.maxBackoff)
		}
	}
}

func TestStatusesThatARetryCannotFixAreNotRetried(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 413} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			svc := &fakeService{t: t, script: []func(http.ResponseWriter, []Event){status(code)}}
			c, rec := start(t, svc)

			c.Record(event("e1"))
			c.Record(event("e2"))
			closeOK(t, c)

			if n := len(svc.requests()); n != 1 {
				t.Fatalf("got %d requests, want 1", n)
			}
			if w := rec.recorded(); len(w) != 0 {
				t.Fatalf("waited %v, want no retry", w)
			}
			wantStats(t, c, Stats{Rejected: 2})
		})
	}
}

func TestPerEventResultsAreCounted(t *testing.T) {
	svc := &fakeService{t: t, script: []func(http.ResponseWriter, []Event){
		results("created", "duplicate", "conflict", "invalid"),
	}}
	c, rec := start(t, svc)

	for i := range 4 {
		c.Record(event(fmt.Sprintf("e%d", i)))
	}
	closeOK(t, c)

	if n := len(svc.requests()); n != 1 {
		t.Fatalf("got %d requests, want 1: refused events are final", n)
	}
	if w := rec.recorded(); len(w) != 0 {
		t.Fatalf("waited %v, want no retry", w)
	}
	wantStats(t, c, Stats{Sent: 2, Rejected: 2})
}

func TestUnreadable200IsNotRetried(t *testing.T) {
	svc := &fakeService{t: t, script: []func(http.ResponseWriter, []Event){
		func(w http.ResponseWriter, _ []Event) { _, _ = io.WriteString(w, "<html>ok</html>") },
	}}
	c, _ := start(t, svc)

	c.Record(event("e1"))
	closeOK(t, c)
	if n := len(svc.requests()); n != 1 {
		t.Fatalf("got %d requests, want 1", n)
	}
	wantStats(t, c, Stats{Sent: 1})
}

// The property the package exists for: with the service hanging, Record
// returns at once, memory stays bounded, and the oldest events are the ones
// dropped.
func TestRecordNeverBlocksAndDropsOldestWhenFull(t *testing.T) {
	release := make(chan struct{})
	inFlight := make(chan struct{}, 1)
	svc := &fakeService{t: t, script: []func(http.ResponseWriter, []Event){
		func(w http.ResponseWriter, events []Event) {
			inFlight <- struct{}{}
			<-release
			allCreated(w, events)
		},
	}}
	c, _ := start(t, svc, WithQueueSize(4), WithBatchSize(2))

	c.Record(event("e0"))
	c.Record(event("e1"))
	<-inFlight // e0,e1 are out of the queue and stuck in a request

	begin := time.Now()
	for i := 2; i < 1002; i++ {
		c.Record(event(fmt.Sprintf("e%d", i)))
	}
	if took := time.Since(begin); took > 2*time.Second {
		t.Fatalf("1000 Record calls took %v with the service hanging", took)
	}
	wantStats(t, c, Stats{Queued: 4, Dropped: 996})

	close(release)
	closeOK(t, c)

	reqs := svc.requests()
	if len(reqs) != 3 || ids(reqs[0]) != "e0,e1" || ids(reqs[1]) != "e998,e999" || ids(reqs[2]) != "e1000,e1001" {
		t.Fatalf("batches = %v, want the in-flight pair then the newest four", reqs)
	}
	wantStats(t, c, Stats{Sent: 6, Dropped: 996})
}

func TestRecordIsSafeForConcurrentUse(t *testing.T) {
	svc := &fakeService{t: t}
	c, _ := start(t, svc, WithBatchSize(50))

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 250 {
				c.Record(event(fmt.Sprintf("g%d-e%d", g, i)))
			}
		})
	}
	wg.Wait()
	closeOK(t, c)

	seen := map[string]bool{}
	for _, batch := range svc.requests() {
		if len(batch) > 50 {
			t.Fatalf("batch of %d events, want at most 50", len(batch))
		}
		for _, e := range batch {
			if seen[e.EventID] {
				t.Fatalf("event %s was sent twice", e.EventID)
			}
			seen[e.EventID] = true
		}
	}
	if len(seen) != 2000 {
		t.Fatalf("service saw %d events, want 2000", len(seen))
	}
	wantStats(t, c, Stats{Sent: 2000})
}

func TestCloseFlushesEverythingQueued(t *testing.T) {
	svc := &fakeService{t: t}
	c, _ := start(t, svc, WithBatchSize(100))

	for i := range 250 {
		c.Record(event(fmt.Sprintf("e%d", i)))
	}
	closeOK(t, c)
	wantStats(t, c, Stats{Sent: 250})

	// A second Close is harmless.
	closeOK(t, c)
}

func TestRecordAfterCloseIsDroppedAndCounted(t *testing.T) {
	svc := &fakeService{t: t}
	c, _ := start(t, svc)
	closeOK(t, c)

	c.Record(event("late"))
	wantStats(t, c, Stats{Dropped: 1})
	if n := len(svc.requests()); n != 0 {
		t.Fatalf("got %d requests, want none", n)
	}
}

func TestCloseGivesUpWhenItsContextEnds(t *testing.T) {
	// The service is down for good; the real backoff wait is in use.
	svc := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(svc)
	defer srv.Close()
	c, err := New(srv.URL, "k", WithBatchSize(2), WithFlushInterval(time.Hour), WithBackoff(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range 5 {
		c.Record(event(fmt.Sprintf("e%d", i)))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if err := c.Close(ctx); err == nil {
		t.Fatal("Close returned nil with events unsent")
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("Close took %v, want it to stop at the deadline", took)
	}
	// The batch in flight and the rest of the queue are all accounted for.
	wantStats(t, c, Stats{Dropped: 5})
}

func TestCloseAbortsARequestThatHangs(t *testing.T) {
	hang := make(chan struct{})
	svc := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	})
	srv := httptest.NewServer(svc)
	defer srv.Close()
	defer close(hang)

	c, err := New(srv.URL, "k", WithHTTPClient(&http.Client{}), WithFlushInterval(time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.Record(event("e1"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Close(ctx); err == nil {
		t.Fatal("Close returned nil with the request still hanging")
	}
	wantStats(t, c, Stats{Dropped: 1})
}

func TestKeyAndEndpointAreNotLogged(t *testing.T) {
	var logs strings.Builder
	var mu sync.Mutex
	svc := &fakeService{t: t, script: []func(http.ResponseWriter, []Event){
		status(500), status(401),
	}}
	srv := httptest.NewServer(svc)
	defer srv.Close()

	// One transport failure, one 500 and one 401 cover every log line.
	var attempts int
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, fmt.Errorf("connection refused")
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	c, err := New(srv.URL, "lum_ingest_secret", WithHTTPClient(&http.Client{Transport: transport}),
		WithLogger(newTestLogger(&logs, &mu)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.sleep = (&sleepRecorder{}).sleep
	ev := event("e1")
	ev.TenantID = "tenant-private"
	c.Record(ev)
	closeOK(t, c)

	mu.Lock()
	out := logs.String()
	mu.Unlock()
	if out == "" {
		t.Fatal("nothing was logged; the test would pass vacuously")
	}
	for _, secret := range []string{"lum_ingest_secret", "tenant-private", srv.URL} {
		if strings.Contains(out, secret) {
			t.Fatalf("log output contains %q:\n%s", secret, out)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}
