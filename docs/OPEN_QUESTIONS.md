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
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** The pricing loader needed a type for per-1M-token rates.
- **Options:** float64 in config, integer micro-units for arithmetic (simple config, careful math) / decimal strings parsed to big.Rat or a decimal library (exact, extra dependency and heavier config)
- **Default applied:** Rates parsed as float64; the upcoming rating code should convert to integer micro-units before summing.
- **Answer:**

### Q2: Which window do volume tiers apply over?
- **Status:** open
- **Blocks:** nothing (default applied)
- **Context:** Graduated tiers need a volume window, and the repo does not say whether that is the queried range or a billing month.
- **Options:** the queried time range (simple, but the same usage costs differently depending on how it is sliced) / calendar-month per tenant+model (matches typical billing, needs the cost endpoint to load the month-to-date offset)
- **Default applied:** `Price` is a pure function of the `Usage` it is given; the caller picks the window. The cost endpoint task will use the queried range unless answered otherwise.
- **Answer:**
