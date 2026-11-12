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
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** The dashboard tasks need a front-end approach.
- **Options:** server-rendered `html/template` pages embedded in the Go binary (one deploy, no build step, plainer interactions) / separate React single-page app (richer charts, second build and deploy)
- **Default applied:** Server-rendered pages embedded in the binary.
- **Answer:**

### Q5: Dashboard login method
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** Dashboard users need to sign in; API keys are for machines only.
- **Options:** email magic link (no passwords to store, needs an email provider) / third-party OAuth sign-in (no email provider, depends on an external account)
- **Default applied:** Email magic link behind a sender interface, with a log-only sender until an email provider is chosen.
- **Answer:**

### Q6: Licence and what stays public
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** The repo is public under Apache 2.0 and is becoming a paid hosted service.
- **Options:** everything stays public, the hosted service is the product (simple, anyone can self-host) / core stays public, dashboard, statements and billing move to a private repo (protects the paid parts, two repos to maintain)
- **Default applied:** Everything stays in this repo for now.
- **Answer:**

### Q7: Raw event retention
- **Status:** open
- **Blocks:** Raw event retention
- **Context:** Rollups are recomputed from raw events, so deleting events removes the ability to repair old hours.
- **Options:** keep forever (always repairable, storage grows without bound) / delete after N days once the month's statement is closed (bounded storage, closed months are frozen by their statement)
- **Default applied:** None. The retention task waits for an answer.
- **Answer:**

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
