// Package proxy is the capture proxy the customer runs next to their app.
//
// It forwards every request to the LLM provider and every response back
// without changing them. For a successful Anthropic Messages call, streamed
// or not, it also reads the usage block of the response and records one
// usage event, plus one for each attempt on another model that declined
// after producing output. A non-streamed OpenAI-compatible chat completion
// gives one event too. Only token counts and the caller's tags leave the proxy: request
// and response bodies and provider keys are never stored, logged or sent to
// the metering service.
//
// Metering never fails a call. A response that cannot be metered is still
// forwarded as it is, and the reason is logged and counted.
package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CheeseBurger7/llm-usage-meter/client"
	"github.com/CheeseBurger7/llm-usage-meter/internal/provider"
)

const (
	// TenantHeader names the tenant a call is billed to. A call without it
	// is metered under Config.DefaultTenant, or forwarded but not metered
	// when that is empty.
	TenantHeader = "X-Meter-Tenant"
	// UnassignedTenant is the reserved tenant for calls without
	// TenantHeader (OPEN_QUESTIONS Q18). The leading underscore keeps it
	// apart from real tenant ids. cmd/proxy uses it as the default for
	// Config.DefaultTenant.
	UnassignedTenant = "_unassigned"
	// FeatureHeader optionally names the product feature that made the call.
	FeatureHeader = "X-Meter-Feature"

	// ProviderAnthropic is the provider label on events from the Messages API.
	ProviderAnthropic = "anthropic"

	// DefaultMaxCaptureBytes is the most response bytes kept in memory per
	// call while looking for the usage block.
	DefaultMaxCaptureBytes = 16 << 20

	messagesPath = "/v1/messages"
	chatPath     = "/v1/chat/completions"
	healthPath   = "/healthz"
)

// Recorder receives usage events. *client.Client implements it. Record must
// not block: it is called on the request path.
type Recorder interface {
	Record(client.Event)
}

// Config configures a Proxy.
type Config struct {
	// Upstream is the provider's base URL, for example
	// https://api.anthropic.com. Required.
	Upstream *url.URL
	// Recorder receives the events of each metered call. Required.
	Recorder Recorder
	// Log receives operational messages. Bodies, header values and keys are
	// never logged. Nil discards.
	Log *slog.Logger
	// Transport sends requests upstream. Nil uses http.DefaultTransport.
	Transport http.RoundTripper
	// MaxCaptureBytes overrides DefaultMaxCaptureBytes. A larger response is
	// forwarded but not metered.
	MaxCaptureBytes int64
	// ChatProvider is the provider label on events from OpenAI-compatible
	// chat completions, for example "openai". Many providers serve that
	// shape, so the label cannot be read from the response. Empty means
	// chat completions are forwarded and counted as skipped, never recorded
	// under a guessed provider. Otherwise, after A-Z is lowercased, it must
	// pass the ingest rule for provider labels (Q20), or New fails: ingest
	// would refuse every event recorded under it.
	ChatProvider string
	// DefaultTenant is the tenant of a call that has no TenantHeader, for
	// example UnassignedTenant. Empty means such calls are forwarded and
	// counted as skipped.
	DefaultTenant string
}

// Stats counts what happened to calls that were candidates for metering:
// POST /v1/messages or POST /v1/chat/completions answered with 200.
type Stats struct {
	// Recorded counts calls handed to the Recorder. A call that fell back
	// to another model counts once, however many events it produced.
	Recorded uint64
	// Skipped counts calls that were forwarded but could not be metered.
	Skipped uint64
	// Untagged counts the recorded calls that had no TenantHeader and went
	// to Config.DefaultTenant. It is part of Recorded, not added to it.
	Untagged uint64
}

// Proxy is an http.Handler. It is safe for concurrent use.
type Proxy struct {
	rp   *httputil.ReverseProxy
	rec  Recorder
	log  *slog.Logger
	max  int64
	now  func() time.Time
	chat string // provider label for chat completions; empty: not metered
	// defaultTenant replaces a missing TenantHeader; empty: not metered.
	defaultTenant string

	recorded, skipped, untagged atomic.Uint64
}

