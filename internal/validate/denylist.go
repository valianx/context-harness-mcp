package validate

import (
	"fmt"
	"regexp"
	"strings"
)

// junkRule pairs a human-readable name with a compiled regexp.
// The name is returned as MatchedPattern in the Error so Claude can surface a
// descriptive rejection reason without exposing the raw matched text.
type junkRule struct {
	name    string
	pattern *regexp.Regexp
}

// junkRules is the curated in-code denylist. Expanding this table requires a
// PR change backed by a unit test — it must never be driven by an env var,
// because an env-var override could silently relax the filter on a
// misconfigured deploy.
//
// Pattern families:
//  1. Filesystem artefacts — paths that indicate generated or dependency
//     directories were pasted into an observation.
//  2. Binary/log markers — content that originated from a binary log,
//     hex dump, or encoded payload rather than prose documentation.
//  3. Code-dump heuristics — ≥10 consecutive lines that look like source code
//     imports or declarations indicate a file was pasted wholesale.
var junkRules = []junkRule{
	// ── 1. Filesystem artefacts ───────────────────────────────────────────────
	{
		name:    "filesystem/node_modules",
		pattern: regexp.MustCompile(`(?i)node_modules`),
	},
	{
		name:    "filesystem/__pycache__",
		pattern: regexp.MustCompile(`__pycache__`),
	},
	{
		name:    "filesystem/.next/",
		pattern: regexp.MustCompile(`\.next/`),
	},
	{
		name:    "filesystem/dist/",
		pattern: regexp.MustCompile(`(?:^|/)dist/`),
	},
	{
		name:    "filesystem/build/",
		pattern: regexp.MustCompile(`(?:^|/)build/`),
	},
	{
		name:    "filesystem/target/debug/",
		pattern: regexp.MustCompile(`target/debug/`),
	},
	{
		name:    "filesystem/target/release/",
		pattern: regexp.MustCompile(`target/release/`),
	},

	// ── 2. Binary / log markers ───────────────────────────────────────────────
	{
		name:    "binary-log/SYSTEM_LOG",
		pattern: regexp.MustCompile(`SYSTEM_LOG`),
	},
	// Hex dump: line starting with 8 lowercase hex digits followed by two spaces
	// (the canonical format produced by `xxd`, `hexdump -C`, etc.).
	{
		name:    "binary-log/hex-dump",
		pattern: regexp.MustCompile(`(?m)^[0-9a-f]{8}  `),
	},
	// Long Base64 run: ≥200 consecutive Base64 characters (typical of embedded
	// certificates, encoded binaries, or JWT payloads pasted raw).
	{
		name:    "binary-log/long-base64",
		pattern: regexp.MustCompile(`[A-Za-z0-9+/=]{200,}`),
	},

	// ── 3. Code-dump heuristics ───────────────────────────────────────────────
	// Ten or more consecutive lines starting with a code-level keyword indicate
	// a file was pasted wholesale rather than a concise observation being stored.
	{
		name:    "code-dump/import-block",
		pattern: buildConsecutiveLinesPattern(10, []string{"import ", "from "}),
	},
	{
		name:    "code-dump/declaration-block",
		pattern: buildConsecutiveLinesPattern(10, []string{"function ", "const ", "class "}),
	},
}

// buildConsecutiveLinesPattern returns a *regexp.Regexp that matches when at
// least n consecutive lines each start (after optional leading whitespace) with
// one of the provided prefixes. Uses a possessive-style repetition via {n,} on
// a non-capturing group of one keyword line followed by a newline.
func buildConsecutiveLinesPattern(n int, prefixes []string) *regexp.Regexp {
	alt := "(?:" + strings.Join(prefixes, "|") + ")"
	// One keyword line: optional leading whitespace, one of the prefixes, rest of line.
	line := `[ \t]*` + alt + `[^\n]*`
	// n or more consecutive such lines separated by \n.
	pattern := fmt.Sprintf(`(?m)(?:(?:%s)\n){%d,}`, line, n)
	return regexp.MustCompile(pattern)
}

// containsJunkPattern tests s against every rule in junkRules. It returns the
// matched rule name and true on the first match, or ("", false) when no rule
// matches. The caller uses the name as MatchedPattern in the rejection Error.
func containsJunkPattern(s string) (matchedPattern string, found bool) {
	for _, rule := range junkRules {
		if rule.pattern.MatchString(s) {
			return rule.name, true
		}
	}
	return "", false
}
