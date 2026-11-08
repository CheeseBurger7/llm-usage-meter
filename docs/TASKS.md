# Agent task backlog

Ordered. The daily agent takes the first unchecked task that is not blocked
(see [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md)). One task = one small PR.
When a task's PR is opened, the agent changes `[ ]` to `[~] (PR #N)`.
Humans change it to `[x]` on merge.

Each task needs: unit tests, an integration test if storage is involved,
and docs updated (see Definition of done in CLAUDE.md).

## Backlog

- [ ] **Pricing: config schema + loader.** `internal/pricing`: YAML config of
  per-model input/output rates per 1M tokens; validate on load; example file
  in `deploy/` or `configs/`.
- [ ] **Pricing: tiered rates + discount rules.** Volume tiers and
  per-tenant discount percentages, applied to rolled-up usage. Table-driven tests.
- [ ] **Query API: usage endpoint.** `GET /v1/tenants/{id}/usage?from&to&model`
  reading `hourly_usage`. Integration test against Postgres.
- [ ] **Query API: cost endpoint.** Same filters, applies pricing rules,
  returns per-model cost breakdown.
- [ ] **Ingestion retries/backoff + graceful shutdown audit.** Verify
  at-least-once behaviour and clean shutdown; add tests for gaps found.
- [ ] **Telemetry: Prometheus `/metrics` + OTel setup.** `internal/telemetry`;
  counters/histograms for ingest, rollup and API.
- [ ] **Telemetry: OTel traces** across ingest, rollup and query paths.
- [ ] **Grafana dashboard JSON** in `dashboards/` driven by the metrics above.
- [ ] **Dockerfile + docker compose demo** in `deploy/compose/`
  (server, Postgres, Prometheus, Grafana). One command up.
- [ ] **Load generator** `cmd/loadgen`: synthetic events with configurable
  tenants/models/rate, includes duplicates to exercise dedupe.
- [ ] **LLM proxy/SDK wrapper** that records usage from real LLM API calls.
- [ ] **Helm chart** in `deploy/helm/`.
- [ ] **README pass:** problem statement, <5 min quickstart, architecture
  diagram, design decisions and tradeoffs, what I'd do next.
