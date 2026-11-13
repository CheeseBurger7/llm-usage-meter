# llm-usage-meter

LLM usage metering and cost attribution service in Go, being built into a hosted product that lets
teams selling AI features bill their own customers for LLM usage.

> **Status:** work in progress. Ingestion, dedupe, hourly rollups and the rating library (per-model
> rates, volume tiers, discounts) work today. The ingest API requires a scoped API key and stores
> events and rollups per org. The capture proxy, the query and cost API and the
> dashboard are planned (see [What I'd do next](#what-id-do-next) and
> [docs/DESIGN.md](docs/DESIGN.md)).

## Problem

Teams that share LLM access across tenants need to know who used which model, how much, and what it
cost. That is harder than summing token counts. Clients retry, so the same call arrives twice.
Events arrive late or out of order. The service restarts mid-aggregation. Prices change per model,
and real contracts add volume tiers and discounts. Naive counters drift under all of this, and a
billing number that cannot be reproduced cannot be trusted.

This service ingests LLM call events (tenant, model, tokens, latency), de-duplicates them on
`event_id` within each org, and aggregates them into hourly rollups per org, tenant, provider,
model and feature. Every rollup is recomputed
from the raw events, so it is idempotent and repairable. Rating (per-model rates, tiers, discounts)
prices those rollups. A query API for usage, cost, revenue and margin, a proxy that captures usage
from real LLM calls, and a dashboard are planned on top.

## Quickstart

You need Go 1.23+ and Docker.

```sh
# 1. Start Postgres
docker run -d --name meter-pg -p 5432:5432 \
  -e POSTGRES_PASSWORD=meter -e POSTGRES_DB=meter postgres:16

# 2. Run the server (applies migrations, backfills stale hours, then listens on :8080)
export DATABASE_URL='postgres://postgres:meter@localhost:5432/meter?sslmode=disable'
make run

# 3. In another terminal, with the same DATABASE_URL: create an org and an ingest key
ORG=$(go run ./cmd/server orgs create --name "Acme")
KEY=$(go run ./cmd/server keys create --org "$ORG" --scope ingest)

# 4. Send an event. Run it twice: the second call returns 200 "duplicate".
#    Without the Authorization header the answer is 401.
curl -i -X POST localhost:8080/v1/events -H "Authorization: Bearer $KEY" -d '{
  "event_id": "evt-1", "tenant_id": "tenant-a", "provider": "anthropic", "model": "claude-sonnet",
  "input_tokens": 100, "output_tokens": 50, "latency_ms": 420,
  "timestamp": "2026-01-02T03:04:05Z"
}'

# 5. Inspect the hourly rollup (the scheduler runs every ROLLUP_INTERVAL, default 1m)
docker exec meter-pg psql -U postgres meter -c 'SELECT * FROM hourly_usage'
```

To recompute a window by hand:

```sh
go run ./cmd/server backfill --from 2026-01-02T00:00:00Z --to 2026-01-03T00:00:00Z
```

Configuration:

| Variable | Default | Purpose |
| --- | --- | --- |
| `DATABASE_URL` | required | Postgres connection string |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `ROLLUP_INTERVAL` | `1m` | How often the scheduler re-rolls the previous and current hour |

A one-command `docker compose` demo (server, proxy, Postgres, Prometheus, Grafana) is planned.

Other commands: `make test` (integration tests use testcontainers and skip without Docker),
`make lint`, `make generate` (rebuild sqlc code).

## Architecture

```mermaid
flowchart LR
    X[Capture proxy, customer-run] -.->|planned| I
    C[Clients] -->|POST /v1/events| I[Ingest API]
    I -->|INSERT ON CONFLICT DO NOTHING, per org| E[(events)]
    E -->|recompute per hour| R[Rollup]
    S[Scheduler / startup scan / backfill CLI] --> R
    R -->|upsert| H[(hourly_usage)]
    H -.->|planned| Q[Query and cost API]
    P[Pricing / rating] -.->|planned| Q
    Q -.->|planned| D[Dashboard, alerts, statements]
```

Solid lines exist today; dotted lines are planned. The rating package exists but nothing calls it
until the cost API lands. Everything runs in one Go binary backed by Postgres, except the capture
proxy, which runs in the customer's environment so prompts and provider keys never reach the
service. The full component design, with all ten components, is in
[docs/DESIGN.md](docs/DESIGN.md).

| Component | Package | Docs |
| --- | --- | --- |
| Ingest API and dedupe | `internal/ingest`, `internal/api` | [docs/ingestion.md](docs/ingestion.md) |
| Hourly rollups and backfill | `internal/rollup` | [docs/rollups.md](docs/rollups.md) |
| Rating: rates, tiers, discounts | `internal/pricing` | [docs/pricing.md](docs/pricing.md) |
| Schema, migrations, sqlc queries | `internal/storage` | [docs/ingestion.md](docs/ingestion.md#schema-and-queries) |
| Orgs and API keys tables | `internal/storage` | [docs/orgs-and-keys.md](docs/orgs-and-keys.md) |
| API key generation, verification, admin commands | `internal/auth`, `cmd/server` | [docs/api-keys.md](docs/api-keys.md) |
| HTTP authentication middleware | `internal/auth`, `internal/api` | [docs/api-keys.md](docs/api-keys.md#http-authentication) |

## Design decisions and tradeoffs

- **Idempotency on `(org_id, event_id)`, enforced by the database.** A single
  `INSERT ... ON CONFLICT (org_id, event_id) DO NOTHING` makes concurrent retries produce exactly
  one insert, with no application-level locking. A re-sent event with an identical payload returns
  `200`. The same `event_id` with different content returns `409`, so a client bug cannot silently
  rewrite history. Delivery is at-least-once and the effect is exactly-once.
- **Rollups are recomputed, not incremented.** Each hour's row is a pure function of the raw
  events. Re-running a window is safe, late events are included on the next run, and a wrong row is
  fixed by re-running it. Incremental counters are O(1) per event but are only correct if every
  update runs exactly once, which retries and crashes break. The cost is re-scanning an hour of
  events per run. Incremental or streaming aggregation is the next step if volume outgrows that.
- **Three ways to fill rollups.** A startup scan covers downtime, a scheduler covers late events,
  and a `backfill` command covers explicit repairs. Each hour is one atomic statement, retried with
  exponential backoff, so an interrupted run leaves only whole hours behind.
- **Postgres over SQLite.** `ON CONFLICT`, `timestamptz` and real concurrency are what the dedupe
  story relies on, and it matches what a production deployment would use. SQLite would make the
  quickstart one step shorter but would not demonstrate the concurrency behavior.
- **`net/http` over chi.** Two routes and one middleware do not need a router dependency. Revisit
  when the query API adds path parameters and route groups.
- **The org comes from the API key, never from the payload.** `event_id` is unique per org, so two
  orgs can reuse an id without seeing or conflicting with each other, and a client cannot write
  into another org by naming it in the request.
- **API keys checked on every request, with no cache.** One indexed lookup per request means a
  revoked key stops working immediately. Unknown and revoked keys get the same `401`, and a failed
  check refuses the request instead of letting it through.
- **sqlc over an ORM.** Queries stay plain SQL, checked against the migrations at generate time,
  with typed Go output and no runtime reflection.
- **Latency stored as sum and max.** Rows stay mergeable across hours. Percentiles are out of scope
  for v1.
- **Timestamps normalized to UTC and truncated to microseconds**, Postgres `timestamptz` precision.
  Otherwise a re-sent event would never compare equal to its stored copy.

Non-goals: multi-region, a hosted proxy that sees prompts or provider keys, and the service making
LLM calls itself.

## What I'd do next

Six milestones, in order. The product is sellable at the end of the fourth. The full backlog is in
[docs/TASKS.md](docs/TASKS.md) and the open decisions are in
[docs/OPEN_QUESTIONS.md](docs/OPEN_QUESTIONS.md).

1. **Multi-tenant core:** orgs and scoped API keys, events and rollups keyed by org, cache token
   and feature fields, a batch ingest endpoint and per-key rate limits.
2. **Capture:** a Go client library and a proxy the customer runs next to their app. It reads
   token usage from Anthropic and OpenAI-compatible responses, streaming included, and fails open.
3. **Read side:** versioned cost and price books per org, and `GET /v1/usage` and `GET /v1/cost`
   returning cost, revenue and margin. Volume tiers apply per calendar month.
4. **Dashboard:** email magic-link login, overview, tenants, API keys, price books and signup,
   server-rendered from the same binary.
5. **Alerts and export:** budgets with signed webhook and email alerts, frozen monthly statements
   with CSV export, reconciliation against the provider's usage report, and a Stripe export.
6. **Platform ops:** Prometheus metrics and OpenTelemetry traces, Dockerfile and `docker compose`
   demo, backups with a restore drill, plan quotas, raw event retention, Grafana dashboard and a
   Helm chart.

Beyond that: bound the startup rollup scan by a time range, and move to incremental aggregation if
the per-hour re-scan becomes the bottleneck.

## License

Apache License 2.0. See [LICENSE](LICENSE).
