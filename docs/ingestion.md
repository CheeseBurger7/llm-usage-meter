# Event ingestion

Ingestion is idempotent on `event_id`, so clients can retry safely (at-least-once delivery).

`ingest.Store.Ingest` runs one statement,
`INSERT ... ON CONFLICT (event_id) DO NOTHING RETURNING event_id`:

| Outcome | Condition | HTTP (next PR) |
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

## Schema and queries

Migrations live in `internal/storage/migrations` (goose, embedded in the binary). Queries are in
`internal/storage/queries`; run `make generate` to rebuild `internal/storage/gen` with sqlc.

## Tests

Integration tests use testcontainers and skip when Docker is unavailable. CI runs them.
