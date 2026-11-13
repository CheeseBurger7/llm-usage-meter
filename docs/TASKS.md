# Agent task backlog

Ordered. The daily agent takes the first unchecked task that is not blocked
(see [OPEN_QUESTIONS.md](OPEN_QUESTIONS.md)). One task = one small PR.
When a task's PR is opened, the agent changes `[ ]` to `[~] (PR #N)`.
Humans change it to `[x]` on merge.

Each task needs: unit tests, an integration test if storage is involved,
and docs updated (see Definition of done in CLAUDE.md).

Component numbers (C1 to C10) refer to [DESIGN.md](DESIGN.md).

## Owner actions (not for the agent)

- Answer Q7 to Q10 in OPEN_QUESTIONS.md, or accept the defaults.
- Before the hosted launch: register an OAuth app with Google and with
  GitHub and supply each client id and secret as environment variables (Q5).

## Done

- [x] (PR #8) **Pricing: config schema + loader.**
- [x] (PR #9) **Pricing: tiered rates + discount rules.**
- [x] (PR #7) **README pass.**

## Backlog

### Milestone 1: multi-tenant core (C2, C3, C4)

- [~] (PR #11) **Storage: orgs and API keys tables.** Migration and sqlc queries for
  `orgs(id, name, created_at)` and `api_keys(id, org_id, key_hash, prefix,
  scope, created_at, revoked_at)`. `scope` is `ingest` or `read`. Unique index
  on `key_hash`. Integration test against Postgres.
- [~] (PR #12) **Auth: API key generation and verification.** New `internal/auth`.
  Generate keys as `lum_<scope>_<random>` from 32 random bytes. Store only the
  SHA-256 hash and an 8-character prefix. `Verify(ctx, key)` returns org ID and
  scope, and rejects revoked keys. Add `server keys create --org --scope`,
  `server keys revoke --prefix` and `server orgs create --name` subcommands.
  The plaintext key is printed once.
- [~] (PR #13) **Auth: HTTP middleware.** `Authorization: Bearer <key>`. 401 for a
  missing or unknown key, 403 for the wrong scope. Puts the org ID in the
  request context. `/healthz` stays open. `POST /v1/events` requires `ingest`
  scope. Never log the key.
- [~] (PR #14) **Events: scope to org.** Migration: add `org_id` to `events`, primary
  key becomes `(org_id, event_id)`, indexes lead with `org_id`. The ingest
  store takes the org ID from the request context, never from the payload. The
  same `event_id` in two orgs must not conflict (integration test). No
  production data exists, so one breaking migration is fine. Update
  docs/ingestion.md.
- [~] (PR #15) **Events: richer usage fields.** Add `provider` (required),
  `cache_read_tokens` and `cache_write_tokens` (default 0, must be >= 0), and
  `feature` (optional, at most 256 bytes). Include all of them in `sameAs` so
  the 409 rule still holds. Validation tests.
- [~] (PR #16) **Rollups: org and new dimensions.** `hourly_usage` key becomes
  `(org_id, tenant_id, provider, model, feature, hour_start)` with sums for
  the cache token columns. Update `RollupWindow` and `ListStaleHours`.
  Integration tests, update docs/rollups.md.
- [~] (PR #17) **Ingest: batch endpoint.** `POST /v1/events/batch`, up to 500 events,
  per-event result (`created`, `duplicate`, `conflict`, `invalid`). One bad
  event does not fail the batch. Request body size limit.
- [~] (PR #21) **Ingest: per-key rate limit.** In-memory token bucket per API key.
  429 with `Retry-After`. Limit is configurable by environment variable.
- [ ] **Milestone 1 acceptance: two-org isolation test.** End-to-end
  integration test over HTTP against Postgres: two orgs each ingest with
  their own key (single and batch), rollups run, and neither org's rows
  appear under the other in `events` or `hourly_usage`. Also covers a
  revoked key (401) and a `read` key on the ingest route (403). This is the
  milestone's "done when" in DESIGN.md.

### Milestone 2: capture (C1)

- [ ] **Go client package.** New `client/`: `New(endpoint, key)`,
  non-blocking `Record(event)`, bounded queue, batching to the batch
  endpoint, retry with exponential backoff, drop-oldest with a dropped
  counter, `Close` flushes. It must never block or fail the caller's LLM
  call. Tests with `httptest`.
- [ ] **Proxy: Anthropic Messages pass-through.** New `cmd/proxy`: reverse
  proxy that forwards requests and responses unchanged, reads the `usage`
  block from non-streaming responses and records an event through the client
  package. Tenant and feature come from `X-Meter-Tenant` and
  `X-Meter-Feature` headers, which are stripped before forwarding. `event_id`
  is the provider's response ID. Never log or store request or response
  bodies or provider keys. Fail open if metering is down. Verify usage field
  names against the current provider docs. Tests with a fake upstream.
- [ ] **Proxy: streaming responses.** Read usage from server-sent events
  while streaming through without buffering the whole response. Test that
  time to first byte is not delayed.
- [ ] **Proxy: OpenAI-compatible chat completions.** Same behaviour for the
  chat completions response shape, streaming and non-streaming. Verify field
  names against the current provider docs.
- [ ] **Load generator** `cmd/loadgen`: synthetic events with configurable
  orgs/tenants/models/rate through the client package, includes duplicates
  to exercise dedupe.

### Milestone 3: read side (C5, C6)

- [ ] **Price book: storage and versions.** Table `price_book_versions(id,
  org_id, kind, effective_from, body, created_at)`. `kind` is `cost` (what the
  org pays the provider) or `price` (what the org charges its tenants). `body`
  uses the existing `pricing.Config` schema and is validated by the existing
  loader on write. Lookup returns the version active at a given time. Add
  `server pricebook import --org --kind --file --effective-from`.
- [ ] **Pricing: cache token rates.** Optional `cache_read_per_mtok` and
  `cache_write_per_mtok` on `Rate` and `Tier`. `Usage` gains the two cache
  fields. Missing rates price cache tokens at the input rate. Table-driven
  tests, update docs/pricing.md.
- [ ] **Query API: usage endpoint.** `GET /v1/usage?from&to&group_by&
  tenant_id&model&feature` reading `hourly_usage`. `group_by` is one or more
  of `tenant`, `model`, `feature`, `day`. `read` scope, org from the key.
  `from` and `to` must align to the hour. Adopt chi if routing needs it.
  Integration test against Postgres.
- [ ] **Query API: cost endpoint.** `GET /v1/cost` with the same filters.
  Returns cost (cost book), revenue (price book) and margin per group in
  integer micro-units plus the currency. Volume tiers apply per calendar
  month in UTC per tenant and model (Q2), so load the month-to-date offset
  before rating the queried range. Each hour is rated with the price book
  version active at that hour.
- [ ] **Ingestion retries/backoff + graceful shutdown audit.** Verify
  at-least-once behaviour and clean shutdown; add tests for gaps found.

### Milestone 4: dashboard (C3, C8)

- [ ] **Dashboard login.** `users(id, org_id, email, created_at)` with a
  unique index on `email`, and `user_identities(user_id, provider, subject)`
  with a unique key on `(provider, subject)`. OAuth sign-in with Google and
  GitHub (Q5): authorization-code flow with `golang.org/x/oauth2`, a `state`
  value checked on the callback, PKCE where the provider supports it. Request
  only the scopes needed to read the account id and email. Match users on the
  provider's account id, and refuse an email the provider has not verified.
  Client id and secret come from environment variables; a provider without
  them is not offered. Session cookie is HttpOnly, Secure and SameSite=Lax,
  and sessions are stored server-side so they can be revoked. A
  development-only login, enabled by an environment variable and off by
  default, signs in without a provider for the local demo. Verify endpoint
  and field names against the current provider docs. Tests with a fake
  provider.
- [ ] **Dashboard shell and overview page.** New `web/`: `html/template`
  pages and static assets embedded in the server binary (Q4). Charts use a
  small chart library vendored under `web/` (no CDN, no Node build); state
  the library and its licence in the PR. Overview shows
  month-to-date cost, revenue and margin, and the top 10 tenants by cost.
  Pages call the same query functions as the API.
- [ ] **Dashboard: tenants table and tenant detail.** Sortable table of
  tenants with usage, cost, revenue and margin. Detail page breaks one tenant
  down by model and feature per day.
- [ ] **Dashboard: API keys page.** Create a key (shown once), list by
  prefix, revoke.
- [ ] **Dashboard: price book page.** List versions per kind, upload a YAML
  file as a new version with an effective date, show validation errors.
- [ ] **Signup flow.** Create an org, its first user and its first ingest
  and read keys. Ends on a quickstart page with a copyable proxy command.

### Milestone 5: alerts and export (C7, C9)

- [ ] **Budgets: storage and evaluation.** `budgets(id, org_id, tenant_id
  nullable, monthly_amount_micros, thresholds)` and `alert_events(budget_id,
  period, threshold, fired_at)` with a unique key so each threshold fires
  once per period. A budget is compared with month-to-date cost (cost book,
  what the org pays the provider), not revenue. Evaluate after every rollup
  run. Also flag any tenant with
  negative month-to-date margin.
- [ ] **Dashboard: budgets page.** List budgets per org and tenant, create
  and delete a budget with its thresholds, show alerts fired this period.
- [ ] **Alerts: webhook delivery.** POST a JSON payload signed with
  HMAC-SHA256 using a per-org secret. Retry with backoff. Record delivery
  status.
- [ ] **Alerts: email delivery.** Email sending sits behind an interface
  with a log-only sender for development.
- [ ] **Statements: monthly snapshot.** `statements` and `statement_lines`
  hold one frozen statement per tenant per month, one line per model with
  usage, rate, discount and net. Late events after a statement closes become
  an adjustment line on the next statement, never a rewrite. `server
  statements close --org --month`.
- [ ] **Statements: CSV export.** `GET /v1/statements/{id}.csv` and a
  dashboard download link.
- [ ] **Reconciliation report.** The org uploads its provider's usage
  export for a month (Q10). Compare metered tokens per provider and model
  with the provider's totals and show the difference, absolute and percent,
  on a dashboard page and at `GET /v1/reconciliation?month`. Flag a
  difference above a configurable threshold. Start with one provider.
  Verify the export format against the current provider docs; never invent
  it. No provider key is stored.
- [ ] **Stripe usage export.** Push closed statement totals to Stripe.
  Blocked by Q8.

### Milestone 6: platform ops (C10)

- [ ] **Telemetry: Prometheus `/metrics` + OTel setup.** `internal/telemetry`;
  counters/histograms for ingest, rollup and API.
- [ ] **Telemetry: OTel traces** across ingest, rollup and query paths.
- [ ] **Dockerfile + docker compose demo** in `deploy/compose/`
  (server, proxy, Postgres, Prometheus, Grafana). One command up.
- [ ] **Backups.** Scheduled `pg_dump` to S3-compatible object storage with
  a documented restore drill in docs/. Backups are kept for 360 days (Q7).
- [ ] **Plan limits.** Monthly event quota per org. Over quota returns 429
  with a clear error body and never drops already-accepted events.
- [ ] **Partition `events` by month.** Range-partition `events` on the event
  timestamp, one partition per UTC month, created ahead of time by the
  scheduler. Postgres requires the partition column in the primary key, so
  keep dedupe on `(org_id, event_id)` across months with a small
  `event_ids` table (or an equivalent the design note justifies). Cheapest
  before the hosted launch holds customer data (Q7).
- [ ] **Raw event retention.** Job that detaches and drops an `events`
  partition once its month ended more than 180 days ago and every tenant's
  statement for that month is closed (Q7). No `DELETE` plus `VACUUM`.
  `hourly_usage`, statements and statement lines are kept. Rollup recompute
  and backfill must skip months whose raw events were purged, or they would
  overwrite kept rollups with zeros (integration test).
- [ ] **Grafana dashboard JSON** in `dashboards/` driven by the metrics above.
- [ ] **Helm chart** in `deploy/helm/`.
