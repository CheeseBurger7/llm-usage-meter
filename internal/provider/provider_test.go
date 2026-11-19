package provider

import (
	"strings"
	"testing"
)

func TestNormalizeFoldsASCIIOnly(t *testing.T) {
	tests := map[string]string{
		"anthropic":     "anthropic",
		"Anthropic":     "anthropic",
		"OPENAI":        "openai",
		"Azure-OpenAI":  "azure-openai",
		"É":             "É", // non-ASCII is left for Check to refuse
		"Ánthropic":     "Ánthropic",
		" Anthropic\t":  " anthropic\t", // no trimming
		"Ⱥ":             "Ⱥ",            // strings.ToLower would make this 3 bytes
		"":              "",
		"bedrock.v2_X-": "bedrock.v2_x-",
	}
	for in, want := range tests {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheck(t *testing.T) {
	tests := map[string]bool{
		"anthropic":                   true,
		"openai":                      true,
		"azure-openai":                true,
		"bedrock.us_east-1":           true,
		"unknown":                     true, // stored by migration 00005
		"_unassigned":                 true,
		"0":                           true,
		strings.Repeat("p", MaxLen):   true,
		"":                            false,
		strings.Repeat("p", MaxLen+1): false,
		"Anthropic":                   false, // Check expects a normalized value
		"ánthropic":                   false,
		"é":                           false,
		" anthropic":                  false,
		"anthropic ":                  false,
		"anth ropic":                  false,
		"anthropic\t":                 false,
		"anthropic\n":                 false,
		"open/ai":                     false,
		"open:ai":                     false,
		"anthropic\x00":               false,
		strings.Repeat("ⱥ", MaxLen/3): false,
	}
	for in, ok := range tests {
		reason := Check(in)
		if (reason == "") != ok {
			t.Errorf("Check(%q) = %q; want allowed %v", in, reason, ok)
		}
		if reason != "" && in != "" && strings.Contains(reason, in) {
			t.Errorf("Check(%q) repeats the value in its reason %q", in, reason)
		}
	}
}

func TestCheckReasons(t *testing.T) {
	if got := Check(""); got != "required" {
		t.Errorf("empty: %q", got)
	}
	if got := Check(strings.Repeat("p", MaxLen+1)); got != "must be at most 256 bytes" {
		t.Errorf("too long: %q", got)
	}
	// Length is checked first, so an overlong non-ASCII value reports length.
	if got := Check(strings.Repeat("é", MaxLen)); got != "must be at most 256 bytes" {
		t.Errorf("too long and non-ASCII: %q", got)
	}
}

// Every value Normalize returns for an input made of allowed characters and
// A-Z passes Check, so uppercase input is accepted (Q13 still holds).
func TestNormalizeThenCheckAcceptsUppercaseASCII(t *testing.T) {
	for _, in := range []string{"ANTHROPIC", "OpenAI", "Bedrock.US_EAST-1"} {
		if r := Check(Normalize(in)); r != "" {
			t.Errorf("%q: %s", in, r)
		}
	}
}
