# Pricing configuration

Rates live in a YAML file (see [`configs/pricing.example.yaml`](../configs/pricing.example.yaml)),
loaded by `pricing.LoadFile` in `internal/pricing`.

```yaml
currency: USD
models:
  example-large:
    input_per_mtok: 3.00    # per 1,000,000 input tokens
    output_per_mtok: 15.00  # per 1,000,000 output tokens
```

Validation runs on load and reports every problem at once: currency required,
at least one model, rates finite and non-negative, and unknown keys rejected
(a typo such as `output_per_mtk` fails instead of silently pricing at zero).

## Tiers and discounts

A model may define `tiers` instead of flat rates. Pricing is **graduated**: each
token is billed at the rate of the tier it falls in (like income tax brackets),
so crossing a boundary never makes the bill go down. `up_to_mtok` is the
cumulative upper bound in millions of tokens; the last tier omits it. Input and
output volume graduate independently. Tier bounds must be strictly increasing.

`discounts` maps a tenant ID to a percentage (0-100) taken off the gross cost.

`Config.Price(tenant, model, Usage)` returns `Cost{Gross, Discount, Net}` in
`MicroUnits` (millionths of the currency). Math is exact `big.Int`, rounding
half-up once per step (gross, then discount), so results are deterministic and
cannot overflow on large volumes.

Tier volume is whatever `Usage` the caller passes; the cost endpoint (a later
task) decides the window (see OPEN_QUESTIONS Q2).

Not yet wired in: the cost endpoint.

## Tradeoff: float64 rates

Rates are `float64` in config, but `Price` converts to integer micro-units
before any arithmetic (OPEN_QUESTIONS Q1), so no float error accumulates.
