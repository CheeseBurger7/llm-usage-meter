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
- **Rollups are not per org yet.** `hourly_usage` still groups by tenant and model only. See
  [rollups.md](rollups.md#known-limits).

Timestamps are normalized to UTC and truncated to microseconds, which is Postgres `timestamptz`
precision. Otherwise a re-sent event would never compare equal to its stored copy.

## HTTP API

`POST /v1/events` takes one event as JSON. Unknown fields, trailing data and bodies over 1 MiB are
rejected (`400`, `400`, `413`). `timestamp` is RFC 3339.

The request needs an API key with the `ingest` scope in `Authorization: Bearer <key>`. Without one
the answer is `401`, and with a `read` key it is `403`. See
[api-keys.md](api-keys.md#http-authentication) for creating a key and for the error bodies.

```sh
curl -i -X POST localhost:8080/v1/events -H "Authorization: Bearer $KEY" -d '{
  "event_id": "evt-1", "tenant_id": "tenant-a", "model": "claude-sonnet",
  "input_tokens": 100, "output_tokens": 50, "latency_ms": 420,
  "timestamp": "2026-01-02T03:04:05Z"
}'
```

Responses are JSON: `{"status":"created"}` / `{"status":"duplicate"}` on success, and
`{"error": "...", "fields": {"model": "required"}}` for validation failures. Unexpected store
errors return `500` with a generic message; details go to the log only.

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

Migrations live in `internal/storage/migrations` (goose, embedded in the binary). Queries are in
`internal/storage/queries`; run `make generate` to rebuild `internal/storage/gen` with sqlc.

## Tests

Integration tests use testcontainers and skip when Docker is unavailable. CI runs them.

- `internal/ingest/store_integration_test.go`: created, duplicate and conflict within an org, the
  same `event_id` in two orgs with different payloads, an unknown org, and concurrent retries.
- `internal/api/events_test.go`: the handler takes the org from the request context, rejects
  `org_id` in the payload, and refuses to run without an identity.
- `internal/api/events_integration_test.go`: over HTTP, a second org reuses an `event_id` and gets
  `201`, and each row is stored under the org of the key that sent it.
- `internal/storage/migrate_integration_test.go`: migration 4 applied to a database that already
  has events and rollups, then down and up again.
