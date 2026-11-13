# Open questions

The agent logs decisions it cannot make alone here, picks the safe default,
and continues. The owner answers when available by filling in **Answer** and
setting Status to `answered`. The agent reads this file at the start of every run
and follows answers; if an answer contradicts shipped code, it opens a
follow-up task instead of rewriting silently.

## Template

### Q<N>: <short title>
- **Status:** open | answered
- **Blocks:** <task name, or "nothing (default applied)">
- **Context:** <why it came up>
- **Options:** <A> (tradeoff) / <B> (tradeoff)
- **Default applied:** <what the agent did>
- **Answer:** <owner fills in>

## Questions

### Q1: Money representation for rates and cost
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** The pricing loader needed a type for per-1M-token rates.
- **Options:** float64 in config, integer micro-units for arithmetic (simple config, careful math) / decimal strings parsed to big.Rat or a decimal library (exact, extra dependency and heavier config)
- **Default applied:** Rates parsed as float64; the upcoming rating code should convert to integer micro-units before summing.
- **Answer:** Keep the default: float64 rates in config, integer micro-units for all arithmetic (as `internal/pricing/rate.go` does).

### Q2: Which window do volume tiers apply over?
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** Graduated tiers need a volume window, and the repo does not say whether that is the queried range or a billing month.
- **Options:** the queried time range (simple, but the same usage costs differently depending on how it is sliced) / calendar-month per tenant+model (matches typical billing, needs the cost endpoint to load the month-to-date offset)
- **Default applied:** `Price` is a pure function of the `Usage` it is given; the caller picks the window. The cost endpoint task will use the calendar month in UTC per tenant and model (the DESIGN.md recommendation) unless answered otherwise.
- **Answer:** Tenants are billed per calendar month. Volume tiers reset at the start of each month in UTC, per tenant and model, and statements cover the same month.

### Q3: Where does usage capture run?
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** The proxy and SDK tasks need to know whether LLM traffic passes through the hosted service.
- **Options:** customer-run proxy and client library that send token counts and tags only (no prompts or provider keys reach the service, no added latency or outage risk, customer deploys one container) / hosted proxy (nothing to deploy, but the service sees prompts and keys and becomes a point of failure for every LLM call)
- **Default applied:** Customer-run. The proxy is shipped as a container the customer runs next to their app.
- **Answer:** Customer-run, as the default. The org reports its own numbers, so they are checked by reconciliation against the provider's usage report (see the reconciliation task), not by hosting the proxy.

### Q4: Dashboard stack
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** The dashboard tasks need a front-end approach.
- **Options:** server-rendered `html/template` pages embedded in the Go binary (one deploy, no build step, plainer interactions) / separate React single-page app (richer charts, second build and deploy)
- **Default applied:** Server-rendered pages embedded in the binary.
- **Answer:** Keep the default: server-rendered `html/template` pages embedded in the binary. Time-series views use a small chart library vendored as a static asset (no CDN, no Node build), fed JSON by the same query functions as the API. Buyers judge the dashboard on speed and correct numbers, not its stack, and the Milestone 4 pages are tables, forms and a few charts. Revisit only if the dashboard itself becomes the differentiator, for example an embeddable customer-facing usage portal.

### Q5: Dashboard login method
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** Dashboard users need to sign in; API keys are for machines only.
- **Options:** email magic link (no passwords to store, needs an email provider) / third-party OAuth sign-in (no email provider, depends on an external account)
- **Default applied:** Email magic link behind a sender interface, with a log-only sender until an email provider is chosen.
- **Answer:** OAuth sign-in with Google and GitHub, using each provider's standard authorization-code flow through `golang.org/x/oauth2` directly, with no hosted auth vendor. Both providers charge nothing for sign-in, and it removes the email provider from the dashboard milestone. No magic link and no passwords. A user is matched on the provider's stable account id, and only a provider-verified email is accepted. The local demo cannot depend on OAuth credentials, so a development-only login, off by default, signs in without a provider. The email sender interface moves to the "Alerts: email delivery" task. SAML and enterprise SSO are out of scope. Owner action before the hosted launch: register an OAuth app with each provider and supply the client id and secret as environment variables.

