# Event ingestion

Every event belongs to one org, and ingestion is idempotent on `(org_id, event_id)`, so clients
can retry safely (at-least-once delivery).

`ingest.Store.Ingest(ctx, org, event)` runs one statement,
`INSERT ... ON CONFLICT (org_id, event_id) DO NOTHING RETURNING event_id`:

| Outcome | Condition | HTTP |
|---|---|---|
| `Created` | the row was inserted | `201` |
| `Duplicate` | the org already has `event_id` with an identical payload | `200` |
| `ErrConflict` | the org already has `event_id` with any differing field, including `timestamp` | `409` |
| `*ValidationError` | the payload failed validation | `400` |

When no row is returned, the store reads the org's existing row and compares every field. Postgres
makes a concurrent inserter of the same `(org_id, event_id)` wait for the first transaction to
commit, so the read sees the winning row and N concurrent retries produce exactly one `Created`.

## Org scoping

- **The org comes from the API key, never from the payload.** The auth middleware puts the key's
  org in the request context and the handler passes it to the store. The payload has no org field,
  and because unknown fields are rejected, a request that sends `org_id` gets `400`.
- **`event_id` is unique per org, not globally.** Two orgs can use the same `event_id` (for example
  both use a provider's response id scheme, or both count from 1). Neither can see, duplicate or
  conflict with the other's event: every read and write of `events` filters on `org_id`.
- **The store takes the org as an argument**, `Ingest(ctx, org, event)`, instead of reading it from
  the context itself. A caller that forgets the org does not compile. The handler refuses with
  `500` if it is ever reached without the auth middleware, so an event is never stored without an
  org.
- **`events.org_id` references `orgs(id)`.** An event cannot be stored under an org that does not
  exist, and an org that has events cannot be deleted.
- **Rollups are per org too.** `hourly_usage` is keyed by `org_id` first. See
  [rollups.md](rollups.md).

Timestamps are normalized to UTC and truncated to microseconds, which is Postgres `timestamptz`
precision. Otherwise a re-sent event would never compare equal to its stored copy.

## Event fields

| Field | Required | Rule |
|---|---|---|
| `event_id` | yes | 1 to 256 bytes. Unique per org |
| `tenant_id` | yes | 1 to 256 bytes |
| `provider` | yes | 1 to 256 bytes, for example `anthropic` |
| `model` | yes | 1 to 256 bytes |
| `feature` | no | At most 256 bytes. Left out or `""` means not set |
| `input_tokens` | no, default 0 | `>= 0`. Does not include cache tokens |
| `output_tokens` | no, default 0 | `>= 0` |
| `cache_read_tokens` | no, default 0 | `>= 0` |
| `cache_write_tokens` | no, default 0 | `>= 0` |
| `latency_ms` | no, default 0 | `>= 0` |
| `timestamp` | yes | RFC 3339 |

- **`provider`, `tenant_id`, `model` and `feature` are free-text labels.** They are stored and
  compared byte for byte, with no trimming or case folding: `Anthropic` and `anthropic` are two
  providers. Pick one spelling per provider and keep it. Whether the service should normalize
  `provider` is Q13 in [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md).
- **The four token counts do not overlap.** `input_tokens` is the input that was neither read from
  nor written to a cache, so a call's total input is `input_tokens + cache_read_tokens +
  cache_write_tokens`. They are separate because providers price them at different rates. Some
  providers report an input count that already includes cached tokens; whatever records the event
  must subtract them, or those tokens are billed twice (Q14).
- **`feature` unset is stored as `''`, not `NULL`.** It becomes part of the `hourly_usage` primary
  key, which cannot hold `NULL`. So leaving `feature` out and sending `""` are the same event.
- **All eleven fields count for the `409` rule.** A re-send that changes `provider`, `feature` or a
  cache token count, including adding a `feature` to an event first sent without one, is a
  conflict, not a duplicate.

## HTTP API

`POST /v1/events` takes one event as JSON. Unknown fields, trailing data and bodies over 1 MiB are
rejected (`400`, `400`, `413`).

The request needs an API key with the `ingest` scope in `Authorization: Bearer <key>`. Without one
the answer is `401`, and with a `read` key it is `403`. See
[api-keys.md](api-keys.md#http-authentication) for creating a key and for the error bodies.

```sh
curl -i -X POST localhost:8080/v1/events -H "Authorization: Bearer $KEY" -d '{
  "event_id": "evt-1", "tenant_id": "tenant-a", "provider": "anthropic", "model": "claude-sonnet",
  "feature": "chat", "input_tokens": 100, "output_tokens": 50,
  "cache_read_tokens": 30, "cache_write_tokens": 20, "latency_ms": 420,
  "timestamp": "2026-01-02T03:04:05Z"
}'
```

Responses are JSON: `{"status":"created"}` / `{"status":"duplicate"}` on success, and
`{"error": "...", "fields": {"model": "required"}}` for validation failures. Unexpected store
errors return `500` with a generic message; details go to the log only.

### Batch

`POST /v1/events/batch` takes 1 to 500 events in one request, with the same key and scope. It is
what the [client library](client.md) uses, and the proxy through it.

```sh
curl -i -X POST localhost:8080/v1/events/batch -H "Authorization: Bearer $KEY" -d '{
  "events": [
    {"event_id": "evt-1", "tenant_id": "tenant-a", "provider": "anthropic", "model": "claude-sonnet",
     "input_tokens": 100, "output_tokens": 50, "timestamp": "2026-01-02T03:04:05Z"},
    {"event_id": "evt-2", "tenant_id": "tenant-a", "provider": "anthropic"}
  ]
}'
```

Each event follows the rules of the single endpoint and gets its own result, in request order:

```json
{"results": [
  {"event_id": "evt-1", "status": "created"},
  {"event_id": "evt-2", "status": "invalid", "error": "validation failed",
   "fields": {"model": "required", "timestamp": "required"}}
]}
```

| `status` | Meaning | Single-endpoint equivalent |
|---|---|---|
| `created` | stored for the first time | `201` |
| `duplicate` | already stored with an identical payload | `200` |
| `conflict` | the org already has this `event_id` with a different payload; nothing changed | `409` |
| `invalid` | failed validation (`fields` says which), or could not be decoded: wrong type, unknown field, not an object | `400` |

- **The HTTP status describes the batch, not the events.** A readable batch answers `200` even if
  every event in it was rejected, so a client must read `results`. No per-event status is worth
  retrying: all four are final.
- **One bad event does not fail the batch.** Each element of `events` is decoded on its own, so an
  event with a string where a number belongs is `invalid` in its slot and the events after it are
  still stored. `event_id` is echoed when it could be read and is `""` otherwise; match by position
  in that case.
- **The whole request is rejected, and nothing is stored**, when the body is not a JSON object with
  only an `events` array (`400`), when `events` is empty or has more than 500 entries (`400`), or
  when the body is over 4 MiB (`413`). 500 events with every field at its byte limit fit in well
  under that.
- **An `event_id` repeated inside one batch** is handled in order: the first is `created`, an
  identical repeat is `duplicate`, a different payload is `conflict`.
- **A batch is not a transaction.** Events are written one statement at a time, each committed on
  its own. If the database fails partway, the request answers `500` with no results, the events
  before the failure stay stored, and re-sending the same batch reports them as `duplicate`. That
  is the same at-least-once contract as the single endpoint. The alternative, one multi-row insert,
  is Q15 in [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md).

### Rate limit

Each API key has a token bucket shared by `POST /v1/events` and `POST /v1/events/batch`. A request
over the limit gets `429` with a `Retry-After` header in whole seconds and stores nothing:

```
HTTP/1.1 429 Too Many Requests
Retry-After: 1

