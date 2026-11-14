# Go client library

`client/` records usage events from a Go program and delivers them to the service in the
background. The capture proxy and the load generator will send through it.

The one rule it is built around: **metering must never slow down or fail the LLM call it
describes.** `Record` only puts the event in an in-memory queue. It does no I/O, takes no context
and returns no error.

```go
import "github.com/CheeseBurger7/llm-usage-meter/client"

c, err := client.New("https://meter.example.com", os.Getenv("METER_INGEST_KEY"))
if err != nil {
    return err // bad endpoint or empty key; nothing was started
}
defer func() {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    _ = c.Close(ctx) // sends what is queued
}()

// After each LLM call:
c.Record(client.Event{
    EventID:  resp.ID, // stable per call, so a retry dedupes
    TenantID: "tenant-a",
    Provider: "anthropic",
    Model:    resp.Model,
    Feature:  "chat",
    InputTokens: 100, OutputTokens: 50,
    LatencyMs: 420,
})
```

The key needs the `ingest` scope ([api-keys.md](api-keys.md)). `Event` has the fields of the
ingest API with the same rules ([ingestion.md](ingestion.md#event-fields)); a zero `Timestamp` is
set to the time of the `Record` call. The client does not validate events itself. The service
does, and a refused event is counted in `Stats().Rejected`.

## How delivery works

One goroutine per client takes events off the queue and posts them to `POST /v1/events/batch`.

| Step | Behaviour |
|---|---|
| Batching | A batch goes out as soon as `batch size` events are waiting, or after the flush interval for a partial batch |
| Service answers `200` | Each event's result is counted: `created` and `duplicate` as sent, `conflict` and `invalid` as rejected. Nothing is retried, because every per-event result is final |
| `429`, `408`, `5xx` or no answer | The same batch is sent again after a wait. The wait starts at the minimum backoff and doubles per attempt up to the maximum, minus up to half at random. A `Retry-After` header in seconds is honoured if it is longer (capped at one minute) |
| Any other status (`400`, `401`, `403`, `413`) | The batch is counted as rejected and not retried: the same bytes with the same key would get the same answer |
| Queue full | The **oldest** queued event is dropped to make room and counted in `Dropped` |
| `Close(ctx)` | Stops accepting events, sends everything queued, and returns. If `ctx` ends first, the unsent events are counted as dropped and `Close` returns an error |

Retrying is safe because ingestion is idempotent on `event_id`: a batch that was stored but whose
answer was lost comes back as `duplicate` on the next attempt.

## Options

| Option | Default | Meaning |
|---|---|---|
| `WithQueueSize(n)` | 10000 | Events that may wait in memory. The memory bound of the client |
| `WithBatchSize(n)` | 100 | Events per request, at most 500 (the service limit) |
| `WithFlushInterval(d)` | 1s | Longest a partial batch waits |
| `WithBackoff(min, max)` | 200ms, 30s | First and longest wait between retries |
| `WithHTTPClient(h)` | 10s timeout | Transport, proxy and TLS settings |
| `WithLogger(l)` | discard | Where delivery problems are logged |

`Stats()` returns `Queued`, `Sent`, `Rejected` and `Dropped`. Once `Close` has returned, every
recorded event is in exactly one of the last three.

## Decisions and tradeoffs

- **`Record` cannot fail, so loss is visible only in `Stats` and the log.** The alternative,
  returning an error or blocking when the queue is full, would push metering failures into the
  request path of the customer's product. A caller that needs to know polls `Stats().Dropped`.
  The telemetry task can export it as a metric.
- **Drop the oldest, not the newest.** In a long outage the queue ends up holding the most recent
  events. Dropping the newest would be equally lossy; oldest-first keeps what is most likely still
  relevant to a live dashboard and is what the task asked for. Either way the monthly
  reconciliation against the provider's report shows the gap.
- **A batch is retried without an attempt limit.** The bound on memory is the queue, not a retry
  count. While one batch is being retried, new events wait in the queue, so an outage shorter than
  the time it takes to fill the queue loses nothing. With an attempt limit, an outage longer than
  a few backoff steps would throw away batch after batch that the queue had room to keep. The cost
  is that one batch can stay in flight for the whole outage, and `Close` needs a deadline. This is
  Q17 in [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md).
- **One worker, one request at a time.** Order is kept and a struggling service gets no more than
  one connection per client. At the default batch size and the service's default rate limit
  (100 requests per second per key) that is far more throughput than one process produces.
- **Events are lost if the process dies.** The queue is memory only. A disk spool would survive a
  crash but adds files, fsync and cleanup to a library whose job is to stay out of the way.
- **No dependencies outside the standard library**, so importing the client does not pull the
  server's dependencies into a customer's build.
- **Nothing sensitive is logged.** Log lines carry counts, HTTP status and the attempt number.
  They never include the API key, the endpoint, or any event field.
- **A `200` whose body cannot be read counts the batch as sent.** The service only answers `200`
  after it has processed every event, so re-sending would only produce duplicates.

## Tests

- `client/client_test.go`, against an `httptest` server: the request path, key header and JSON
  shape; a full batch goes out at once and a partial one after the interval; retries with growing
  backoff on `5xx`, `408` and transport errors; `Retry-After` on `429`; no retry on `400`, `401`,
  `403`, `404`, `413`; per-event results counted; `Record` returning immediately and dropping the
  oldest while the service hangs; concurrent `Record` calls (no event lost or sent twice); `Close`
  flushing, giving up at its deadline, and aborting a hanging request; and that the key, endpoint
  and event fields never reach the log.
- `client/queue_test.go`: FIFO order, drop-oldest and wrap-around of the ring buffer.
- `client/contract_test.go`: the client against the service's real router, auth middleware and
  batch handler with an in-memory store. The handler rejects unknown fields, so a JSON field that
  drifts between client and service fails here.
