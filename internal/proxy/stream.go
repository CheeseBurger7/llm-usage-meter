package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/CheeseBurger7/llm-usage-meter/client"
)

const (
	// maxUsageEventBytes is the most data kept for one message_start or
	// message_delta event. Neither carries content, so real ones are far
	// smaller; a larger one is not read and the call is not metered.
	maxUsageEventBytes = 1 << 20
	// maxOtherLineBytes is the most kept of any other line: enough to
	// recognise "event: <name>". The rest of the line, which is where the
	// completion text travels, passes through without being kept.
	maxOtherLineBytes = 128

	eventMessageStart = "message_start"
	eventMessageDelta = "message_delta"
	eventMessageStop  = "message_stop"
	eventError        = "error"
)

// streamUsage is the usage block of a stream event. A count that is absent
// or null leaves the value seen so far unchanged.
type streamUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	// Iterations lists every attempt when the provider retried a declined
	// call on a fallback model.
	Iterations []iteration `json:"iterations"`
}

// streamEvent is the part of a message_start or message_delta event the
// proxy reads. Field names follow the provider's "Streaming messages" page.
type streamEvent struct {
	Message *struct {
		ID    string       `json:"id"`
		Model string       `json:"model"`
		Usage *streamUsage `json:"usage"`
	} `json:"message"` // message_start
	Usage *streamUsage `json:"usage"` // message_delta
	// Delta holds the top-level fields of the message that changed; the
	// stop reason arrives here (message_delta).
	Delta *struct {
		StopReason  *string      `json:"stop_reason"`
		StopDetails *stopDetails `json:"stop_details"`
	} `json:"delta"`
}

// sseMeter reads the usage out of a Messages event stream as its bytes pass
// through. It keeps the current line up to a small limit and the data of
// usage events only, so memory per call does not grow with the response.
//
// It relies on the "event:" line coming before the "data:" line of the same
// event, as the provider sends them. Lines end with LF or CRLF.
type sseMeter struct {
	line     []byte
	lineOver bool // the current line was longer than its limit

	event    string // name of the event being read
	data     []byte // its data, kept for usage events only
	dataOver bool

	ev          client.Event
	iterations  []iteration // the last usage.iterations seen
	haveUsage   bool
	stopReason  string
	stopDetails *stopDetails
	stopped     bool // message_stop seen: the stream is complete
	errored     bool // the provider sent an error event
	unread      bool // a usage event could not be read
}

func usageEvent(name string) bool {
	return name == eventMessageStart || name == eventMessageDelta
}

// write consumes the next bytes of the stream. It reports true when they
// completed the message.
func (s *sseMeter) write(p []byte) bool {
	wasStopped := s.stopped
	for len(p) > 0 && !s.stopped {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.keep(p)
			break
		}
		s.keep(p[:i])
		s.endLine()
		p = p[i+1:]
	}
	return s.stopped && !wasStopped
}

func (s *sseMeter) keep(p []byte) {
	limit := maxOtherLineBytes
	if usageEvent(s.event) {
		limit = maxUsageEventBytes
	}
	if room := limit - len(s.line); len(p) > room {
		p = p[:max(room, 0)]
		s.lineOver = true
	}
	s.line = append(s.line, p...)
}

func (s *sseMeter) endLine() {
	line, over := bytes.TrimSuffix(s.line, []byte("\r")), s.lineOver
	s.line, s.lineOver = s.line[:0], false

	if len(line) == 0 && !over {
		s.dispatch()
		return
	}
	field, value, _ := bytes.Cut(line, []byte(":"))
	value = bytes.TrimPrefix(value, []byte(" "))
	switch string(field) {
	case "event":
		s.event = string(value)
		if over {
			s.event = "" // no event the proxy reads has a name this long
		}
	case "data":
		if !usageEvent(s.event) {
			return
		}
		if over || len(s.data)+len(value) > maxUsageEventBytes {
			s.dataOver = true
			return
		}
		if len(s.data) > 0 {
			s.data = append(s.data, '\n')
		}
		s.data = append(s.data, value...)
	}
}