// New returns a Proxy that forwards to cfg.Upstream.
func New(cfg Config) (*Proxy, error) {
	u := cfg.Upstream
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("proxy: upstream must be an http or https URL with a host")
	}
	if cfg.Recorder == nil {
		return nil, errors.New("proxy: recorder is required")
	}
	chat := provider.Normalize(cfg.ChatProvider)
	if chat != "" {
		if reason := provider.Check(chat); reason != "" {
			return nil, fmt.Errorf("proxy: chat provider label %s", reason)
		}
	}
	p := &Proxy{
		rec:  cfg.Recorder,
		log:  cfg.Log,
		max:  cfg.MaxCaptureBytes,
		now:  time.Now,
		chat: chat,

		defaultTenant: cfg.DefaultTenant,
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	if p.max <= 0 {
		p.max = DefaultMaxCaptureBytes
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			// The tags are for the meter, not for the provider.
			pr.Out.Header.Del(TenantHeader)
			pr.Out.Header.Del(FeatureHeader)
		},
		Transport:      cfg.Transport,
		ModifyResponse: p.inspect,
		ErrorHandler:   p.upstreamError,
		// The default logger would write to stderr outside the JSON log.
		ErrorLog: slog.NewLogLogger(p.log.Handler(), slog.LevelWarn),
	}
	return p, nil
}

// Stats returns the current counters.
func (p *Proxy) Stats() Stats {
	return Stats{Recorded: p.recorded.Load(), Skipped: p.skipped.Load(), Untagged: p.untagged.Load()}
}

// call is what the proxy remembers about one request until its response
// has been forwarded.
type call struct {
	start   time.Time
	tenant  string
	feature string
	api     api
	// untagged is set when the request had no TenantHeader and tenant is
	// the configured default.
	untagged bool
}

// api is the response shape a metered endpoint returns.
type api int

const (
	apiNone     api = iota // not a metered endpoint
	apiMessages            // POST /v1/messages
	apiChat                // POST /v1/chat/completions
)

func apiOf(r *http.Request) api {
	if r.Method != http.MethodPost {
		return apiNone
	}
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case messagesPath:
		return apiMessages
	case chatPath:
		return apiChat
	}
	return apiNone
}

type callKey struct{}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == healthPath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		w.WriteHeader(http.StatusOK)
		return
	}
	c := &call{
		start:   p.now(),
		tenant:  r.Header.Get(TenantHeader),
		feature: r.Header.Get(FeatureHeader),
		api:     apiOf(r),
	}
	if c.tenant == "" && p.defaultTenant != "" {
		c.tenant, c.untagged = p.defaultTenant, true
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callKey{}, c)))
}

// inspect decides from the response headers whether a call can be metered
// and, if so, wraps the body so the usage block is read as it passes. It
// never changes the response and never returns an error.
func (p *Proxy) inspect(resp *http.Response) error {
	c, _ := resp.Request.Context().Value(callKey{}).(*call)
	if c == nil || c.api == apiNone || resp.StatusCode != http.StatusOK {
		// Other endpoints, and calls the provider refused, carry no usage.
		return nil
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	streamed := mediaType == "text/event-stream"
	plain := encoding == "" || encoding == "identity"
	switch {
	case !streamed && mediaType != "application/json":
		p.skip("response is neither JSON nor an event stream")
	case c.tenant == "":
		// Only reached when no default tenant is configured.
		p.skip("request has no " + TenantHeader + " header")
	case c.api == apiChat && p.chat == "":
		p.skip("no provider label is configured for chat completions")
	case c.api == apiChat && streamed:
		p.skip("streamed chat completions are not metered yet")
	case streamed && !plain:
		p.skip("compressed event streams are not supported")
	case streamed:
		resp.Body = &streamCapture{body: resp.Body, p: p, c: c}
	case !plain && encoding != "gzip":
		p.skip("response content encoding is not supported")
	default:
		resp.Body = &capture{body: resp.Body, p: p, c: c, gzip: encoding == "gzip"}
	}
	return nil
}

func (p *Proxy) skip(reason string) {
	p.skipped.Add(1)
	p.log.Warn("proxy: call forwarded but not metered", "reason", reason)
}

func (p *Proxy) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		// The caller went away; there is nobody to answer.
		p.log.Info("proxy: request cancelled by the caller", "method", r.Method, "path", r.URL.Path)
	} else {
		// The path only: a query string can carry a key with some providers.
		p.log.Error("proxy: upstream request failed", "method", r.Method, "path", r.URL.Path, "err", err.Error())
	}
	http.Error(w, "llm-usage-meter proxy: upstream request failed", http.StatusBadGateway)
}

// capture passes a response body through and keeps a copy, up to the limit,
// to read the usage block from once the whole body has been forwarded.
type capture struct {
	body io.ReadCloser
	p    *Proxy
	c    *call
	gzip bool

	buf     bytes.Buffer
	tooBig  bool
	eof     bool
	latency time.Duration
	once    sync.Once
}

