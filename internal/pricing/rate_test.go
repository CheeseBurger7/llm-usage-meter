package pricing

import (
	"strings"
	"testing"
)

func mustLoad(t *testing.T, y string) *Config {
	t.Helper()
	c, err := Load(strings.NewReader(y))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const ratingYAML = `
currency: USD
models:
  flat: {input_per_mtok: 3, output_per_mtok: 15}
  tiered:
    tiers:
      - {up_to_mtok: 1, input_per_mtok: 10, output_per_mtok: 20}
      - {up_to_mtok: 3, input_per_mtok: 8,  output_per_mtok: 16}
      - {input_per_mtok: 5, output_per_mtok: 10}
discounts:
  acme: 10
  free: 100
`

func TestPrice(t *testing.T) {
	c := mustLoad(t, ratingYAML)
	tests := []struct {
		name          string
		tenant, model string
		u             Usage
		want          Cost
	}{
		{"zero usage", "x", "flat", Usage{}, Cost{}},
		{"flat", "x", "flat", Usage{1_000_000, 1_000_000}, Cost{18_000_000, 0, 18_000_000}},
		{"flat sub-mtok", "x", "flat", Usage{1000, 0}, Cost{3000, 0, 3000}},
		{"within first tier", "x", "tiered", Usage{500_000, 0}, Cost{5_000_000, 0, 5_000_000}},
		{"exactly first boundary", "x", "tiered", Usage{1_000_000, 0}, Cost{10_000_000, 0, 10_000_000}},
		{"spans two tiers", "x", "tiered", Usage{2_000_000, 0}, Cost{18_000_000, 0, 18_000_000}},
		{"spans all tiers", "x", "tiered", Usage{4_000_000, 0}, Cost{10_000_000 + 16_000_000 + 5_000_000, 0, 31_000_000}},
		{"directions graduate independently", "x", "tiered", Usage{1_000_000, 2_000_000}, Cost{10_000_000 + 20_000_000 + 16_000_000, 0, 46_000_000}},
		{"discount", "acme", "flat", Usage{1_000_000, 1_000_000}, Cost{18_000_000, 1_800_000, 16_200_000}},
		{"full discount", "free", "flat", Usage{1_000_000, 0}, Cost{3_000_000, 3_000_000, 0}},
		{"unlisted tenant no discount", "other", "flat", Usage{1_000_000, 0}, Cost{3_000_000, 0, 3_000_000}},
				{"huge volume no overflow", "x", "flat", Usage{1_000_000_000_000, 0}, Cost{3_000_000_000_000, 0, 3_000_000_000_000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.Price(tt.tenant, tt.model, tt.u)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("got %+v want %+v", got, tt.want)
			}
		})
	}
}

func TestPriceRounding(t *testing.T) {
	c := mustLoad(t, "currency: USD\nmodels:\n  m: {input_per_mtok: 0.5, output_per_mtok: 0}\ndiscounts: {t: 33.33}\n")
	// 3 tokens * 0.5 micro-units/token... 0.5/MTok => 500000 micro per MTok => 1.5 micro => rounds to 2.
	got, err := c.Price("t", "m", Usage{InputTokens: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got.Gross != 2 || got.Discount != 1 || got.Net != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestPriceErrors(t *testing.T) {
	c := mustLoad(t, ratingYAML)
	if _, err := c.Price("x", "nope", Usage{}); err == nil {
		t.Error("unknown model should error")
	}
	if _, err := c.Price("x", "flat", Usage{InputTokens: -1}); err == nil {
		t.Error("negative tokens should error")
	}
}

func TestValidateTiersAndDiscounts(t *testing.T) {
	tests := []struct{ name, yaml, want string }{
		{"last tier bounded", "currency: U\nmodels:\n  m:\n    tiers:\n      - {up_to_mtok: 1, input_per_mtok: 1, output_per_mtok: 1}\n", "must be omitted on the last tier"},
		{"non-increasing", "currency: U\nmodels:\n  m:\n    tiers:\n      - {up_to_mtok: 2, input_per_mtok: 1, output_per_mtok: 1}\n      - {up_to_mtok: 1, input_per_mtok: 1, output_per_mtok: 1}\n      - {input_per_mtok: 1, output_per_mtok: 1}\n", "greater than the previous"},
		{"unbounded middle", "currency: U\nmodels:\n  m:\n    tiers:\n      - {input_per_mtok: 1, output_per_mtok: 1}\n      - {input_per_mtok: 1, output_per_mtok: 1}\n", "tiers[0].up_to_mtok"},
		{"negative tier rate", "currency: U\nmodels:\n  m:\n    tiers:\n      - {input_per_mtok: -1, output_per_mtok: 1}\n", "tiers[0]: rates"},
		{"discount too high", "currency: U\nmodels:\n  m: {input_per_mtok: 1, output_per_mtok: 1}\ndiscounts: {a: 101}\n", "discounts.a"},
		{"discount negative", "currency: U\nmodels:\n  m: {input_per_mtok: 1, output_per_mtok: 1}\ndiscounts: {a: -1}\n", "discounts.a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want containing %q", err, tt.want)
			}
		})
	}
}
