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

Not yet wired in: tiered rates and discounts (next task), and the cost endpoint.

## Tradeoff: float64 rates

Rates are `float64`. That is fine for a rate table, but cost *arithmetic*
(summing across hours, tiers, discounts) should not accumulate float error.
See OPEN_QUESTIONS Q1; the rating step will compute in integer micro-units.
