# Hourly rollups

`hourly_usage` holds one row per `(org_id, tenant_id, provider, model, feature, hour_start)` with
event count, input, output, cache read and cache write token sums, and latency sum and max. Hours
are UTC.

- **`org_id` leads the primary key**, so two orgs that use the same `tenant_id` and model never
  share a row, and every per-org read is a prefix scan of the key. It references `orgs(id)`.
- **`provider` and `feature` are dimensions**, copied from the event byte for byte (Q13). `feature`
  is `''` when the event did not set one, because a primary key column cannot be `NULL`.
- **The three token sums do not overlap.** `input_tokens` leaves out cache tokens (Q14), so total
  input for a row is `input_tokens + cache_read_tokens + cache_write_tokens`.

## Recompute, not increment

Each rollup row is a pure function of the `events` table. `Rollup(from, to)` runs
one `INSERT ... SELECT ... GROUP BY ... ON CONFLICT DO UPDATE` per hour that
overwrites the row with freshly aggregated values.

- **Idempotent:** running a window twice gives the same rows. There is no state to double-count.
- **Repairable:** any row can be rebuilt from raw events, so a missed or wrong window is fixed by re-running it.
- **Late events:** an event that arrives after its hour was rolled up is included on the next run of that hour.
- **Cost:** every run re-scans the hour's events (indexed on `event_time`). Incremental counters would be O(1) per event but are only correct if each update runs exactly once, which retries, crashes and re-runs break. Incremental or streaming aggregation is the next step if volume outgrows the scan.

Latency is stored as sum and max so rows stay mergeable. Percentiles are out of scope for v1.

## How rollups run

| Trigger | What it does |
| --- | --- |
| Startup | `BackfillStale` finds hours with events but no rollup row, or a row older than the newest event in that hour (`source_ingested_at`), and recomputes them. Covers downtime and first run. |
| Scheduler | Every `ROLLUP_INTERVAL` (default `1m`) re-rolls the previous and current hour, so late events are picked up. Stops on graceful shutdown. |
| Manual | `server backfill --from <RFC3339> --to <RFC3339>` recomputes an explicit window. Bounds are widened to whole hours; `--to` is exclusive. Needs `DATABASE_URL`. |

Each hour is one atomic statement, retried up to 5 times with exponential backoff
(200ms, doubling), so an interrupted run leaves only whole hours behind.

## Migration

`00006_hourly_usage_org_dimensions.sql` adds the columns and changes the primary key. It **deletes
all rows in `hourly_usage`**, because the old rows have no org and may mix several. No usage is
lost: `events` is untouched, and the startup scan finds every hour stale and rebuilds it. On a
large `events` table that first startup scan re-rolls every hour that has events.

## Tests

- `internal/rollup/rollup_integration_test.go`: idempotent re-runs and late events; two orgs with
  the same `event_id`, tenant, provider, model and feature get separate rows; provider and feature
  each split a row; cache tokens are summed; a new event in one org makes the hour stale without
  changing the other org's numbers.
- `internal/storage/migrate_integration_test.go`: migration 6 applied to a database that already
  has an event and a rollup row, the new key accepts and rejects the right rows, then down and up
  again.

## Known limits

- **The rollup job is not per org.** One statement recomputes an hour for every org, and one stale bucket in any org re-rolls that hour for all of them. Rows never mix orgs, so this costs work, not correctness. Per-org scheduling is only worth it if one org's volume starts to delay the others.
- **More dimensions mean more rows.** A tenant that sends many distinct `feature` values gets one row per value per hour. Nothing caps the number of distinct values yet.
- Events older than the scheduler's two-hour window are only picked up by the startup scan or a manual backfill.
- The startup scan aggregates all of `events` to find stale hours. Fine for v1; it should be bounded by a time range as data grows.
