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
