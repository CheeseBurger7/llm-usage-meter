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
- **Options:** customer-run proxy and client library that send token counts and tags only (no prompts or inference keys reach the service, no added latency or outage risk, customer deploys one container) / hosted proxy (nothing to deploy, but the service sees prompts and keys and becomes a point of failure for every LLM call)
- **Default applied:** Customer-run. The proxy is shipped as a container the customer runs next to their app.
- **Answer:** Customer-run, as the default. The org reports its own numbers, so they are checked by reconciliation against the provider's usage report (see the reconciliation task), not by hosting the proxy. The guarantee is that the service never sits in the LLM call path: it never sees prompts or completions and never holds an inference key, meaning a key that can make LLM calls. A key that can only read usage totals is a different credential and is covered by Q10.

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
- **Status:** answered
- **Blocks:** Stripe usage export
- **Context:** The agent must not invent external accounts or endpoints.
- **Options:** push per-tenant usage quantities to the customer's own Stripe account / push rated amounts as invoice items
- **Default applied:** None. The task waits for an answer and test credentials.
- **Answer:** Push rated amounts as invoice items to the org's own Stripe account. Each closed statement line becomes one invoice item on the tenant's Stripe customer, and adjustment lines for late events go out with the next statement. Our price books stay the only place prices are computed: pushing quantities would make each org rebuild its price book, including our monthly tiers per tenant and model, as Stripe prices, and the two would drift until the invoice no longer matches the dashboard. The org connects its account through Stripe Connect with Standard accounts (the org authorises us through Stripe's OAuth flow; we store the connected account id, never a secret key), with Stripe handling pricing so the platform pays no Connect fees. Stripe's processing and invoicing fees fall on the org's account, not ours. The org maps each `tenant_id` to a Stripe customer id. Pushes are idempotent per statement line (Stripe idempotency key derived from the statement id and line number), so a retry never creates a second item. Build and test against Stripe test mode first. A quantities-only mode for orgs that already run Stripe meters is out of scope until a customer asks. Owner action before the task starts: create a Stripe account, enable Connect, and supply the test-mode platform key and Connect client id as environment variables. Check current Stripe pricing before quoting fees to customers.

### Q9: Product name and domain
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** Signup, the quickstart page and emails need a product name and domain. The API key prefix `lum_` is derived from the repo name and is hard to change once customers hold keys.
- **Options:** keep `llm-usage-meter` and `lum_` (no work, generic name) / pick a product name before the API key task ships (keys carry the brand from day one, needs a decision now)
- **Default applied:** Working name `llm-usage-meter`, key prefix `lum_`. Signup and email copy read the name from config so a rename touches one place.
- **Answer:** The product is **Lumecount**, on **lumecount.com**. Tagline: "Every token, lit up." "Lume" is the glow on a watch's hands, a precision instrument you can read in the dark, which fits usage numbers customers can check. The API key prefix stays `lum_` and now stands for Lumecount, so no key code changes and no key already issued in a dev database breaks. The repo and Go module keep the name `llm-usage-meter`: renaming them would change every import path for no gain, and the product name comes from config. Short `.com` names were checked against the registry before choosing; Lumetry was dropped because lumetry.com and getlumetry.com are taken and a company in another field already uses the name. Owner actions before signup ships: register lumecount.com (and lumecount.io and lumecount.dev as redirects if cheap); run a trademark search for "Lumecount" in classes 9 and 42; set up company email on the domain (`billing@`, `security@`) for the Stripe account and Google's OAuth review; publish a homepage and privacy page on the domain, which Google's OAuth consent screen needs to leave testing mode. Product copy (README title, dashboard, emails) moves to the new name in its own PR.

### Q10: How do provider totals reach the reconciliation report?
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** Reconciliation compares metered usage with what the provider says it served. The service must not hold inference keys (Q3).
- **Options:** the org uploads the provider's usage export file (no credentials stored, a manual step each month) / the org gives the service a provider key with read access to usage data and totals are pulled automatically (no manual step, the service stores a sensitive credential)
- **Default applied:** File upload.
- **Answer:** Automatic pull with a usage read key is the recommended path, and file upload stays as the fallback for orgs that won't share a key. A usage read key is a provider key that grants read access to usage and cost data per model (token counts, request counts, cost), with no access to prompts or completions and no ability to make LLM calls. Use the narrowest scope each provider offers; if a provider only exposes usage through a broader key, say so plainly on the setup page so the org can choose file upload instead. The key is encrypted at rest, used only by the reconciliation job, never logged or returned by the API, and the org can revoke it from the dashboard at any time. The privacy policy states that the key is used only to read usage totals and that prompts and completions are never accessed.

### Q11: ID type for orgs and API keys
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** The orgs and API keys tables need a primary key type, and every later table references `orgs.id`, so it is costly to change once other tables exist.
- **Options:** UUID generated by Postgres (not guessable or countable when ids appear in URLs and exports, 16 bytes per reference) / BIGINT identity (smaller and easier to read, reveals the number of customers and can be enumerated)
- **Default applied:** UUID with `gen_random_uuid()` for `orgs.id` and `api_keys.id`.
- **Answer:** Keep the default: UUID with `gen_random_uuid()` for `orgs.id` and `api_keys.id`. Ids appear in URLs, exports and Stripe metadata, so they must not reveal how many customers exist or let anyone step through them. The extra 8 bytes per reference is small next to a usage row.

### Q12: API key encoding
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** The key task says `lum_<scope>_<random>` from 32 random bytes but not how the bytes are encoded. The verifier checks the exact shape, so changing the encoding after customers hold keys means accepting both forms.
- **Options:** hex, 64 characters, key is 75 long (standard library only, no `_` or `-` inside the secret, longer) / base62, about 43 characters (shorter, needs an encoder) / base64url, 43 characters (standard library, but contains `_` and `-`, which collide with the separator and break double-click selection)
- **Default applied:** Hex. The stored prefix is the first 8 hex characters of the secret.
- **Answer:** Keep the default: lowercase hex from `encoding/hex`, 64 characters, stored prefix is the first 8 hex characters. Security is the same as base62 or base64url because all three encode the same 32 random bytes; only the length differs. Hex needs no extra code, never contains the `_` separator, and double-click selects the whole key. The extra length doesn't matter for a value pasted into an environment variable.

### Q13: Is `provider` free text or a fixed list?
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** The events task makes `provider` required but does not say what values are allowed. It becomes part of the `hourly_usage` key and of price book lookups, so `Anthropic` and `anthropic` would be two rollup rows and could miss a rate. Tightening later means rewriting stored values.
- **Options:** free text, compared byte for byte (same as `tenant_id`, `model` and `feature`; no surprise rewrites; misspellings split usage) / lowercase and trim on ingest (fewer splits; the stored value differs from what was sent) / fixed list such as `anthropic`, `openai`, `google` (no splits; every new provider needs a release, and self-hosted models have no entry)
- **Default applied:** Free text, 1 to 256 bytes, no normalization. The capture proxy will set the value itself, so most events get a consistent spelling.
- **Answer:** Free text, but case-insensitive. Ingest lowercases `provider` before validation, dedupe and storage, so `Anthropic` and `anthropic` land in one rollup row and match the same price book entry. `sameAs` compares the lowercased value, so a retry that differs only in case counts as a duplicate, not a conflict. The stored and returned value is the lowercase form. Only case is normalized: no trimming and no fixed list, so self-hosted and new providers work without a release. `tenant_id`, `model` and `feature` stay byte for byte. When price books gain a provider key, it is lowercased on load as well. No production data exists, so existing dev rows need no migration. Shipped code compares byte for byte, so this is a follow-up task in docs/TASKS.md.

### Q14: Does `input_tokens` include cache tokens?
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** Providers do not agree on whether the input token count in a response includes cached tokens. The event schema needs one meaning, or cost is wrong for one group of providers. The "Pricing: cache token rates" task prices cache tokens on top of input tokens, which only works if they do not overlap.
- **Options:** exclusive: `input_tokens` leaves out cache tokens and total input is the sum of the three (no double billing; capture must subtract for providers that report an inclusive count) / inclusive: `input_tokens` is the total and cache counts are a breakdown (pricing must subtract, and an event with cache counts above `input_tokens` becomes invalid)
- **Default applied:** Exclusive. Ingest stores the three counts as sent and does not cross-check them. The proxy tasks must map each provider's usage block to this meaning, verified against the provider's current docs.
- **Answer:** Keep the default: exclusive. `input_tokens`, `cache_read_tokens` and `cache_write_tokens` together make up the whole prompt with no overlap, and `output_tokens` is separate. The goal is that the bill covers every token the provider charged for: each of the four counts is priced at its own rate and the results are summed, so no token goes unbilled and none is billed twice. Pricing does not use the cache counts yet; the "Pricing: cache token rates" task adds them and must land before the cost endpoint ships.

### Q15: How does the batch endpoint write its events?
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** `POST /v1/events/batch` takes up to 500 events and must report `created`, `duplicate`, `conflict` or `invalid` for each. The task did not say how the events reach Postgres or what a database failure halfway through a batch returns.
- **Options:** One statement per event, each committed on its own (reuses `Store.Ingest` unchanged, same rules as the single endpoint by construction, but up to 500 round trips and commits per request) / one multi-row `INSERT ... ON CONFLICT DO NOTHING RETURNING` plus one read of the skipped ids (two round trips and one commit, but new SQL, its own handling of ids repeated inside a batch, and two concurrent batches that share ids can deadlock unless rows are sorted)
- **Default applied:** One statement per event, no surrounding transaction. A database failure returns `500` for the whole request with no partial results; events before the failure stay stored and a re-send reports them as `duplicate`. A 500-event batch against a local Postgres took about 0.2 s in the integration test. Revisit with numbers once the load generator exists.
- **Answer:** Keep the default: one statement per event, and a database failure returns `500` for the whole request. Re-sending the full batch is always safe; events already stored come back as `duplicate`. How the batch reaches Postgres is internal, so it can switch to a multi-row insert later without changing the API: the request, the per-event statuses and the retry rule stay the same. Revisit if load generator results show batch latency or database load is a problem.

### Q16: What does the ingest rate limit count, and what is the default?
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** The rate limit task says "token bucket per API key" with a configurable limit, but not whether a token is a request or an event, nor the default. A batch request holds up to 500 events, so the two units differ by up to 500 times. The client library will be written against whichever is chosen.
- **Options:** count requests (decided before the body is read, simple to explain, but a batch of 500 costs the same as one event) / count events (matches database work, but the body must be read and parsed before refusing, and a batch larger than the remaining tokens needs a rule: refuse it whole or accept part)
- **Default applied:** Requests. 100 per second per key with a burst of 100, set by `INGEST_RATE_LIMIT_RPS` and `INGEST_RATE_LIMIT_BURST`; `0` turns it off. The limit is on by default so a deployment that sets nothing is still protected. It is per process: with N replicas a key can reach N times the limit.
- **Answer:** Keep the default: the limit counts requests, 100 per second per API key with a burst of 100, on by default. Both numbers are requests, not events: the rate is the sustained refill, and the burst is how many requests an idle key can send back to back. Batch size is a separate limit of up to 500 events per request, so a key sending full batches can reach about 50,000 events per second. Counting requests lets the server refuse before reading the body and needs no rule for partial batches. The client library batches events, and Q15 measured about 0.2 s for a 500-event batch, so database load is not the bottleneck yet. Revisit with a separate events-per-second limit if load generator results show the database under strain.

### Q17: How long does the client library retry a batch?
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** The client task says "retry with exponential backoff" and "drop-oldest with a dropped counter" but not when a batch that keeps failing is given up. The proxy and the load generator inherit the choice, and it decides how many events an outage of the service costs a customer.
- **Options:** no attempt limit, the batch in flight is retried until the service answers and the queue (10000 events by default) is the only bound, dropping its oldest when full (an outage shorter than the time to fill the queue loses nothing; one batch can stay in flight for the whole outage, and `Close` needs a deadline) / give up after N attempts and count the batch as dropped (simple to explain, a stuck batch never holds up newer ones, but an outage longer than a few backoff steps discards batch after batch that the queue had room to keep)
- **Default applied:** No attempt limit. Waits grow from 200 ms to 30 s, with `Retry-After` honoured up to one minute. Only `429`, `408`, `5xx` and transport errors are retried; `400`, `401`, `403` and `413` count the batch as rejected at once, so a revoked key does not retry forever. `Close(ctx)` drops and counts what is unsent when `ctx` ends.
- **Answer:** Keep the default: no attempt limit. The batch in flight is retried with backoff from 200 ms to 30 s (`Retry-After` honoured up to one minute) until the service answers, and the 10000-event queue is the only bound, dropping its oldest events when full. An outage shorter than the time to fill the queue loses nothing. Only `429`, `408`, `5xx` and transport errors are retried; `400`, `401`, `403` and `413` reject the batch at once. `Close(ctx)` drops and counts whatever is unsent when `ctx` ends.

### Q18: What does the proxy do with a call that has no tenant header?
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** The proxy takes the tenant from `X-Meter-Tenant`, and an event without a `tenant_id` is refused by ingest. The task says the proxy fails open when metering is down, but not what happens when the caller forgot the header. Every later proxy task (streaming, OpenAI-compatible) inherits the choice.
- **Options:** forward the call and do not meter it, with a warning in the log and a skipped counter (a tagging mistake never breaks the customer's product; untagged usage is missing from billing until someone reads the log or reconciliation shows the gap) / refuse the call with `400` (no usage can go unbilled; a metering mistake now fails LLM calls, which the design rules out) / meter it under a configurable default tenant such as `unassigned` (nothing is lost and the gap is visible in the dashboard; one more setting, and cost lands on a tenant nobody can be billed for)
- **Default applied:** Forward and do not meter. The proxy logs the reason without any header value and counts the call as skipped.
- **Answer:** Meter untagged calls under the reserved tenant `_unassigned`. The leading underscore keeps it apart from real tenant ids, which are free strings. The tenant is set by `PROXY_DEFAULT_TENANT`, and an empty value goes back to forwarding without metering. The call is still forwarded unchanged, logged with its reason (no header values) and counted as `calls_untagged`, so the cause is easy to find. Nobody can be invoiced for `_unassigned`: statements, and so the Stripe export, leave it out. The dashboard shows its cost and counts its revenue as zero, so missing tags show up as lost margin, and the negative margin flag on budgets fires on it. Leaving the header off is not a way to cheat. The org runs the proxy, and only the org's backend calls it, so a tenant cannot drop the header. An org that drops it undercharges its own tenants. An org that wants to hide usage from the service can skip the proxy altogether, so nothing done with untagged calls could enforce metering anyway. The only check on complete metering is reconciliation against the provider's usage report. A larger risk is a proxy reachable by tenants, who could then send any tenant id; that is why it listens on loopback by default.

### Q19: What does the proxy record for a stream that never finishes?
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** A streamed call reports its final token counts in the last `message_delta`, just before `message_stop`. When the caller hangs up, the connection drops or the provider sends an `error` event, the proxy has seen the input counts from `message_start` but only a partial output count. The provider can still bill what it generated. The OpenAI-compatible streaming task inherits the choice.
- **Options:** skip the call, log the reason and count it as skipped (no event with a known-wrong number; the usage is missing from billing until reconciliation shows the gap) / record what was seen so far (input tokens are billed to the tenant; output is understated by an unknown amount and the event looks complete) / record what was seen with a marker that it is partial (nothing hidden; needs a new event field, a migration and a rule for how statements treat partial events)
- **Default applied:** Skip. The call is forwarded as it is, logged with its reason and counted in `calls_skipped`, the same as a non-streamed call the caller abandoned.
- **Answer:** Record what was seen, marked as partial. Events gain an optional `partial` boolean, default false, so existing clients are unaffected. When a stream ends before `message_stop`, the proxy records an event with `partial: true` and counts it as `calls_partial` instead of skipped. The event holds the input and cache counts from `message_start`, which are exact, and the latest output count seen. That output count is usually only the 1 to 3 tokens in `message_start`, because the real count arrives in the final `message_delta`. The event records the model from `message_start`; fallback attempts are not recorded, because `usage.iterations` also arrives in the final `message_delta`. A partial event can only undercount, never overcount, so statements bill it as recorded and show how many partial calls each line includes. Reconciliation against the provider's report shows the output that is missing. The marker is there so an undercount never looks like a complete call; stored events cannot be marked afterwards. Streams that carried an `error` event need a check first: before the proxy records them, verify in the provider's current docs whether input is billed when a stream ends in an error. If it is not billed, or the docs do not say, a stream that ends in an `error` event stays skipped, so tenants are never charged for usage the provider did not bill. Only caller hang-ups and dropped connections are then recorded as partial. The OpenAI-compatible streaming task follows the same rule.

### Q20: Does lowercasing `provider` cover non-ASCII letters?
- **Status:** answered
- **Blocks:** nothing (default applied)
- **Context:** Q13 says ingest lowercases `provider` but not which letters that covers. Go's `strings.ToLower` folds every Unicode letter; an ASCII-only fold leaves `É` and `é` as two providers. Whatever ingest does, the price book loader must do the same when it gains a provider key, and so must any SQL or dashboard code that compares providers, or a rate lookup can miss. For a few letters the lowercase form is longer in bytes, so the 256 byte limit is checked after lowercasing. Changing the rule later means rewriting stored values.
- **Options:** Unicode lowercase with `strings.ToLower` (the plain reading of Q13; every other component has to reproduce Go's mapping, and Postgres `lower()` depends on the database collation) / ASCII-only lowercase (the same result in every language and in SQL; non-ASCII names stay case-sensitive) / reject non-ASCII providers (nothing to disagree about; a breaking validation rule for anyone who sends one)
- **Default applied:** Unicode lowercase with `strings.ToLower`, applied only in ingest. Real provider names are ASCII, where all three options store the same value.
- **Answer:** Stricter than all three options: after an ASCII-only lowercase, `provider` must match `^[a-z0-9._-]{1,256}$`, or ingest refuses the event with `400` and a field error. Today ingest only checks the length and folds with `strings.ToLower`, so a direct API client can store non-ASCII values such as `ánthropic`. Only the proxy's constant `anthropic` is guaranteed ASCII. Uppercase ASCII is still accepted and folded, so Q13 holds and `Anthropic` stays a duplicate of `anthropic`. Because every allowed value is ASCII, the price book loader, SQL and the dashboard all compare the same bytes, with no Unicode mapping or Postgres collation involved, and the length is the same before and after lowercasing. The rule also refuses whitespace, so `" anthropic"` can no longer become a second provider. Provider names are labels such as `anthropic`, `openai` or `bedrock`, so the limit costs nothing real, and there is still no fixed list, so a new provider needs no release. Adding the rule now is cheap because there are no paying users yet. A migration adds a matching `CHECK` constraint on `events.provider` and `hourly_usage.provider`, after confirming that the stored values (`anthropic`, `unknown` from migration 00005, and test data) pass it. It is a breaking validation rule for any client that sends other characters, and the changelog says so.

### Q21: How does the proxy know whether a refusal without output is billed?
- **Status:** open
- **Blocks:** Proxy: refusals billed by category
- **Context:** The provider's Refusals and fallback page (checked 2026-11-16) says a refusal that arrives before any output is billed only when its category is `bio`, `frontier_llm` or `reasoning_extraction`, and that this list may change. A refusal after some output is always billed. For a declined fallback attempt the counts are in `usage.iterations`, but the category is only in the `fallback` block inside `content`, which the proxy does not decode today. For a call that ends in a refusal the category is in the top-level `stop_details`. The attempts that produced output are recorded already; this is about the ones with `output_tokens` of 0.
- **Options:** read the category (from `stop_details`, and from `fallback` blocks in `content`, which in a stream means decoding `content_block_start` events) and compare it with a list built into the proxy (exact while the list is current; the proxy starts decoding part of `content`, though never text, and a list change at the provider needs a proxy release) / the same, with the billed list as a proxy setting (the customer can follow a list change without a release; one more setting that is wrong when nobody updates it) / never record a refusal without output (no decoding of `content`; billed refusals in the three categories are missing until reconciliation) / always record them (nothing the provider bills is missed; tenants are charged for refusals the provider did not bill)
- **Default applied:** A declined fallback attempt without output is not recorded and the proxy logs how many it left out. A call that ends in a refusal is recorded as before this question came up. Nothing in `content` is decoded.
- **Answer:**

### Q22: Is `cache_write_tokens` part of `prompt_tokens` in a chat completion?
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** The `CompletionUsage` type in OpenAI's generated Python library (checked 2026-11-17) has `prompt_tokens_details.cache_write_tokens`, described only as "the unadjusted number of prompt tokens written to cache". `cached_tokens` is described as "cached tokens present in the prompt", so it is part of `prompt_tokens` and the proxy subtracts it (Q14). For cache writes the spec does not say. OpenAI's documentation site could not be reached from the agent's environment to check the prompt caching guide.
- **Options:** leave cache writes inside `input_tokens` and record `cache_write_tokens` as 0 (each token billed once whichever way the provider counts, at the input rate; wrong only if the provider prices writes differently from input) / treat them as part of `prompt_tokens`, subtract and record them as cache writes (right rate if they are a subset; if they are not, input is understated and the call may be skipped as inconsistent) / treat them as extra to `prompt_tokens` and record them on top (right if they are extra; double-billed if they are a subset)
- **Default applied:** Leave them inside `input_tokens`; `cache_write_tokens` is 0 on chat completion events.
- **Answer:**

### Q23: Which `provider` label does a chat completion get?
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** Many providers and self-hosted servers serve the OpenAI chat completions shape, and the response does not name the provider. The label is part of the `hourly_usage` key and will pick the price book entry, so a wrong label prices usage at the wrong rates while looking correct.
- **Options:** always `openai` (no setting; wrong for every other provider, including the Anthropic compatibility layer) / derive it from the upstream host for known provider hosts, with a setting to override, and skip chat completions when neither applies (correct where known; one more setting for gateways and self-hosted servers, and their calls are unmetered until it is set) / require the setting always (explicit; the common case needs configuration too)
- **Default applied:** Derived from the upstream host (`api.openai.com` gives `openai`, `api.anthropic.com` gives `anthropic`), overridden by `CHAT_COMPLETIONS_PROVIDER`. Any other host without the setting: chat completions are forwarded and counted as skipped, with the reason in the log.
- **Answer:**

### Q24: Do chat completion event ids need a provider namespace?
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** `docs/proxy.md` left it to the OpenAI-compatible task to settle whether ids from a second provider can collide with others, since the service dedupes on `(org_id, event_id)`. Hosted providers' ids differ in shape (`msg_...`, `chatcmpl-...`) and are unique per call. Some self-hosted OpenAI-compatible servers may issue short or repeating ids; a repeat would be stored as a `duplicate` or refused as a `conflict`, and the call would be unbilled.
- **Options:** the response `id` as it is (same rule as Messages calls; relies on the server's ids being unique) / prefix with the provider label, such as `openai/chatcmpl-...` (separates providers, but does not help a server whose own ids repeat) / an id generated by the proxy (never collides; a response seen twice is no longer deduped, and the id no longer points back to the provider's logs)
- **Default applied:** The response `id` as it is.
- **Answer:**