{"error":"rate limit exceeded, retry after the number of seconds in Retry-After"}
```

| Variable | Default | Meaning |
|---|---|---|
| `INGEST_RATE_LIMIT_RPS` | `100` | Requests per second per key, averaged. Fractions are allowed (`0.5` is one request every two seconds). `0` turns the limit off |
| `INGEST_RATE_LIMIT_BURST` | one second of the rate, rounded up, at least 1 | Size of the bucket: how many requests a key may send at once |

A value that does not parse stops the server at startup instead of falling back to the default.

- **A bucket starts full**, each request takes one token, and tokens come back at the configured
  rate up to the burst. `Retry-After` is the time until the next token, rounded up, so a client
  that waits that long is let through (unless another client of the same key got there first).
- **`429` is the only ingest answer worth retrying later unchanged.** Ingestion is idempotent, so
  re-sending after the wait is safe. The [client library](client.md) honours `Retry-After`.
- **The limit counts requests, not events.** A batch of 500 costs one token, the same as a single
  event, so the default allows up to 50,000 events per second per key through the batch endpoint.
  It protects the service from a client stuck in a retry loop; it is not a usage quota (that is
  the "Plan limits" task). Counting events instead is Q16 in
  [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md).
- **The bucket belongs to the key, not the org.** Two keys of one org are limited separately, so
  one noisy deployment does not starve another. An org's total is the sum over its keys.
- **The check runs after authentication and before the body is read.** A request with a missing,
  unknown, revoked or wrong-scope key gets `401` or `403` and spends no token, so an attacker
  without a key cannot use up a customer's limit. It also means unauthenticated traffic is not
  limited here; that belongs to the load balancer in front of the service.
- **Buckets live in the memory of one process.** They are empty after a restart (every key gets a
  full burst again), and with N replicas a key can reach N times the limit. That is acceptable
  while the service runs as one replica; a shared limiter would add a network call to every
  request. Idle buckets are dropped once they would be full again, so memory is bounded by the
  number of keys active within one refill period.
- **The bucket is a small in-repo type, `internal/ratelimit`**, not `golang.org/x/time/rate`. It
  is under 100 lines, needs no new dependency, and takes a clock, so its tests do not sleep.

The server needs `DATABASE_URL` and applies migrations at startup. `LISTEN_ADDR` defaults to `:8080`.

## Schema and queries

`events` has primary key `(org_id, event_id)` and two indexes:

| Index | Used by |
|---|---|
| `events_org_tenant_time_idx (org_id, tenant_id, event_time)` | Per-org reads by tenant and time window |
| `events_time_idx (event_time)` | The rollup, which scans one hour across all orgs |

`events_time_idx` deliberately does not lead with `org_id`. The scheduler recomputes an hour for
every org in one statement, and an index that starts with `org_id` cannot serve a range scan on
time alone.

Migration `00004_events_org_scope.sql` adds `org_id` and changes the primary key. It **deletes
all rows in `events` and `hourly_usage`**: events stored before orgs existed cannot be attributed
to an org, and no production data exists. A local database from an earlier quickstart upgrades
cleanly but loses its events.

Migration `00005_events_usage_fields.sql` adds `provider`, `cache_read_tokens`,
`cache_write_tokens` and `feature`. It keeps existing rows: they get provider `unknown`, zero cache
tokens and no feature. The `unknown` default exists only for that backfill and is dropped in the
same migration, so an insert without a provider fails. Re-sending an old event with its real
provider is a `409`, because the stored copy says `unknown`. **This is a breaking API change:** a
client that does not send `provider` now gets `400`.

Migrations live in `internal/storage/migrations` (goose, embedded in the binary). Queries are in
`internal/storage/queries`; run `make generate` to rebuild `internal/storage/gen` with sqlc.

## Tests

Integration tests use testcontainers and skip when Docker is unavailable. CI runs them.

- `internal/ingest/event_test.go`: validation of every field, including the byte limit on
  `feature`, and that `sameAs` compares every field. That test counts the fields of `Event`, so
  adding a field without adding it to the comparison fails.
- `internal/ingest/store_integration_test.go`: created, duplicate and conflict within an org (one
  conflict case per field), the new fields reach their columns and default to zero and empty, the
  same `event_id` in two orgs with different payloads, an unknown org, and concurrent retries.
- `internal/api/events_test.go`: the handler takes the org from the request context, rejects
  `org_id` in the payload, and refuses to run without an identity.
- `internal/api/events_integration_test.go`: over HTTP, a second org reuses an `event_id` and gets
  `201`, and each row is stored under the org of the key that sent it.
- `internal/api/batch_test.go`: one result per event in request order across every outcome,
  undecodable events never reach the store, the 500-event and body-size limits, and a store
  failure answering `500` without partial results.
- `internal/api/batch_integration_test.go`: over HTTP against Postgres, a mixed batch stores only
  its good events, ids repeated inside a batch, a re-sent batch stores nothing new, a second org
  reusing the same ids, and a full batch of 500.
- `internal/ratelimit/limiter_test.go`: burst then refill on a fake clock, the advertised wait is
  enough, refill stops at the burst, keys are independent, idle buckets are dropped, and
  concurrent callers never get more than the burst.
- `internal/api/ratelimit_test.go`: `429` with `Retry-After` on both routes from one bucket, a
  refused request never reaches the store, `401` and `403` spend nothing, and a second key is
  unaffected.
- `internal/api/ratelimit_integration_test.go`: over HTTP against Postgres, two stored keys of one
  org have separate buckets and the refused event is not in `events`.
- `internal/api/isolation_integration_test.go`: the milestone 1 acceptance test (see
  [DESIGN.md](DESIGN.md), build order). Over HTTP against Postgres, two orgs send through both
  ingest routes with their own keys, reusing the same event ids and tenant ids with different
  token counts, and the rollup runs. Every row of `events` and `hourly_usage` is read back per
  org, so a row stored or summed under the wrong org fails the comparison. A revoked key (`401`)
  and a `read` key (`403`) store nothing on either route, and the rows are unchanged after a
  forced recompute.
- `cmd/server/ratelimit_test.go`: the environment variables, their defaults, `0` for off, and
  startup errors for values that do not parse.
- `internal/storage/migrate_integration_test.go`: migration 4 applied to a database that already
  has events and rollups, then down and up again. Migration 5 applied to a database that already
  has an event: the row is kept with provider `unknown`, an insert without a provider or with a
  negative cache count fails, then down and up again.