func (b *capture) Read(dst []byte) (int, error) {
	n, err := b.body.Read(dst)
	if n > 0 && !b.tooBig {
		if int64(b.buf.Len()+n) > b.p.max {
			b.tooBig = true
			b.buf = bytes.Buffer{}
		} else {
			b.buf.Write(dst[:n])
		}
	}
	if err == io.EOF && !b.eof {
		b.eof = true
		b.latency = b.p.now().Sub(b.c.start)
	}
	return n, err
}

// Close runs after the last byte went to the caller, so reading the usage
// block here cannot delay the response.
func (b *capture) Close() error {
	err := b.body.Close()
	b.once.Do(b.finish)
	return err
}

func (b *capture) finish() {
	switch {
	case b.tooBig:
		b.p.skip("response is larger than the capture limit")
		return
	case !b.eof:
		b.p.skip("response was not read to the end")
		return
	}
	data := b.buf.Bytes()
	if b.gzip {
		var ok bool
		if data, ok = gunzip(data, b.p.max); !ok {
			b.p.skip("response could not be decompressed within the capture limit")
			return
		}
	}
	if b.c.api == apiChat {
		ev, reason := chatEvent(data, b.p.chat)
		if reason != "" {
			b.p.skip(reason)
			return
		}
		b.p.record(b.c, metered{serving: ev}, b.latency)
		return
	}
	m, ok := anthropicEvents(data)
	if !ok {
		b.p.skip("response has no message id, model or usage block")
		return
	}
	b.p.record(b.c, m, b.latency)
}

// metered is what was read from one response: the event of the attempt
// that produced the returned message, then one event for each attempt that
// declined after producing output.
type metered struct {
	serving  client.Event
	declined []client.Event
	// leftOut counts declined attempts that were not recorded because the
	// usage block cannot tell whether the provider bills them.
	leftOut int
}

// record adds what the proxy knows about the call to the events read from
// the response and hands them to the Recorder.
//
// The latency of the call goes on the serving event only. The provider
// does not report a duration per attempt, and repeating the call's latency
// on every event would count it several times in the hourly sums.
func (p *Proxy) record(c *call, m metered, latency time.Duration) {
	m.serving.LatencyMs = latency.Milliseconds()
	for _, ev := range append([]client.Event{m.serving}, m.declined...) {
		ev.TenantID = c.tenant
		ev.Feature = c.feature
		ev.Timestamp = c.start.UTC()
		p.rec.Record(ev)
	}
	p.recorded.Add(1)
	if c.untagged {
		p.untagged.Add(1)
		// The reason only: the default tenant is configuration, and no
		// header value is logged.
		p.log.Warn("proxy: call metered under the default tenant", "reason", "request has no "+TenantHeader+" header")
	}
	if m.leftOut > 0 {
		p.log.Info("proxy: declined attempts without output were not recorded", "attempts", m.leftOut)
	}
}

func gunzip(data []byte, limit int64) ([]byte, bool) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	out, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil || int64(len(out)) > limit {
		return nil, false
	}
	return out, true
}

// anthropicMessage is the part of a Messages API response the proxy reads.
// Field names follow the "Create a Message" API reference; everything else,
// including the content, is ignored by the decoder.
type anthropicMessage struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		// Both cache counts may be null, which decodes to 0.
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		// Iterations lists every attempt when the provider retried a
		// declined call on a fallback model.
		Iterations []iteration `json:"iterations"`
	} `json:"usage"`
}

// anthropicEvents maps a Messages API response to its usage events.
//
// The provider documents total input as input_tokens +
// cache_creation_input_tokens + cache_read_input_tokens, so input_tokens
// already excludes cache tokens, which is the meaning the event schema uses
// (OPEN_QUESTIONS Q14). The counts are copied without subtraction.
//
// After a fallback the top-level model and counts describe only the attempt
// that produced the returned message; the attempts that declined are read
// from usage.iterations.
func anthropicEvents(body []byte) (metered, bool) {
	var m anthropicMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return metered{}, false
	}
	if m.Type != "message" || m.ID == "" || m.Model == "" || m.Usage == nil {
		return metered{}, false
	}
	out := metered{serving: client.Event{
		// The provider's id is stable for the call, so a re-sent event dedupes.
		EventID:          m.ID,
		Provider:         ProviderAnthropic,
		Model:            m.Model,
		InputTokens:      m.Usage.InputTokens,
		OutputTokens:     m.Usage.OutputTokens,
		CacheReadTokens:  m.Usage.CacheReadInputTokens,
		CacheWriteTokens: m.Usage.CacheCreationInputTokens,
	}}
	out.declined, out.leftOut = declinedAttempts(m.ID, m.Usage.Iterations)
	return out, true
}
