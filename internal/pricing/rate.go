package pricing

import (
	"fmt"
	"math"
	"math/big"
)

// MicroUnits is currency in millionths of a unit (e.g. micro-dollars). All
// cost arithmetic is integer so summing across hours cannot drift (Q1).
type MicroUnits int64

const (
	tokensPerMTok = 1_000_000
	microsPerUnit = 1_000_000
)

// Usage is token volume for one tenant and model over some period.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// Cost is the rated result for one model's usage.
type Cost struct {
	Gross    MicroUnits // before discount
	Discount MicroUnits
	Net      MicroUnits // Gross - Discount
}

// Price prices usage for a tenant on a model. Tiers are applied graduated per
// direction, then the tenant's discount (if any) is taken off the gross.
// Rounding is half-up and happens once per step on exact big-integer math.
func (c *Config) Price(tenant, model string, u Usage) (Cost, error) {
	r, ok := c.Models[model]
	if !ok {
		return Cost{}, fmt.Errorf("pricing: no rate for model %q", model)
	}
	if u.InputTokens < 0 || u.OutputTokens < 0 {
		return Cost{}, fmt.Errorf("pricing: negative token count")
	}
	tiers := r.Tiers
	if len(tiers) == 0 {
		tiers = []Tier{{InputPerMTok: r.InputPerMTok, OutputPerMTok: r.OutputPerMTok}}
	}
	// Each term is tokens * microsPerMTok, i.e. micro-units scaled by 1e6.
	scaled := new(big.Int)
	scaled.Add(scaled, graduated(u.InputTokens, tiers, func(t Tier) float64 { return t.InputPerMTok }))
	scaled.Add(scaled, graduated(u.OutputTokens, tiers, func(t Tier) float64 { return t.OutputPerMTok }))
	gross, err := roundDiv(scaled, big.NewInt(tokensPerMTok))
	if err != nil {
		return Cost{}, err
	}
	// Discount in basis points (percent * 100), rounded to the nearest bp.
	bp := int64(math.Round(c.Discounts[tenant] * 100))
	disc := new(big.Int).Mul(big.NewInt(int64(gross)), big.NewInt(bp))
	d, err := roundDiv(disc, big.NewInt(10_000))
	if err != nil {
		return Cost{}, err
	}
	return Cost{Gross: gross, Discount: d, Net: gross - d}, nil
}

// graduated returns sum(tokens in tier * micro-rate of tier), unscaled by the
// per-million divisor.
func graduated(tokens int64, tiers []Tier, rate func(Tier) float64) *big.Int {
	total := new(big.Int)
	var lower int64
	for _, t := range tiers {
		if tokens <= lower {
			break
		}
		upper := int64(math.MaxInt64)
		if t.UpToMTok > 0 {
			upper = int64(math.Round(t.UpToMTok * tokensPerMTok))
		}
		n := min(tokens, upper) - lower
		micro := int64(math.Round(rate(t) * microsPerUnit))
		total.Add(total, new(big.Int).Mul(big.NewInt(n), big.NewInt(micro)))
		lower = upper
	}
	return total
}

func roundDiv(n, d *big.Int) (MicroUnits, error) {
	half := new(big.Int).Rsh(d, 1)
	q := new(big.Int).Div(new(big.Int).Add(n, half), d)
	if !q.IsInt64() {
		return 0, fmt.Errorf("pricing: cost overflows int64")
	}
	return MicroUnits(q.Int64()), nil
}