### Q6: Licence and what stays public
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** The repo is public under Apache 2.0 and is becoming a paid hosted service.
- **Options:** everything stays public, the hosted service is the product (simple, anyone can self-host) / core stays public, dashboard, statements and billing move to a private repo (protects the paid parts, two repos to maintain)
- **Default applied:** Everything stays in this repo for now.
- **Answer:** Everything stays public in this repo under Apache 2.0. The hosted service is the product: customers pay for operation, upgrades and backups, not for access to the code. Public code is what lets employers judge the engineering and lets buyers audit how their bill is computed. Secrets, customer data and the hosted deployment's environment-specific configuration never go in the repo. Revisit when there are paying customers or when someone else hosts the code commercially; the options then are to keep new paid features in a private repo or to change the licence for future versions. Code already released stays Apache 2.0. To keep that second option open, outside contributions need a contributor agreement before they are merged.

### Q7: Raw event retention
- **Status:** answered
- **Blocks:** Raw event retention
- **Context:** Rollups are recomputed from raw events, so deleting events removes the ability to repair old hours.
- **Options:** keep forever (always repairable, storage grows without bound) / delete after N days once the month's statement is closed (bounded storage, closed months are frozen by their statement)
- **Default applied:** None. The retention task waits for an answer.
- **Answer:** Delete raw events 180 days after the end of their month, and only once every tenant's statement for that month is closed. 180 days covers card dispute windows (up to about 120 days) and late questions about a bill, with two full billing cycles still repairable. Retention applies to `events` only: `hourly_usage`, `statements` and `statement_lines` are kept for the life of the org, because they are the audit record once raw events are gone. Account data (`orgs`, `users`, `api_keys`, price books, budgets) is removed when an account is deleted, not by age. Implement it by range-partitioning `events` by month and dropping whole partitions, not with `DELETE` plus `VACUUM`, which churns WAL and leaves bloat that only a locking `VACUUM FULL` returns. A partitioned table's primary key must include the partition column, so keeping dedupe on `(org_id, event_id)` across months needs its own task. Rollup recompute and backfill skip purged months. Backups are kept for 360 days, so a purged event can survive in a backup for up to 360 days after it was dropped; the retention docs must say so.

### Q8: Stripe account and export shape
- **Status:** open
- **Blocks:** Stripe usage export
- **Context:** The agent must not invent external accounts or endpoints.
- **Options:** push per-tenant usage quantities to the customer's own Stripe account / push rated amounts as invoice items
- **Default applied:** None. The task waits for an answer and test credentials.
- **Answer:**

### Q9: Product name and domain
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** Signup, the quickstart page and emails need a product name and domain. The API key prefix `lum_` is derived from the repo name and is hard to change once customers hold keys.
- **Options:** keep `llm-usage-meter` and `lum_` (no work, generic name) / pick a product name before the API key task ships (keys carry the brand from day one, needs a decision now)
- **Default applied:** Working name `llm-usage-meter`, key prefix `lum_`. Signup and email copy read the name from config so a rename touches one place.
- **Answer:**

### Q10: How do provider totals reach the reconciliation report?
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** Reconciliation compares metered usage with what the provider says it served. The service must not hold provider keys (Q3).
- **Options:** the org uploads the provider's usage export file (no credentials stored, a manual step each month) / the org gives the service a provider admin key and totals are pulled automatically (no manual step, the service stores a sensitive credential)
- **Default applied:** File upload.
- **Answer:**

### Q11: ID type for orgs and API keys
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** The orgs and API keys tables need a primary key type, and every later table references `orgs.id`, so it is costly to change once other tables exist.
- **Options:** UUID generated by Postgres (not guessable or countable when ids appear in URLs and exports, 16 bytes per reference) / BIGINT identity (smaller and easier to read, reveals the number of customers and can be enumerated)
- **Default applied:** UUID with `gen_random_uuid()` for `orgs.id` and `api_keys.id`.
- **Answer:**

