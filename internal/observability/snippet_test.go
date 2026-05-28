package observability

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSnippetTruncate_ShortStringUnchanged(t *testing.T) {
	input := "hello"
	got := SnippetTruncate(input, 100)
	assert.Equal(t, input, got, "string shorter than max must be returned unchanged")
}

func TestSnippetTruncate_ExactLengthUnchanged(t *testing.T) {
	input := "hello"
	got := SnippetTruncate(input, 5)
	assert.Equal(t, input, got, "string equal to max must be returned unchanged")
}

func TestSnippetTruncate_TruncatesWithEllipsis(t *testing.T) {
	input := "hello world"
	got := SnippetTruncate(input, 8)
	assert.True(t, strings.HasSuffix(got, "…"), "truncated string must end with ellipsis")
	runes := []rune(got)
	assert.Equal(t, 8, len(runes), "truncated rune count must equal max")
}

func TestSnippetTruncate_MultibyteSafe(t *testing.T) {
	// Each kanji is 3 bytes; we truncate at 5 runes → 4 kanji + ellipsis.
	input := "日本語テスト文字列" // 9 runes
	got := SnippetTruncate(input, 5)
	runes := []rune(got)
	assert.Equal(t, 5, len(runes), "rune count must equal max for multibyte input")
	assert.True(t, strings.HasSuffix(got, "…"), "must end with ellipsis")
}

func TestSnippetTruncate_EmojiSafe(t *testing.T) {
	// Emoji are typically 2 bytes in UTF-16 but 4 bytes in UTF-8. Ensure we
	// count by rune, not by byte.
	input := "😀😁😂😃😄😅😆😇😈😉" // 10 emoji runes
	got := SnippetTruncate(input, 6)
	runes := []rune(got)
	assert.Equal(t, 6, len(runes), "rune count must equal max for emoji input")
	assert.True(t, strings.HasSuffix(got, "…"), "must end with ellipsis")
}

func TestSnippetTruncate_MaxZeroReturnsEmpty(t *testing.T) {
	got := SnippetTruncate("hello", 0)
	assert.Equal(t, "", got, "max=0 must return empty string")
}

func TestSnippetTruncate_MaxNegativeReturnsEmpty(t *testing.T) {
	got := SnippetTruncate("hello", -1)
	assert.Equal(t, "", got, "negative max must return empty string")
}

func TestSnippetTruncate_MaxOneReturnsEllipsisOnly(t *testing.T) {
	got := SnippetTruncate("hello", 1)
	assert.Equal(t, "…", got, "max=1 must return only the ellipsis")
}

func TestSnippetTruncate_EmptyStringUnchanged(t *testing.T) {
	got := SnippetTruncate("", 10)
	assert.Equal(t, "", got, "empty input must be returned unchanged")
}

func TestSnippetTruncate_LongASCIIAt100Chars(t *testing.T) {
	// Simulate an observation snippet capped at 100 chars (AC-I.4).
	input := strings.Repeat("a", 200)
	got := SnippetTruncate(input, 100)
	runes := []rune(got)
	assert.Equal(t, 100, len(runes), "100-char cap must yield exactly 100 runes")
	assert.True(t, strings.HasSuffix(got, "…"), "must end with ellipsis")
}
