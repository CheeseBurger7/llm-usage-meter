# Hourly rollups

`hourly_usage` holds one row per `(tenant_id, model, hour_start)` with event count,
input/output tokens, and latency sum and max. Hours are UTC.

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

## Known limits

- **Rollups are not per org yet.** `events` is keyed by org, but `hourly_usage` still groups by `(tenant_id, model, hour_start)`, so two orgs that use the same `tenant_id` and model share a rollup row. Nothing reads `hourly_usage` over the API yet. The fix is the "Rollups: org and new dimensions" task in [TASKS.md](TASKS.md).
- **Rollups ignore the newer event fields.** `events` records `provider`, `feature`, `cache_read_tokens` and `cache_write_tokens`, but `hourly_usage` neither groups by the first two nor sums the last two yet. Same task as above.
- Events older than the scheduler's two-hour window are only picked up by the startup scan or a manual backfill.
- The startup scan aggregates all of `events` to find stale hours. Fine for v1; it should be bounded by a time range as data grows.
