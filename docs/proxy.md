# Capture proxy

`cmd/proxy` records usage from real LLM calls without the app writing event code. The customer
runs it next to their app and points the provider's base URL at it. It forwards every request and
response unchanged, reads the token counts from the response and sends one event per call through
the [Go client library](client.md). A call that the provider retried on a fallback model can give
more than one event, see [Fallback attempts](#fallback-attempts).

Today it meters **Anthropic Messages calls** (`POST /v1/messages`), streamed or not, and
**non-streamed OpenAI-compatible chat completions** (`POST /v1/chat/completions`), see
[Chat completions](#chat-completions). Streamed chat completions are the next task in
[TASKS.md](TASKS.md); until then they are forwarded and counted as skipped.

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
| `CHAT_COMPLETIONS_PROVIDER` | from the upstream host, see below | `provider` label on events from chat completions |
| `PROXY_DEFAULT_TENANT` | `_unassigned` | `tenant_id` of calls without `X-Meter-Tenant`. Set it to empty to forward those calls without metering. See [Untagged calls](#untagged-calls) |
| `PROXY_BILLED_REFUSAL_CATEGORIES` | `bio,frontier_llm,reasoning_extraction` | Comma-separated refusal categories the provider bills before any output. Replaces the built-in list; set to empty to bill none. See [Refusals](#refusals) |

| Header | Required | Meaning |
| --- | --- | --- |
| `X-Meter-Tenant` | no, but needed to bill the right tenant | `tenant_id` of the event |
| `X-Meter-Feature` | no | `feature` of the event |

Both headers are removed before the request goes to the provider. `GET /healthz` is answered by
the proxy itself.

## What is recorded

One event per call that the provider answered with `200` and a JSON message or a complete event
stream. For a JSON message:

| Event field | Source |
| --- | --- |
| `event_id` | response `id` (the provider's message id, so a re-sent event dedupes) |
| `tenant_id`, `feature` | `X-Meter-Tenant` (or `PROXY_DEFAULT_TENANT` when it is missing), `X-Meter-Feature` |
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

## Streamed responses

With `"stream": true` the provider answers with server-sent events (`text/event-stream`). The
proxy passes each chunk to the caller as it arrives and scans the same bytes for three events:

| Stream event | What the proxy reads |
| --- | --- |
| `message_start` | `message.id` (the `event_id`), `message.model`, and the first counts in `message.usage` |
| `message_delta` | `usage`, with the same four count names as above. Each count that is present replaces the value seen so far. Also `delta.stop_reason` and `delta.stop_details`, for [refusals](#refusals) |
| `message_stop` | Nothing: it marks the stream as complete, and `latency_ms` ends here |

The provider's [Streaming messages](https://platform.claude.com/docs/en/build-with-claude/streaming)
page, checked on 2026-11-15, states that the counts in `message_delta` are cumulative, so they are
taken as totals and never added up. A `message_delta` may carry only `output_tokens`; the input
and cache counts then stay as `message_start` reported them. `ping`, content events and event
types the proxy does not know are ignored.

One event is recorded when the stream reaches `message_stop`. A stream that does not get there is
forwarded like any other and not metered (Q19): the caller hung up, the connection dropped, or the
provider sent an `error` event.

## Fallback attempts

The provider can retry a call that a model declined on a fallback model, inside the same request.
The response is still one message with one `id`, but more than one model ran. The rules below
come from the provider's
[Refusals and fallback](https://platform.claude.com/docs/en/build-with-claude/refusals-and-fallback)
page, checked on 2026-11-16:

- `usage.iterations` lists every attempt with its own `model` and the four counts. A model that
  declined is a `message` entry; the model that produced the returned message is the
  `fallback_message` entry.
- The top-level `model` and `usage` counts describe only the attempt that produced the returned
  message. Tokens from different models are never summed into one field.
- Every attempt that produced output is billed separately, at the rates of the model that ran it,
  including one that declined partway through.
- An attempt that declined before any output is billed only when its refusal category is one of
  the billed ones. Its tokens are listed in `usage.iterations` either way.

The proxy records, for streamed and non-streamed calls alike:

| Attempt | Event | `event_id` |
| --- | --- | --- |
| The one that produced the returned message | Yes, from the top-level `model` and `usage`, as for any call | the message `id` |
| Declined after producing output (`message` entry with `output_tokens` above 0) | Yes, with the entry's own `model` and counts | `<message id>/attempt-<n>`, where `n` is the entry's position in `usage.iterations`, counted from 1 |
| Declined before any output (`message` entry with `output_tokens` of 0) | No, see below | |

All events of one call carry the same `tenant_id`, `feature` and `timestamp`. `latency_ms` is the
whole call's on the serving event and `0` on the others: the provider reports no duration per
attempt, and repeating the call's latency would count it several times in the hourly sums.

In a stream, the entries are read from the last `message_delta` that carries `usage.iterations`.
When the decline happened in the middle of the stream, `message_start` has already named the model
that declined, so the serving model is taken from the `fallback_message` entry.

`usage.iterations` is read only when it holds a `fallback_message` entry. Other features use the
same array for other things, and a `message` entry there is not a declined attempt.

**Not recorded yet: an attempt that declined before any output.** Whether the provider bills it
depends on the refusal category, which is not in the usage block but in a `fallback` block in
`content` that the proxy does not decode yet. The proxy does not guess: the attempt gives no
event and the proxy logs how many it left out. Where such an attempt was billed, the tenant is
under-billed by its input tokens until reconciliation shows the gap. Q21 in
[OPEN_QUESTIONS.md](OPEN_QUESTIONS.md) settled it: a follow-up task in [TASKS.md](TASKS.md)
applies the [Refusals](#refusals) rule to these attempts too.

## Refusals

A safety classifier can decline a call. The provider then answers `200` with
`stop_reason: "refusal"` and a `stop_details` object whose `category` names the policy area.
Per the provider's
[Refusals and fallback](https://platform.claude.com/docs/en/build-with-claude/refusals-and-fallback)
page, checked on 2026-11-20:

- A refusal after some output is billed: input and the output already produced.
- A refusal before any output is billed only when its category is `bio`, `frontier_llm` or
  `reasoning_extraction`. The other documented categories, `cyber` and `general_harms`, and a
  `null` category are not billed. The provider states this list may change.

So for a call whose returned attempt ended in a refusal with `output_tokens` of 0, the proxy
reads the category (from the top-level `stop_details`, or in a stream from `delta.stop_details`
of `message_delta`, next to `stop_reason`) and decides (Q21). The provider's docs show no streamed
refusal; the stream path comes from `MessageDeltaEventDelta` in the provider's Go SDK
(`anthropics/anthropic-sdk-go`, `message.go` at `cbcdbf6`, checked 2026-11-20), which has no
top-level `stop_details` on the event (Q25):

| Category | What happens | Counter in the shutdown log |
| --- | --- | --- |
| On the billed list | Recorded like any other call | `calls_recorded` |
| Documented but not on the list | Forwarded, not recorded, logged at info with the category | `refusals_unbilled` |
| Missing, `null` or not documented | Forwarded, not recorded, logged as a warning with the category | `refusals_unknown_category` |

A refusal with output above 0 is recorded whatever its category. Nothing in `content` is read
for this: the category comes from `stop_details` only. The category names a policy area, not
anything from the request, so it is logged.

The billed list is built in and replaced by `PROXY_BILLED_REFUSAL_CATEGORIES`, so a customer can
follow a change at the provider without a new proxy release. Entries must be lowercase letters,
digits or underscores, or the proxy refuses to start. A category on the replaced list is billed
even if the proxy does not know it, which covers a category the provider adds. The startup log
line shows the list in effect.

What can still go wrong: when the provider stops billing a category and the list has not caught
up, tenants are charged for those refusals; compare the date above with the provider page. A
category the provider starts billing lands under `refusals_unknown_category` and is under-billed
until the list is updated; the counter and reconciliation show it.

When every attempt of a fallback call was declined, the attempts that declined after producing
output are recorded as in [Fallback attempts](#fallback-attempts) even if the final refusal is not
billed.

## Chat completions

The OpenAI chat completions shape is served by many providers and self-hosted servers, so the
response does not say who served it. The `provider` label comes from configuration instead:

| `UPSTREAM_URL` host | `provider` |
| --- | --- |
| `api.openai.com` | `openai` |
| `api.anthropic.com` (the provider's OpenAI SDK compatibility layer) | `anthropic` |
| any other host, such as a gateway or a vLLM server | none: chat completions are forwarded and counted as skipped |

`CHAT_COMPLETIONS_PROVIDER` overrides the table, for example `CHAT_COMPLETIONS_PROVIDER=together`.
Its `A`-`Z` is lowercased, as ingest does with every provider (Q13), and the result must match
ingest's rule `^[a-z0-9._-]{1,256}$` (Q20): otherwise the proxy refuses to start, since ingest
would reject every chat completion event it sends. The error names the variable, not its value. The proxy never guesses a label for an
unknown host, because the label picks the price book entry (Q23). The Messages endpoint is always
labelled `anthropic`.

Such calls are unbilled, so Q23 also adds a startup warning when the host is unknown and
`CHAT_COMPLETIONS_PROVIDER` is empty; until that task lands, the only signs are the skip counter,
the per-call log lines and reconciliation.

For a non-streamed chat completion (`"object": "chat.completion"`):

| Event field | Source |
| --- | --- |
| `event_id` | response `id` for known provider hosts; see Q24 for other hosts |
| `tenant_id`, `feature` | `X-Meter-Tenant`, `X-Meter-Feature` |
| `provider` | as above |
| `model` | response `model` |
| `input_tokens` | `usage.prompt_tokens` minus `usage.prompt_tokens_details.cached_tokens` |
| `output_tokens` | `usage.completion_tokens` |
| `cache_read_tokens` | `usage.prompt_tokens_details.cached_tokens` (absent or `null` counts as 0) |
| `cache_write_tokens` | always 0, see below |
| `latency_ms`, `timestamp` | as for Messages calls |

The field names were checked on 2026-11-17 against the `ChatCompletion` and `CompletionUsage`
types that the [official OpenAI Python library](https://github.com/openai/openai-python) generates
from OpenAI's API spec (`src/openai/types/chat/chat_completion.py` and
`src/openai/types/completion_usage.py`); OpenAI's documentation site could not be reached from the
environment that wrote this. The provider's
[OpenAI SDK compatibility](https://platform.claude.com/docs/en/api/openai-sdk) page, checked the
same day, lists `usage.prompt_tokens` and `usage.completion_tokens` as supported and
`prompt_tokens_details` as always empty, since prompt caching is not offered there.

- **`prompt_tokens` includes cached tokens.** The spec describes `cached_tokens` as "cached tokens
  present in the prompt". The event schema keeps the two apart (Q14), so the proxy subtracts. A
  response with more cached tokens than prompt tokens is skipped rather than given a negative
  count.
- **`completion_tokens` includes reasoning tokens**, which the provider bills as output, and
  rejected prediction tokens, which the spec says are billed too. It is copied as it is.
- **`prompt_tokens_details.cache_write_tokens` is not read.** The spec calls it "the unadjusted
  number of prompt tokens written to cache" and does not say whether it is part of
  `prompt_tokens`. Leaving those tokens in `input_tokens` bills each token once whichever way it
  is, at the input rate. Q22 settled this as the rule; a provider that prices cache writes above
  input is under-billed by the difference until reconciliation shows it.
- **Audio and image tokens** are part of `prompt_tokens` and `completion_tokens` and are billed as
  text tokens. The event schema has no field for them.
- **`service_tier`** (flex, priority) changes the provider's price but is not recorded.

## What is never recorded

- **Request and response bodies.** The response is decoded into a struct with only `type`, `id`,
  `model`, the four usage counts and, per entry of `usage.iterations`, its `type`, `model` and
  counts. A chat completion is decoded into `object`, `id`, `model`, `prompt_tokens`,
  `completion_tokens` and `cached_tokens`; `choices` is never decoded. Nothing else is kept, logged or sent. From a stream, only
  the data of `message_start` and `message_delta` events is decoded, and neither carries content.
- **Provider keys.** The proxy forwards the caller's headers and never reads them. Log lines hold
  a method, a path without its query string, and a reason; no header values.
- **The ingest key** goes only to the metering service, never to the provider.

`TestCallThroughProxyReachesTheService` checks every byte sent to the service, and
`TestLogsNeverContainBodiesOrKeys` checks the log, for the prompt, the completion and the provider
key.

## Untagged calls

A call without `X-Meter-Tenant` is still forwarded unchanged and metered under the tenant in
`PROXY_DEFAULT_TENANT`, by default the reserved tenant `_unassigned` (Q18). This covers every
event of the call: streamed or not, Messages or chat completions, and declined fallback attempts.
Each such call logs `call metered under the default tenant` with its reason (never a header
value) and is counted in `calls_untagged` in the shutdown log line. `calls_untagged` is part of
`calls_recorded`, not added to it. The default tenant is never sent to the provider.

With `PROXY_DEFAULT_TENANT` set to empty, untagged calls are forwarded without metering and
counted as skipped, as before Q18. A value over 256 bytes, the `tenant_id` limit in ingest, stops
the proxy at startup.

## Calls that are forwarded but not metered

Metering never changes or fails a call. Each case below is forwarded exactly as the provider
answered, logged with its reason and counted (`calls_skipped` in the shutdown log line):

| Case | Why |
| --- | --- |
| No `X-Meter-Tenant` header, only while `PROXY_DEFAULT_TENANT` is set to empty | An event needs a tenant. By default such calls are metered under `_unassigned`, see [Untagged calls](#untagged-calls) |
| Chat completion with no provider label configured | The proxy does not guess one. See Q23 |
| Streamed chat completion | Not supported yet, see [TASKS.md](TASKS.md) |
| Chat completion with more cached than prompt tokens | The counts contradict each other |
| Stream that ended before `message_stop`, or held an `error` event | The final counts were never seen. See Q19: once the partial streams task lands, these are recorded with `partial: true`. A stream that ended in an `error` event stays skipped unless the provider's docs confirm that its input is billed |
| Stream without `event:` lines, or with a usage event over 1 MiB | The proxy finds usage events by their event name |
| Compressed stream (`Content-Encoding` set) | The proxy cannot scan it |
| `200` that is neither JSON nor an event stream, or has no `id`, `model` and `usage` (for a chat completion: no `prompt_tokens` or `completion_tokens`) | Nothing to read |
| Response larger than 16 MiB | Capture limit, see below |
| JSON response with a `Content-Encoding` other than gzip | The proxy cannot read it |
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
- **A stream is scanned as it passes, not copied.** The scanner keeps the current line up to 128
  bytes, enough to read an `event:` name, and the full data of usage events only. Memory per
  streamed call stays in the kilobytes however long the answer is, and no chunk is held back, so
  time to first byte is the provider's (`TestStreamFirstByteIsNotDelayed`). The cost is a
  dependency on the provider's framing: the `event:` line must come before its `data:` line, as
  the provider sends it. A stream framed differently is forwarded and counted as skipped.
- **An unfinished stream will be recorded as partial, not skipped** (Q19). Today it is skipped.
  Before `message_stop` the proxy has exact input counts, but the output count is usually the
  1 to 3 tokens in `message_start`, so an event understates output by an unknown amount. Skipping
  loses the input cost as well. Recording without a marker would make an undercount look like a
  complete call. So the event carries `partial: true`, statements bill it as recorded (it can
  only undercount) and show how many partial calls each line includes, and reconciliation
  shows the missing output. A stream that ended in an `error` event is recorded only if the
  provider's docs confirm that its input is billed; otherwise it stays skipped.
- **A missing tenant header never fails the call** (Q18). Rejecting would let a metering mistake
  break the product. Such calls are metered under the reserved tenant `_unassigned`
  (`PROXY_DEFAULT_TENANT`, empty means skip), so the cost shows on the dashboard as lost margin
  instead of disappearing. Unset and empty differ on purpose: the safe default needs no setting,
  and an org that prefers the old behaviour can still choose it. The `proxy` package itself
  defaults to skipping (`Config.DefaultTenant` empty), so embedding it never invents a tenant;
  only `cmd/proxy` applies `_unassigned`.
  Statements leave `_unassigned` out, since nobody can be invoiced for it. Leaving the header
  off cannot be used to cheat: only the org's own backend calls the proxy, and an org that wants
  to hide usage can skip the proxy altogether. Reconciliation against the provider's report is
  the check on complete metering.
- **Loopback listen address by default.** The proxy does not authenticate callers: anyone who can
  reach it can send usage for any tenant under the org's ingest key. It holds no provider key, so
  it cannot be used to make free LLM calls. Set `LISTEN_ADDR` to expose it to other containers or
  hosts on a private network.
- **A declined attempt gets the message id plus its position, `<message id>/attempt-<n>`.** The
  attempts of a call share one message id, and the service dedupes on `event_id`, so each needs
  its own. The serving attempt keeps the bare message id: a call without a fallback has the same
  id as before, and a response seen by an older proxy dedupes against the same call seen by a
  newer one. The position in `usage.iterations` is the only per-attempt value the provider gives
  that is the same every time the response is read; a hash of the counts would also be stable but
  says nothing to someone reading a statement.
- **`event_id` is the provider's message id.** It is unique per call and identical if the same
  response is ever seen twice, so no id has to be invented. The service treats ids as unique per
  org. Chat completions also use the response `id` as it is, without a provider prefix: the
  hosted providers' ids differ in shape (`msg_...`, `chatcmpl-...`) and are unique per call, and a
  prefix would not help a self-hosted server whose ids repeat, since those repeat under one
  provider label. Q24 settled that once its task lands, other hosts get an id generated by the
  proxy (once per response, reused on retries), because a repeated server id would leave the
  second call unbilled as a `duplicate` or `conflict`. Conflicts are also counted, since only two
  calls sharing an id cause one.
- **The chat completions provider label is configured, never guessed** (Q23). A wrong label puts
  usage on the wrong price book entry and looks correct; a skipped call is in the log, the counter
  and reconciliation. Known provider hosts get their label without a setting, so the common case
  needs no configuration.
- **Compressed responses are forwarded compressed.** If the caller asked for gzip, the proxy keeps
  the provider's bytes and decompresses only its own copy.
