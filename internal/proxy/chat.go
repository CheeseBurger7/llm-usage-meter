package proxy

import (
	"encoding/json"

	"github.com/CheeseBurger7/llm-usage-meter/client"
)

// chatCompletion is the part of an OpenAI-compatible chat completion the
// proxy reads. Field names follow the ChatCompletion and CompletionUsage
// types generated from OpenAI's API spec (see docs/proxy.md); everything
// else, including the choices, is ignored by the decoder.
type chatCompletion struct {
	ID     string `json:"id"`
	Object string `json:"object"`
	Model  string `json:"model"`
	Usage  *struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
		// Optional, and null or absent on many OpenAI-compatible servers;
		// a missing count decodes to 0.
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// chatEvent maps a non-streamed chat completion to its usage event, or
// returns why it cannot be metered.
//
// prompt_tokens counts the whole prompt, and prompt_tokens_details.
// cached_tokens is the part of it read from the cache. The event schema
// keeps the two apart (OPEN_QUESTIONS Q14), so the cached tokens are
// subtracted from the input count. completion_tokens already includes
// reasoning tokens, which are billed as output, so it is copied as it is.
//
// prompt_tokens_details.cache_write_tokens is not read: the spec does not
// say whether it is part of prompt_tokens, and leaving it inside the input
// count can neither bill a token twice nor drop one (OPEN_QUESTIONS Q22).
func chatEvent(body []byte, provider string) (client.Event, string) {
	var c chatCompletion
	if err := json.Unmarshal(body, &c); err != nil {
		return client.Event{}, "response is not a chat completion"
	}
	u := c.Usage
	if c.Object != "chat.completion" || c.ID == "" || c.Model == "" ||
		u == nil || u.PromptTokens == nil || u.CompletionTokens == nil {
		return client.Event{}, "response has no completion id, model or usage block"
	}
	var cached int64
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	if *u.PromptTokens < 0 || *u.CompletionTokens < 0 || cached < 0 || cached > *u.PromptTokens {
		return client.Event{}, "response has inconsistent token counts"
	}
	return client.Event{
		// The provider's id is stable for the call, so a re-sent event dedupes.
		EventID:         c.ID,
		Provider:        provider,
		Model:           c.Model,
		InputTokens:     *u.PromptTokens - cached,
		OutputTokens:    *u.CompletionTokens,
		CacheReadTokens: cached,
	}, ""
}
