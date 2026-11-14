// Package client records LLM usage events and delivers them to the metering
// service in the background.
//
// Record never blocks and never returns an error, so metering cannot slow
// down or fail the LLM call it describes. Events wait in a bounded in-memory
// queue, are sent in batches to POST /v1/events/batch, and are retried with
// exponential backoff while the service is unreachable. When the queue is
// full the oldest event is dropped and counted.
//
//	c, err := client.New("https://meter.example.com", os.Getenv("METER_INGEST_KEY"))
//	if err != nil { ... }
//	defer c.Close(ctx)
//
//	c.Record(client.Event{
//		EventID: resp.ID, TenantID: "tenant-a", Provider: "anthropic",
//		Model: resp.Model, InputTokens: 100, OutputTokens: 50,
//	})
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one LLM call. The fields and their rules are those of the ingest
// API (docs/ingestion.md). EventID must be stable across retries of the same
// call, for example the provider's response id: the service dedupes on it.
type Event struct {
	EventID  string `json:"event_id"`
	TenantID string `json:"tenant_id"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Feature is optional.
	Feature string `json:"feature,omitempty"`
	// InputTokens excludes cache tokens: a call's total input is
	// InputTokens + CacheReadTokens + CacheWriteTokens.
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	LatencyMs        int64 `json:"latency_ms"`
	// Timestamp is when the call happened. Record sets a zero Timestamp to
	// the current time.
	Timestamp time.Time `json:"timestamp"`
}

// Stats is a snapshot of what happened to recorded events.
type Stats struct {
	// Queued is the number of events waiting to be sent, not counting a
	// batch that is being sent right now.
	Queued int
	// Sent counts events the service answered "created" or "duplicate".
	Sent uint64
	// Rejected counts events the service refused for good: invalid or
	// conflicting events, and whole batches refused with a status that a
	// retry cannot fix, such as 401 for a revoked key.
	Rejected uint64
	// Dropped counts events that never got an answer: evicted from a full
	// queue, recorded after Close, or still unsent when Close gave up.
	Dropped uint64
}

// MaxBatchSize is the most events the service accepts in one batch request.
const MaxBatchSize = 500

const (
	batchPath = "/v1/events/batch"

	defaultQueueSize     = 10000
	defaultBatchSize     = 100
	defaultFlushInterval = time.Second
	defaultMinBackoff    = 200 * time.Millisecond
	defaultMaxBackoff    = 30 * time.Second
	defaultHTTPTimeout   = 10 * time.Second

	// maxRetryAfter caps how long a Retry-After header can make the client
	// wait, so a wrong header cannot stall delivery for hours.
	maxRetryAfter = time.Minute
	// maxResponseBytes bounds how much of a response is read. 500 results
	// with field errors are far below it.
	maxResponseBytes = 4 << 20
)

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the HTTP client used for delivery. The default has a
// 10 second timeout. A client without a timeout can hang a batch, and with
// it Close, until its context ends.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithQueueSize sets how many events may wait in memory (default 10000).
// One more event beyond that evicts the oldest.
func WithQueueSize(n int) Option {
	return func(c *Client) { c.queueSize = n }
}

// WithBatchSize sets how many events go in one request (default 100, at
// most MaxBatchSize). A full batch is sent without waiting for the flush
// interval.
func WithBatchSize(n int) Option {
	return func(c *Client) { c.batchSize = n }
}

// WithFlushInterval sets how long an event waits at most before a partial
// batch is sent (default 1 second).
func WithFlushInterval(d time.Duration) Option {
	return func(c *Client) { c.flushInterval = d }
}

// WithBackoff sets the first and the longest wait between retries of a
// batch (defaults 200 milliseconds and 30 seconds). The wait doubles on each
// failed attempt.
func WithBackoff(minWait, maxWait time.Duration) Option {
	return func(c *Client) { c.minBackoff, c.maxBackoff = minWait, maxWait }
}

// WithLogger sets where delivery problems are logged. The default discards
// them; Stats still counts them. Events, keys and bodies are never logged.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) { c.log = l }
}

// Client queues events and delivers them from one background goroutine. It
// is safe for concurrent use.
type Client struct {
	url  string
	key  string
	http *http.Client
	log  *slog.Logger

	queueSize     int
	batchSize     int
	flushInterval time.Duration
	minBackoff    time.Duration
	maxBackoff    time.Duration

	// sleep waits for d or until ctx ends, and reports whether it waited
	// the full time. Tests replace it.
	sleep func(ctx context.Context, d time.Duration) bool

	mu     sync.Mutex
	q      *queue
	closed bool

	wake    chan struct{} // a full batch is waiting
	closing chan struct{} // closed by Close: flush everything and stop
	done    chan struct{} // closed when the worker has stopped

	// ctx ends when Close gives up; it aborts requests and backoff waits.
	ctx    context.Context
	cancel context.CancelFunc

	sent, rejected, dropped atomic.Uint64
}

// New returns a Client that sends to the metering service at endpoint, for
// example "https://meter.example.com", using an API key with the ingest
// scope. It starts one goroutine; call Close to flush and stop it.
func New(endpoint, key string, opts ...Option) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("client: invalid endpoint: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("client: endpoint must be an http or https URL with a host")
	}
	if key == "" {
		return nil, errors.New("client: key is required")
	}

	c := &Client{
		url:           strings.TrimRight(endpoint, "/") + batchPath,
		key:           key,
		http:          &http.Client{Timeout: defaultHTTPTimeout},
		log:           slog.New(slog.DiscardHandler),
		queueSize:     defaultQueueSize,
		batchSize:     defaultBatchSize,
		flushInterval: defaultFlushInterval,
		minBackoff:    defaultMinBackoff,
		maxBackoff:    defaultMaxBackoff,
		sleep:         sleepContext,
		wake:          make(chan struct{}, 1),
		closing:       make(chan struct{}),
		done:          make(chan struct{}),
	}
	for _, opt := range opts {
		opt(c)
	}
	switch {
	case c.http == nil:
		return nil, errors.New("client: HTTP client must not be nil")
	case c.log == nil:
		return nil, errors.New("client: logger must not be nil")
	case c.queueSize < 1:
		return nil, errors.New("client: queue size must be at least 1")
	case c.batchSize < 1 || c.batchSize > MaxBatchSize:
		return nil, fmt.Errorf("client: batch size must be 1 to %d", MaxBatchSize)
	case c.flushInterval <= 0:
		return nil, errors.New("client: flush interval must be positive")
	case c.minBackoff <= 0 || c.maxBackoff < c.minBackoff:
		return nil, errors.New("client: backoff must be positive, with max >= min")
	}

	c.q = newQueue(c.queueSize)
	c.ctx, c.cancel = context.WithCancel(context.Background())
	go c.run()
	return c, nil
}

// Record queues an event for delivery and returns at once. It never blocks
// on the network and never fails: if the queue is full the oldest queued
// event is dropped, and after Close the event itself is dropped. Both are
// counted in Stats.
func (c *Client) Record(ev Event) {
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now().UTC()
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.dropped.Add(1)
		return
	}
	evicted := c.q.push(ev)
	full := c.q.len() >= c.batchSize
	c.mu.Unlock()

	if evicted {
		c.dropped.Add(1)
	}
	if full {
		select {
		case c.wake <- struct{}{}:
		default: // the worker already has a wake-up pending
		}
	}
}

// Stats returns the current counters.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	queued := c.q.len()
	c.mu.Unlock()
	return Stats{
		Queued:   queued,
		Sent:     c.sent.Load(),
		Rejected: c.rejected.Load(),
		Dropped:  c.dropped.Load(),
	}
}

// Close stops accepting events, sends what is queued and stops the worker.
// It keeps retrying while the service is unreachable, so give ctx a
// deadline: when ctx ends, unsent events are dropped and counted, and Close
// returns an error. Calling Close again waits for the same shutdown.
func (c *Client) Close(ctx context.Context) error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.closing)
	}
	c.mu.Unlock()

	select {
	case <-c.done:
	case <-ctx.Done():
		c.cancel()
		<-c.done
		return fmt.Errorf("client: close gave up before every event was sent: %w", ctx.Err())
	}
	c.cancel()
	return nil
}

func (c *Client) run() {
	defer close(c.done)
	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()

	for {
		// A wake-up means a full batch may be waiting. It can be stale (the
		// batch already went out), so only the ticker and Close send a
		// partial batch.
		closing, partial := false, false
		select {
		case <-c.wake:
		case <-ticker.C:
			partial = true
		case <-c.closing:
			closing = true
		}

		for closing || partial || c.queued() >= c.batchSize {
			batch := c.take()
			if len(batch) == 0 {
				break
			}
			if !c.deliver(batch) {
				// Close gave up. Whatever is left will never be sent.
				c.dropped.Add(uint64(len(batch) + len(c.takeAll())))
				return
			}
			// After the ticker's one partial batch, only full ones.
			partial = false
		}
		if closing {
			return
		}
	}
}

func (c *Client) take() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.q.take(c.batchSize)
}

func (c *Client) takeAll() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.q.take(c.q.len())
}

func (c *Client) queued() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.q.len()
}

// deliver sends one batch until the service gives a final answer, waiting
// between attempts. It returns false only when c.ctx ended first, in which
// case the batch was not delivered.
//
// There is no attempt limit. During an outage the batch stays in flight and
// new events pile up in the queue, which drops its oldest when full. Giving
// up after N attempts would lose events in an outage the queue could have
// ridden out.
func (c *Client) deliver(batch []Event) bool {
	body, err := json.Marshal(batchRequest{Events: batch})
	if err != nil {
		// Cannot happen for Event's field types; never retry it.
		c.rejected.Add(uint64(len(batch)))
		c.log.Error("meter client: encoding a batch failed", "events", len(batch), "err", err)
		return true
	}

	for attempt := 0; ; attempt++ {
		out := c.post(body, len(batch))
		if out.final {
			return true
		}
		if c.ctx.Err() != nil {
			return false
		}
		wait := max(c.backoff(attempt), out.retryAfter)
		c.log.Warn("meter client: batch not delivered, will retry",
			"events", len(batch), "attempt", attempt+1, "wait", wait, "reason", out.reason)
		if !c.sleep(c.ctx, wait) {
			return false
		}
	}
}

type batchRequest struct {
	Events []Event `json:"events"`
}

type batchResponse struct {
	Results []struct {
		Status string `json:"status"`
	} `json:"results"`
}

type outcome struct {
	// final means the service answered and a retry would change nothing.
	final      bool
	retryAfter time.Duration
	reason     string
}

// post makes one attempt and updates the counters when the answer is final.
func (c *Client) post(body []byte, n int) outcome {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		c.rejected.Add(uint64(n))
		c.log.Error("meter client: building the request failed", "events", n, "err", err)
		return outcome{final: true}
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return outcome{reason: "request failed: " + redact(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))

	switch {
	case resp.StatusCode == http.StatusOK:
		// The service has stored the batch, so nothing here is retried,
		// even if the answer cannot be read.
		var parsed batchResponse
		if readErr != nil || json.Unmarshal(data, &parsed) != nil || len(parsed.Results) != n {
			c.sent.Add(uint64(n))
			c.log.Warn("meter client: batch accepted but its results were unreadable", "events", n)
			return outcome{final: true}
		}
		var refused uint64
		for _, r := range parsed.Results {
			if r.Status != "created" && r.Status != "duplicate" {
				refused++
			}
		}
		c.sent.Add(uint64(n) - refused)
		if refused > 0 {
			c.rejected.Add(refused)
			c.log.Warn("meter client: the service refused some events as invalid or conflicting",
				"refused", refused, "events", n)
		}
		return outcome{final: true}

	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode >= 500:
		return outcome{
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			reason:     "status " + strconv.Itoa(resp.StatusCode),
		}

	default:
		// 400, 401, 403, 413 and the like: sending the same bytes with the
		// same key again gets the same answer.
		c.rejected.Add(uint64(n))
		c.log.Error("meter client: the service refused a batch", "events", n, "status", resp.StatusCode)
		return outcome{final: true}
	}
}

// backoff returns the wait before retry number attempt+1: the minimum
// doubled per attempt up to the maximum, then reduced by up to half at
// random so that many clients do not retry in step.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.maxBackoff
	if attempt < 32 {
		if grown := c.minBackoff << attempt; grown > 0 && grown < c.maxBackoff {
			d = grown
		}
	}
	half := d / 2
	return half + rand.N(d-half+1)
}

// parseRetryAfter reads a Retry-After header given in whole seconds, the
// form the service sends. Anything else is treated as absent.
func parseRetryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || secs <= 0 {
		return 0
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter)
}

// redact describes a transport error without the request URL, which
// *url.Error would otherwise include.
func redact(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err.Error()
	}
	return err.Error()
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
