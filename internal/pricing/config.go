// Package pricing loads per-model rates and rates usage against them.
package pricing

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Rate is the price of one model in currency units per 1,000,000 tokens.
//
// If Tiers is set it replaces the flat rates: volume is priced graduated, so
// each token is billed at the rate of the tier it falls into.
type Rate struct {
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
	Tiers         []Tier  `yaml:"tiers,omitempty"`
}

// Tier is one band of a graduated schedule. UpToMTok is the cumulative upper
// bound in millions of tokens (per direction); omit it on the last tier for
// "everything above". Input and output volumes graduate independently.
type Tier struct {
	UpToMTok      float64 `yaml:"up_to_mtok,omitempty"`
	InputPerMTok  float64 `yaml:"input_per_mtok"`
	OutputPerMTok float64 `yaml:"output_per_mtok"`
}

// Config is the parsed and validated pricing configuration.
type Config struct {
	Currency string          `yaml:"currency"`
	Models   map[string]Rate `yaml:"models"`
	// Discounts maps tenant ID to a percentage (0-100) taken off the total.
	Discounts map[string]float64 `yaml:"discounts,omitempty"`
}

// Rate returns the rate for a model and whether it is configured.
func (c *Config) Rate(model string) (Rate, bool) {
	r, ok := c.Models[model]
	return r, ok
}

// LoadFile reads and validates a pricing config from a YAML file.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied config
	if err != nil {
		return nil, fmt.Errorf("open pricing config: %w", err)
	}
	defer func() { _ = f.Close() }()
	return Load(f)
}

// Load parses and validates a pricing config. Unknown fields are rejected so
// typos in rate names fail loudly instead of silently billing zero.
func Load(r io.Reader) (*Config, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read pricing config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse pricing config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate reports every problem in the config, joined into one error.
func (c *Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.Currency) == "" {
		problems = append(problems, "currency: required")
	}
	if len(c.Models) == 0 {
		problems = append(problems, "models: at least one model is required")
	}
	names := make([]string, 0, len(c.Models))
	for name := range c.Models {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r := c.Models[name]
		if strings.TrimSpace(name) == "" {
			problems = append(problems, "models: model name must not be empty")
		}
		if !validRate(r.InputPerMTok) {
			problems = append(problems, fmt.Sprintf("models.%s.input_per_mtok: must be a finite number >= 0", name))
		}
		if !validRate(r.OutputPerMTok) {
			problems = append(problems, fmt.Sprintf("models.%s.output_per_mtok: must be a finite number >= 0", name))
		}
		problems = append(problems, validateTiers(name, r.Tiers)...)
	}
	tenants := make([]string, 0, len(c.Discounts))
	for t := range c.Discounts {
		tenants = append(tenants, t)
	}
	sort.Strings(tenants)
	for _, t := range tenants {
		if d := c.Discounts[t]; !validRate(d) || d > 100 {
			problems = append(problems, fmt.Sprintf("discounts.%s: must be a percentage between 0 and 100", t))
		}
	}
	if len(problems) > 0 {
		return errors.New("invalid pricing config: " + strings.Join(problems, "; "))
	}
	return nil
}

func validRate(v float64) bool {
	return v >= 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func validateTiers(model string, tiers []Tier) []string {
	var problems []string
	prev := 0.0
	for i, t := range tiers {
		p := fmt.Sprintf("models.%s.tiers[%d]", model, i)
		if !validRate(t.InputPerMTok) || !validRate(t.OutputPerMTok) {
			problems = append(problems, p+": rates must be finite numbers >= 0")
		}
		last := i == len(tiers)-1
		switch {
		case last && t.UpToMTok != 0:
			problems = append(problems, p+".up_to_mtok: must be omitted on the last tier")
		case !last && (!validRate(t.UpToMTok) || t.UpToMTok <= prev):
			problems = append(problems, p+".up_to_mtok: must be finite and greater than the previous tier")
		}
		prev = t.UpToMTok
	}
	return problems
}
