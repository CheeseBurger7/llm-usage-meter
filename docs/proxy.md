# Capture proxy

`cmd/proxy` records usage from real LLM calls without the app writing event code. The customer
runs it next to their app and points the provider's base URL at it. It forwards every request and
response unchanged, reads the token counts from the response and sends one event per call through
the [Go client library](client.md).

Today it meters **non-streaming Anthropic Messages calls** (`POST /v1/messages`). Streamed
responses and the OpenAI-compatible shape are the next two tasks in [TASKS.md](TASKS.md); until
then those calls are forwarded but not metered.

## Run it

```sh
export METER_ENDPOINT='https://meter.example.com'   # the metering service
export METER_INGEST_KEY='lum_ingest_...'            # a key with the ingest scope
go run ./cmd/proxy                                  # listens on 127.0.0.1:8081
```

Then send the provider call to the proxy with two extra headers. Everything else, including the
provider key, is what the app already sends:

```sh
curl http://127.0.0.1:8081/v1/messages \
  -H "x-api-key: $ANTHROPIC_API_KEY" -H 'anthropic-version: 2023-06-01' \
  -H 'content-type: application/json' \
  -H 'X-Meter-Tenant: tenant-a' -H 'X-Meter-Feature: chat' \
  -d '{"model": "claude-opus-5-5", "max_tokens": 256,
       "messages": [{"role": "user", "content": "Hello"}]}'
```

With an SDK, set its base URL to the proxy (for the Anthropic SDKs, `ANTHROPIC_BASE_URL`) and add
the two headers as default or per-request headers.

| Variable | Default | Purpose |
| --- | --- | --- |
| `METER_ENDPOINT` | required | Base URL of the metering service |
| `METER_INGEST_KEY` | required | API key with the `ingest` scope ([api-keys.md](api-keys.md)) |
| `UPSTREAM_URL` | `https://api.anthropic.com` | Provider base URL the proxy forwards to |
| `LISTEN_ADDR` | `127.0.0.1:8081` | Listen address |

| Header | Required | Meaning |
| --- | --- | --- |
| `X-Meter-Tenant` | to be metered | `tenant_id` of the event |
| `X-Meter-Feature` | no | `feature` of the event |

Both headers are removed before the request goes to the provider. `GET /healthz` is answered by
the proxy itself.

## What is recorded

One event per call that the provider answered with `200` and a JSON message:

| Event field | Source |
| --- | --- |
| `event_id` | response `id` (the provider's message id, so a re-sent event dedupes) |
| `tenant_id`, `feature` | `X-Meter-Tenant`, `X-Meter-Feature` |
| `provider` | `anthropic` |
| `model` | response `model` (the model that served the call, not the requested alias) |
| `input_tokens` | `usage.input_tokens` |
| `output_tokens` | `usage.output_tokens` |
| `cache_read_tokens` | `usage.cache_read_input_tokens` (`null` counts as 0) |
| `cache_write_tokens` | `usage.cache_creation_input_tokens` (`null` counts as 0) |
| `latency_ms` | from the request reaching the proxy to the last response byte read |
| `timestamp` | when the request reached the proxy |

The field names were checked against the provider's
[Create a Message](https://platform.claude.com/docs/en/api/messages/create) reference on
2026-11-14. It states that total input is the sum of `input_tokens`,
`cache_creation_input_tokens` and `cache_read_input_tokens`, so `input_tokens` already excludes
cache tokens. That is the meaning the event schema uses (Q14), and the counts are copied as they
are.

## What is never recorded

- **Request and response bodies.** The response is decoded into a struct with only `type`, `id`,
  `model` and the four usage counts. Nothing else is kept, logged or sent.
- **Provider keys.** The proxy forwards the caller's headers and never reads them. Log lines hold
  a method, a path without its query string, and a reason; no header values.
- **The ingest key** goes only to the metering service, never to the provider.

`TestCallThroughProxyReachesTheService` checks every byte sent to the service, and
`TestLogsNeverContainBodiesOrKeys` checks the log, for the prompt, the completion and the provider
key.

## Calls that are forwarded but not metered

Metering never changes or fails a call. Each case below is forwarded exactly as the provider
answered, logged with its reason and counted (`calls_skipped` in the shutdown log line):

| Case | Why |
| --- | --- |
| No `X-Meter-Tenant` header | An event needs a tenant. See Q18 |
| Streamed response (`text/event-stream`) | Next task |
| `200` that is not a JSON message with `id`, `model` and `usage` | Nothing to read |
| Response larger than 16 MiB | Capture limit, see below |
| `Content-Encoding` other than gzip | The proxy cannot read it |
| Caller disconnected before the end | The usage block was never seen |

Not counted, because they carry no usage: provider errors (any status other than `200`) and every
other path, such as `/v1/messages/count_tokens`. The Message Batches API returns its usage later
through the batch results, so batch calls are not metered.

## Failure behaviour

| Failure | Result for the caller |
| --- | --- |
| Metering service down or slow | None. Events queue in the client library, which retries in the background and drops the oldest when its queue is full ([client.md](client.md)) |
| Ingest key revoked | None. Events are counted as rejected |
| Provider unreachable | `502` from the proxy, since there is no answer to forward |
| Proxy stops (SIGTERM) | In-flight calls finish (up to 15 s), then queued events are sent (up to 10 s) |

## Decisions and tradeoffs

- **`httputil.ReverseProxy` from the standard library**, not a hand-written forwarder. It already
  handles hop-by-hop headers, cancellation and flushing of streamed responses. No new dependency.
- **The usage block is read from a copy, after the response was forwarded.** Bytes go to the
  caller as they arrive and a copy is kept in memory; the JSON is decoded only once the last byte
  was written. The alternative, buffering the whole response and then forwarding it, would add the
  full generation time to time-to-first-byte. The cost is memory: up to one response per in-flight
  call, capped at 16 MiB each. A streaming JSON scanner would remove the copy and is worth doing
  if memory shows up in practice.
- **A missing tenant header skips metering instead of failing the call** (Q18). Rejecting would
  let a metering mistake break the product. The cost is that untagged calls are invisible to
  billing until someone reads the proxy log; reconciliation against the provider's report shows
  the gap.
- **Loopback listen address by default.** The proxy does not authenticate callers: anyone who can
  reach it can send usage for any tenant under the org's ingest key. It holds no provider key, so
  it cannot be used to make free LLM calls. Set `LISTEN_ADDR` to expose it to other containers or
  hosts on a private network.
- **`event_id` is the provider's message id.** It is unique per call and identical if the same
  response is ever seen twice, so no id has to be invented. The service treats ids as unique per
  org; when a second provider is added its ids could in theory collide, which the
  OpenAI-compatible task should settle.
- **Compressed responses are forwarded compressed.** If the caller asked for gzip, the proxy keeps
  the provider's bytes and decompresses only its own copy.