### Q12: API key encoding
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** The key task says `lum_<scope>_<random>` from 32 random bytes but not how the bytes are encoded. The verifier checks the exact shape, so changing the encoding after customers hold keys means accepting both forms.
- **Options:** hex, 64 characters, key is 75 long (standard library only, no `_` or `-` inside the secret, longer) / base62, about 43 characters (shorter, needs an encoder) / base64url, 43 characters (standard library, but contains `_` and `-`, which collide with the separator and break double-click selection)
- **Default applied:** Hex. The stored prefix is the first 8 hex characters of the secret.
- **Answer:**

### Q13: Is `provider` free text or a fixed list?
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** The events task makes `provider` required but does not say what values are allowed. It becomes part of the `hourly_usage` key and of price book lookups, so `Anthropic` and `anthropic` would be two rollup rows and could miss a rate. Tightening later means rewriting stored values.
- **Options:** free text, compared byte for byte (same as `tenant_id`, `model` and `feature`; no surprise rewrites; misspellings split usage) / lowercase and trim on ingest (fewer splits; the stored value differs from what was sent) / fixed list such as `anthropic`, `openai`, `google` (no splits; every new provider needs a release, and self-hosted models have no entry)
- **Default applied:** Free text, 1 to 256 bytes, no normalization. The capture proxy will set the value itself, so most events get a consistent spelling.
- **Answer:**

### Q14: Does `input_tokens` include cache tokens?
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** Providers do not agree on whether the input token count in a response includes cached tokens. The event schema needs one meaning, or cost is wrong for one group of providers. The "Pricing: cache token rates" task prices cache tokens on top of input tokens, which only works if they do not overlap.
- **Options:** exclusive: `input_tokens` leaves out cache tokens and total input is the sum of the three (no double billing; capture must subtract for providers that report an inclusive count) / inclusive: `input_tokens` is the total and cache counts are a breakdown (pricing must subtract, and an event with cache counts above `input_tokens` becomes invalid)
- **Default applied:** Exclusive. Ingest stores the three counts as sent and does not cross-check them. The proxy tasks must map each provider's usage block to this meaning, verified against the provider's current docs.
- **Answer:**

### Q15: How does the batch endpoint write its events?
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** `POST /v1/events/batch` takes up to 500 events and must report `created`, `duplicate`, `conflict` or `invalid` for each. The task did not say how the events reach Postgres or what a database failure halfway through a batch returns.
- **Options:** One statement per event, each committed on its own (reuses `Store.Ingest` unchanged, same rules as the single endpoint by construction, but up to 500 round trips and commits per request) / one multi-row `INSERT ... ON CONFLICT DO NOTHING RETURNING` plus one read of the skipped ids (two round trips and one commit, but new SQL, its own handling of ids repeated inside a batch, and two concurrent batches that share ids can deadlock unless rows are sorted)
- **Default applied:** One statement per event, no surrounding transaction. A database failure returns `500` for the whole request with no partial results; events before the failure stay stored and a re-send reports them as `duplicate`. A 500-event batch against a local Postgres took about 0.2 s in the integration test. Revisit with numbers once the load generator exists.
- **Answer:**

### Q16: What does the ingest rate limit count, and what is the default?
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** The rate limit task says "token bucket per API key" with a configurable limit, but not whether a token is a request or an event, nor the default. A batch request holds up to 500 events, so the two units differ by up to 500 times. The client library will be written against whichever is chosen.
- **Options:** count requests (decided before the body is read, simple to explain, but a batch of 500 costs the same as one event) / count events (matches database work, but the body must be read and parsed before refusing, and a batch larger than the remaining tokens needs a rule: refuse it whole or accept part)
- **Default applied:** Requests. 100 per second per key with a burst of 100, set by `INGEST_RATE_LIMIT_RPS` and `INGEST_RATE_LIMIT_BURST`; `0` turns it off. The limit is on by default so a deployment that sets nothing is still protected. It is per process: with N replicas a key can reach N times the limit.
- **Answer:**
