# Event ingestion

Ingestion is idempotent on `event_id`, so clients can retry safely (at-least-once delivery).

`ingest.Store.Ingest` runs one statement,
`INSERT ... ON CONFLICT (event_id) DO NOTHING RETURNING event_id`:

| Outcome | Condition | HTTP |
|---|---|---|
| `Created` | the row was inserted | `201` |
| `Duplicate` | `event_id` exists with an identical payload | `200` |
| `ErrConflict` | `event_id` exists with any differing field, including `timestamp` | `409` |
| `*ValidationError` | the payload failed validation | `400` |

When no row is returned, the store reads the existing row and compares every field. Postgres makes
a concurrent inserter of the same `event_id` wait for the first transaction to commit, so the
read sees the winning row and N concurrent retries produce exactly one `Created`.

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

Migrations live in `internal/storage/migrations` (goose, embedded in the binary). Queries are in
`internal/storage/queries`; run `make generate` to rebuild `internal/storage/gen` with sqlc.

## Tests

Integration tests use testcontainers and skip when Docker is unavailable. CI runs them.
