# Project: LLM Usage Metering & Cost Attribution Service

## Purpose
A hosted service, developed in a public repo, that lets teams selling AI features bill their own customers for LLM usage with numbers they can audit. A Go service ingests LLM call events (org, tenant, model, tokens, latency), aggregates them hourly, prices them against per-org cost and price books, and exposes usage, cost, revenue and margin via an API and a dashboard. The target buyer is a seed to Series A team with usage-based pricing and no in-house metering. The component design is in `docs/DESIGN.md` (components C1 to C10).

## Owner context
- Meet, senior-level platform/software engineer (Go, Kubernetes, Helm, Terraform, AWS, OpenTelemetry, Prometheus, Python, SQL).
- Goal: turn this into a paid hosted product while keeping the engineering visible on GitHub.
- Prefers: small PRs, clear commit history, working quickstarts, written design tradeoffs.

## Scope (portfolio v1, built; the hosted-product scope is in docs/DESIGN.md and docs/TASKS.md)
1. **Ingestion API**: accept usage events (event_id, tenant_id, model, input_tokens, output_tokens, latency_ms, timestamp). Idempotent on event_id.
2. **Aggregation**: hourly rollups per tenant/model. Backfill job for missed windows (idempotent, safe to re-run).
3. **Pricing/rating**: configurable per-model rates, tiered pricing and discount rules, in config files.
4. **Query API**: per-tenant usage and cost by time range and model.
5. **Reliability**: retries with exponential backoff, at-least-once ingestion with dedupe, graceful shutdown.
6. **Observability**: OpenTelemetry traces and metrics, Prometheus `/metrics`, Grafana dashboard JSON committed in repo.
7. **Deployability**: Dockerfile, Helm chart, one-command local demo (docker compose or kind).
8. **AI angle**: small proxy/SDK wrapper that records usage from real LLM API calls, plus a load generator for the demo.

Now in scope for the hosted product: multi-tenant orgs and API keys, dashboard login, a server-rendered dashboard, budgets and alerts, monthly statements and a Stripe usage export. Still non-goals: multi-region, a hosted proxy that sees prompts or provider keys, the service making LLM calls itself.

## Suggested stack (confirm before locking in)
- Go 1.23+, standard `net/http` or chi, `sqlc` + Postgres (SQLite acceptable for quick start), `golangci-lint`, GitHub Actions CI.
- Postgres for storage; object storage not required in v1.

## Repo layout (target)
```
cmd/server/        # main service
cmd/loadgen/       # load generator
cmd/proxy/         # customer-run capture proxy
client/            # Go client library (batching, retries)
internal/auth/     # orgs, API keys, dashboard login
internal/alerts/   # budgets and alert delivery
internal/export/   # statements, CSV, Stripe
web/               # server-rendered dashboard
internal/ingest/   # event intake + dedupe
internal/rollup/   # hourly aggregation + backfill
internal/pricing/  # rating rules
internal/api/      # query API
internal/telemetry/# OTel + Prometheus setup
deploy/helm/       # Helm chart
deploy/compose/    # local demo
dashboards/        # Grafana JSON
docs/              # architecture, design decisions
```

## Plan
Six milestones in `docs/TASKS.md`: multi-tenant core, capture, read side, dashboard (sellable here), alerts and export, platform ops. Open decisions and their defaults are in `docs/OPEN_QUESTIONS.md`.

## Definition of done per feature
- Unit tests, plus an integration test where storage is involved.
- Docs updated (README or docs/).
- Small PR with a descriptive title and rationale.

## README requirements (must-haves)
One-paragraph problem statement, quickstart that works in under 5 minutes, architecture diagram, "design decisions and tradeoffs", "what I'd do next".

## Working agreements for Claude
- Plan before large changes; propose the approach and wait for a go-ahead.
- Keep commits small and meaningful. Don't commit generated junk.
- Flag anything that looks derived from employer-specific material.
- Explain tradeoffs briefly when choosing a library or pattern.
- Do not add "Co-Authored-By" or "Generated with Claude Code" lines to commit messages or PR bodies.
- `gh` is at `C:\Program Files\GitHub CLI\gh.exe`.

## First session TODO
1. Confirm stack choices and repo name.
2. `go mod init`, create layout, add Makefile (`build`, `test`, `lint`, `run`).
3. Add GitHub Actions CI workflow.
4. Write README skeleton with the must-have sections as headings.
5. Implement `POST /v1/events` with idempotent insert + tests.
6. Add a LICENSE.
