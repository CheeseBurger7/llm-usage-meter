package pricing

import (
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr []string
	}{
		{"valid", "currency: USD\nmodels:\n  m:\n    input_per_mtok: 1\n    output_per_mtok: 2\n", nil},
		{"zero rate ok", "currency: USD\nmodels:\n  m: {input_per_mtok: 0, output_per_mtok: 0}\n", nil},
		{"missing currency", "models:\n  m: {input_per_mtok: 1, output_per_mtok: 1}\n", []string{"currency: required"}},
		{"no models", "currency: USD\n", []string{"at least one model"}},
		{"negative rate", "currency: USD\nmodels:\n  m: {input_per_mtok: -1, output_per_mtok: 1}\n", []string{"models.m.input_per_mtok"}},
		{"nan rate", "currency: USD\nmodels:\n  m: {input_per_mtok: .nan, output_per_mtok: 1}\n", []string{"models.m.input_per_mtok"}},
		{"unknown field", "currency: USD\nmodels:\n  m: {input_per_mtok: 1, output_per_mtk: 1}\n", []string{"output_per_mtk"}},
		{"multiple problems", "models:\n  a: {input_per_mtok: -1, output_per_mtok: -1}\n", []string{"currency", "models.a.input_per_mtok", "models.a.output_per_mtok"}},
		{"malformed yaml", "currency: [", []string{"parse pricing config"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(strings.NewReader(tt.yaml))
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if c == nil {
					t.Fatal("nil config")
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			for _, w := range tt.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q missing %q", err, w)
				}
			}
		})
	}
}

func TestLoadFileExample(t *testing.T) {
	c, err := LoadFile("../../configs/pricing.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, ok := c.Rate("example-large")
	if !ok || r.InputPerMTok != 3 || r.OutputPerMTok != 15 {
		t.Errorf("unexpected rate %+v ok=%v", r, ok)
	}
	if _, ok := c.Rate("nope"); ok {
		t.Error("unknown model should not be found")
	}
}

func TestLoadFileMissing(t *testing.T) {
	if _, err := LoadFile("does-not-exist.yaml"); err == nil {
		t.Fatal("expected error")
	}
}
