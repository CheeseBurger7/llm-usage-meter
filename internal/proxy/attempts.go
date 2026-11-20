package proxy

import (
	"fmt"

	"github.com/CheeseBurger7/llm-usage-meter/client"
)

const (
	// iterationMessage is an attempt by a model that declined, when the
	// same usage block also holds a fallback_message entry.
	iterationMessage = "message"
	// iterationFallback is the attempt by the fallback model that produced
	// the returned message.
	iterationFallback = "fallback_message"
)

// iteration is one entry of usage.iterations: an attempt at the call with
// its own model and counts. Field names follow the provider's "Refusals and
// fallback" page. A count that is absent or null decodes to 0.
type iteration struct {
	Type                     string `json:"type"`
	Model                    string `json:"model"`
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
}

// declinedAttempts returns one event for each attempt that declined after
// producing output, and how many declined attempts it left out.
//
// The provider bills every attempt that produced output, at the rates of
// the model that ran it. An attempt that declined before any output is
// billed only for some refusal categories. The category is not part of the
// usage block but of a fallback block in content, and the provider does not
// document which usage.iterations entry a fallback block belongs to (the
// serving hop's earlier tool-loop iterations are message entries too), so
// those attempts are left out instead of guessed at (OPEN_QUESTIONS Q21,
// Q26).
//
// Entries are read only when a fallback_message entry is present: without
// one, usage.iterations belongs to another feature and a "message" entry
// there is not a declined attempt.
func declinedAttempts(messageID string, its []iteration) (events []client.Event, leftOut int) {
	fellBack := false
	for _, it := range its {
		fellBack = fellBack || it.Type == iterationFallback
	}
	if !fellBack {
		return nil, 0
	}
	for i, it := range its {
		if it.Type != iterationMessage {
			continue
		}
		if it.Model == "" || it.OutputTokens <= 0 {
			leftOut++
			continue
		}
		events = append(events, client.Event{
			EventID:          attemptEventID(messageID, i),
			Provider:         ProviderAnthropic,
			Model:            it.Model,
			InputTokens:      it.InputTokens,
			OutputTokens:     it.OutputTokens,
			CacheReadTokens:  it.CacheReadInputTokens,
			CacheWriteTokens: it.CacheCreationInputTokens,
		})
	}
	return events, leftOut
}

// attemptEventID is the event id of the declined attempt at the given
// position in usage.iterations. The attempts of a call share one message
// id, which stays the id of the serving attempt's event, so a declined
// attempt adds its position, counted from 1. The same response always gives
// the same ids, so a re-sent event dedupes.
func attemptEventID(messageID string, position int) string {
	return fmt.Sprintf("%s/attempt-%d", messageID, position+1)
}