// dispatch handles the event that a blank line just ended.
func (s *sseMeter) dispatch() {
	event, data, over := s.event, s.data, s.dataOver
	s.event, s.data, s.dataOver = "", s.data[:0], false

	switch event {
	case eventMessageStop:
		s.stopped = true
	case eventError:
		s.errored = true
	case eventMessageStart, eventMessageDelta:
		var e streamEvent
		if over || json.Unmarshal(data, &e) != nil {
			s.unread = true
			return
		}
		if m := e.Message; m != nil {
			s.ev.EventID, s.ev.Model = m.ID, m.Model
			s.apply(m.Usage)
		}
		s.apply(e.Usage)
		if d := e.Delta; d != nil {
			if d.StopReason != nil {
				s.stopReason = *d.StopReason
			}
			if d.StopDetails != nil {
				s.stopDetails = d.StopDetails
			}
		}
	}
}

// apply overwrites the counts seen so far: the provider documents the
// counts in message_delta as cumulative, not as increments.
func (s *sseMeter) apply(u *streamUsage) {
	if u == nil {
		return
	}
	set := func(dst, src *int64) {
		if src != nil {
			*dst = *src
			s.haveUsage = true
		}
	}
	set(&s.ev.InputTokens, u.InputTokens)
	set(&s.ev.OutputTokens, u.OutputTokens)
	set(&s.ev.CacheWriteTokens, u.CacheCreationInputTokens)
	set(&s.ev.CacheReadTokens, u.CacheReadInputTokens)
	if len(u.Iterations) > 0 {
		s.iterations = u.Iterations
	}
	// After a fallback in the middle of the stream, message_start named
	// the model that declined; the counts belong to the one that served.
	for _, it := range u.Iterations {
		if it.Type == iterationFallback && it.Model != "" {
			s.ev.Model = it.Model
		}
	}
}

// result returns the events for a finished stream, or why there are none.
//
// A stream that ended before message_stop because the caller hung up or the
// connection dropped is recorded as partial (OPEN_QUESTIONS Q19): the input
// counts from message_start are exact, the output count is the latest seen
// and usually far short of the real one. Declined fallback attempts are not
// recorded for it, since usage.iterations arrives in the final message_delta.
// A stream that carried an error event stays skipped: the provider's docs do
// not say that its input is billed.
func (s *sseMeter) result() (metered, string) {
	switch {
	case s.unread:
		return metered{}, "a usage event in the stream could not be read"
	case s.errored:
		return metered{}, "the provider sent an error in the stream"
	case s.ev.EventID == "" || s.ev.Model == "" || !s.haveUsage:
		if !s.stopped {
			return metered{}, "stream ended before message_stop and before its usage was seen"
		}
		return metered{}, "stream has no message id, model or usage"
	}
	s.ev.Provider = ProviderAnthropic
	if !s.stopped {
		s.ev.Partial = true
		m := metered{serving: s.ev, partial: true}
		m.refusal = refusalWithoutOutput(s.stopReason, s.stopDetails, s.ev.OutputTokens)
		return m, ""
	}
	m := metered{serving: s.ev}
	m.declined, m.leftOut = declinedAttempts(s.ev.EventID, s.iterations)
	m.mismatch = fallbackUsageMismatch(s.ev, s.iterations)
	m.refusal = refusalWithoutOutput(s.stopReason, s.stopDetails, s.ev.OutputTokens)
	return m, ""
}

// streamCapture passes an event stream through and feeds the same bytes to
// an sseMeter. Read returns exactly what the provider sent, when it sent
// it: nothing is held back, so time to first byte is unchanged.
type streamCapture struct {
	body io.ReadCloser
	p    *Proxy
	c    *call

	meter   sseMeter
	latency time.Duration
	once    sync.Once
}

func (b *streamCapture) Read(dst []byte) (int, error) {
	n, err := b.body.Read(dst)
	if n > 0 && b.meter.write(dst[:n]) {
		b.latency = b.p.now().Sub(b.c.start)
	}
	return n, err
}

// Close runs once the stream has ended or the caller went away.
func (b *streamCapture) Close() error {
	err := b.body.Close()
	b.once.Do(func() {
		m, reason := b.meter.result()
		if reason != "" {
			b.p.skip(reason)
			return
		}
		if m.partial {
			// No message_stop: latency ends where the stream did.
			b.latency = b.p.now().Sub(b.c.start)
		}
		b.p.record(b.c, m, b.latency)
	})
	return err
}
