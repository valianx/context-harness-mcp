package embed

import "strings"

// TruncateToTokens returns a truncated version of text so that the approximate
// token count does not exceed max. fastembed-go's internal tokenizer is not
// exported, so we use a whitespace-word heuristic: one word ≈ one token for
// most English KG content. This is conservative but sufficient for v1 — the
// model's own max-length (512) is a hard cap applied by fastembed-go itself.
func TruncateToTokens(text string, max int) string {
	words := strings.Fields(text)
	if len(words) <= max {
		return text
	}
	return strings.Join(words[:max], " ")
}
