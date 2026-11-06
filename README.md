# llm-usage-meter

LLM usage metering and cost attribution service in Go.

## Problem

_TODO: one paragraph on why per-tenant LLM usage and cost attribution is hard (duplicate/late events, restarts, tiered pricing) and what this service does about it._

## Quickstart

_TODO: one-command local demo, under 5 minutes._

## Architecture

_TODO: diagram (ingest -> dedupe -> hourly rollup -> pricing -> query API; OTel/Prometheus alongside)._

## Design decisions and tradeoffs

_TODO: Postgres over SQLite, `net/http` over chi, sqlc, idempotency on `event_id`, at-least-once ingestion._

## What I'd do next

_TODO_

## License

Apache License 2.0. See [LICENSE](LICENSE).
