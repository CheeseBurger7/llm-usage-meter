package proxy

import (
	"fmt"
	"regexp"
)

// stopReasonRefusal is the stop_reason of a call a safety classifier
// declined. Field names and values follow the provider's "Refusals and
// fallback" page.
const stopReasonRefusal = "refusal"

// DefaultBilledRefusalCategories are the refusal categories the provider
// bills when the refusal arrives before any output, per its "Refusals and
// fallback" page (checked 2026-11-20). The provider states the list may
// change; Config.BilledRefusalCategories replaces it (OPEN_QUESTIONS Q21).
var DefaultBilledRefusalCategories = []string{"bio", "frontier_llm", "reasoning_extraction"}

// knownRefusalCategories are all the categories the same page lists. A
// category that is neither here nor on the billed list is unknown.
var knownRefusalCategories = map[string]bool{
	"cyber":                true,
	"bio":                  true,
	"frontier_llm":         true,
	"reasoning_extraction": true,
	"general_harms":        true,
}

// refusalCategoryPattern is what a configured category must look like. The
// provider's categories are lowercase words joined by underscores.
var refusalCategoryPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// ValidRefusalCategory reports whether c can be an entry of
// Config.BilledRefusalCategories.
func ValidRefusalCategory(c string) bool {
	return refusalCategoryPattern.MatchString(c)
}

// stopDetails is the part of a refusal's stop_details the proxy reads. A
// null category, which the provider sends for a refusal that maps to no
// named category, decodes to nil.
type stopDetails struct {
	Type     string  `json:"type"`
	Category *string `json:"category"`
}

// refusal describes a call whose returned attempt was declined before any
// output. Such a call is billed only for some categories.
type refusal struct {
	// category is empty when stop_details or its category was missing or
	// null.
	category string
}

// refusalWithoutOutput returns the refusal of an attempt that stopped with
// the given reason and details after producing outputTokens, or nil when
// the attempt is billed like any other call: it was not a refusal, or it
// declined after some output.
func refusalWithoutOutput(stopReason string, details *stopDetails, outputTokens int64) *refusal {
	isRefusal := stopReason == stopReasonRefusal || (details != nil && details.Type == stopReasonRefusal)
	if !isRefusal || outputTokens > 0 {
		return nil
	}
	r := &refusal{}
	if details != nil && details.Category != nil {
		r.category = *details.Category
	}
	return r
}

// refusalVerdict is what the proxy does with a refusal without output.
type refusalVerdict int

const (
	refusalBilled   refusalVerdict = iota // record it
	refusalUnbilled                       // known category the provider does not bill: skip
	refusalUnknown                        // missing or unrecognised category: skip and log
)

// refusalPolicy decides from the category whether a refusal without output
// is billed.
type refusalPolicy struct {
	billed map[string]bool
}

// newRefusalPolicy builds the policy for a billed list. Nil means
// DefaultBilledRefusalCategories; an empty, non-nil list bills no refusal
// without output.
func newRefusalPolicy(billed []string) (refusalPolicy, error) {
	if billed == nil {
		billed = DefaultBilledRefusalCategories
	}
	p := refusalPolicy{billed: make(map[string]bool, len(billed))}
	for i, c := range billed {
		if !ValidRefusalCategory(c) {
			// The position only: configuration values are never echoed.
			return refusalPolicy{}, fmt.Errorf("billed refusal category %d must match %s", i+1, refusalCategoryPattern)
		}
		p.billed[c] = true
	}
	return p, nil
}

func (p refusalPolicy) verdict(r refusal) refusalVerdict {
	switch {
	case p.billed[r.category]:
		return refusalBilled
	case knownRefusalCategories[r.category]:
		return refusalUnbilled
	default:
		return refusalUnknown
	}
}
