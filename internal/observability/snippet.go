// Package observability — snippet.go provides SnippetTruncate, a rune-safe
// string truncation helper used by MCP handlers to cap span attribute values
// before emitting them to the telemetry pipeline.
package observability

// ellipsis is the Unicode horizontal ellipsis character appended when a string
// is truncated. Its encoded length is 3 bytes in UTF-8.
const ellipsis = "…"

// SnippetTruncate returns s unchanged when len(s) in runes ≤ max. When s
// exceeds max runes, it returns the first (max-1) runes followed by "…" so
// the total rune count equals max.
//
// When max ≤ 0, the empty string is returned. When max == 1, only the ellipsis
// is returned (no content runes fit).
//
// The function operates on Unicode code points (runes), not bytes, so
// multibyte characters such as CJK ideographs or emoji are handled correctly.
func SnippetTruncate(s string, max int) string {
	if max <= 0 {
		return ""
	}

	runes := []rune(s)
	if len(runes) <= max {
		return s
	}

	// Reserve 1 slot for the ellipsis.
	cutAt := max - 1
	if cutAt <= 0 {
		return ellipsis
	}
	return string(runes[:cutAt]) + ellipsis
}
