// Package observability — scrub.go
//
// Implements the realScrubber that replaces PII (emails) and secrets
// (gitleaks rules + inline patterns: JWT, Authorization Bearer, RSA private keys,
// and the operator-extendable token denylist in extraTokenPatterns)
// with safe redaction markers before telemetry data leaves the process.
//
// This file's package-level init() calls RegisterScrubber(NewRealScrubber()),
// which releases the AC-A.8 boot guard defined in init.go.  Importing this
// package from main.go is sufficient — no explicit call is required.
//
// Wrapping strategy:
//   - Spans: ScrubSpanExporter wraps an underlying sdktrace.SpanExporter.
//     ReadOnlySpan is immutable; scrubbing happens on export via a thin
//     scrubbed-span adapter.
//   - Logs: ScrubLogProcessor wraps an underlying sdklog.Processor.
//     sdklog.Record is mutable; scrubbing edits Body + attributes in-place
//     before delegating to the downstream processor.
package observability

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	logapi "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// spaceCollapse collapses any run of whitespace (space, tab, newline, carriage
// return, form feed) into a single space. Compiled once at package init.
var spaceCollapse = regexp.MustCompile(`\s+`)

// ── Operator-extendable token denylist ───────────────────────────────────────
//
// extraTokenPatterns is a slice of compiled regexps for vendor token formats
// that gitleaks does not always cover (custom API key formats, new vendors,
// etc.).  The list lives in code — not an env var — so that every change is
// grep-able and reviewed in a PR.
//
// To add a new pattern:
//  1. Append a regexp.MustCompile(…) entry to extraTokenPatterns below.
//  2. Add one test in scrub_test.go that embeds a sample token and asserts
//     Redact returns "[REDACTED]" in its place.
//
// Why this exists: gitleaks ships ~150 upstream rules and is the primary
// detector, but its rule set lags new vendor formats and does not cover
// highly-specific tokens from smaller vendors (e.g. Axiom xaat- tokens,
// Railway rly- tokens).  extraTokenPatterns gives the operator direct control
// without waiting for an upstream gitleaks release.
//
// Application order: gitleaks runs first (broadest ruleset), then
// extraTokenPatterns (vendor-specific), then the inline patterns below
// (Bearer, JWT, RSA, email).  The pipeline is idempotent: if gitleaks already
// replaced a token with "[REDACTED]", the extra-pattern pass is a no-op for
// that span because the original token bytes are gone.
var extraTokenPatterns = []*regexp.Regexp{
	// Axiom ingest tokens — xaat-<uuid-like>
	regexp.MustCompile(`xaat-[a-zA-Z0-9\-]{20,}`),

	// Anthropic API keys — sk-ant-*
	regexp.MustCompile(`sk-ant-[a-zA-Z0-9_\-]{20,}`),

	// OpenAI API keys — sk-* (includes sk-proj-*)
	regexp.MustCompile(`sk-(proj-)?[a-zA-Z0-9_\-]{20,}`),

	// GitHub Personal Access Tokens — ghp_, gho_, ghu_, ghs_, ghr_
	regexp.MustCompile(`gh[opusr]_[A-Za-z0-9]{36,}`),

	// Stripe secret + publishable keys
	regexp.MustCompile(`(sk|pk)_(live|test)_[a-zA-Z0-9]{20,}`),

	// AWS access key IDs — AKIA + 16 uppercase alphanum
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),

	// AWS temporary session token key IDs — ASIA + 16
	regexp.MustCompile(`ASIA[0-9A-Z]{16}`),

	// GCP service-account JSON private key block (multi-line via (?s))
	regexp.MustCompile(`(?s)"private_key"\s*:\s*"-----BEGIN[^"]+-----END[^"]+"`),

	// Slack tokens — xoxp-, xoxb-, xoxa-, xoxs-, xoxr-
	regexp.MustCompile(`xox[pbars]\-[A-Za-z0-9\-]{20,}`),

	// Railway API tokens
	regexp.MustCompile(`rly-[A-Za-z0-9\-]{20,}`),

	// Hex secrets of 64+ chars (HMAC secrets, JWT signing keys — e.g. openssl rand -hex 32).
	// Matches lowercase hex only; does NOT match UUIDs (dashes break the run) or
	// base64 (uppercase + symbols not included here).
	regexp.MustCompile(`\b[a-f0-9]{64,}\b`),

	// Optional aggressive pattern — generic base64-encoded blobs of 40+ chars.
	// Disabled by default: a SHA-256 hex digest (64 lowercase hex chars) does NOT
	// match this pattern because hex is a strict subset of base64; however, real
	// base64 with uppercase letters, '+', or '/' WILL match and could catch
	// legitimate embedding hashes or long IDs.  Uncomment if you want more
	// aggressive scrubbing and are willing to accept occasional false positives.
	// regexp.MustCompile(`\b[A-Za-z0-9+/]{40,}={0,2}\b`),
}

// ── Compiled patterns (init once, reuse forever) ─────────────────────────────

var (
	compiledOnce sync.Once

	// reAuthBearer matches "Authorization: Bearer <token>" where token is any
	// non-whitespace sequence.  The entire header value is preserved except
	// the token, which becomes [REDACTED].
	reAuthBearer *regexp.Regexp

	// reJWT matches a standalone three-part base64url JWT.
	// Pattern: ey<head>.<payload>.<signature>
	// The leading "ey" anchors to a real JWT header without matching UUIDs.
	reJWT *regexp.Regexp

	// reEmail matches a simple user@domain.tld pattern.
	// Deliberately loose (covers most real emails) but avoids UUID substrings.
	reEmail *regexp.Regexp

	// reRSAKey matches a full PEM RSA private key block (multi-line via (?s)).
	reRSAKey *regexp.Regexp
)

func initPatterns() {
	compiledOnce.Do(func() {
		reAuthBearer = regexp.MustCompile(
			`(Authorization:\s*Bearer\s+)\S+`,
		)
		reJWT = regexp.MustCompile(
			`ey[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`,
		)
		reEmail = regexp.MustCompile(
			`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`,
		)
		reRSAKey = regexp.MustCompile(
			`(?s)-----BEGIN[^\n]*PRIVATE KEY-----.*?-----END[^\n]*PRIVATE KEY-----`,
		)
	})
}

// ── realScrubber ─────────────────────────────────────────────────────────────

// realScrubber implements the Scrubber interface.  It applies gitleaks rules
// and four additional inline patterns (Bearer auth, JWT, email → hash, RSA key).
type realScrubber struct{}

// NewRealScrubber returns a realScrubber ready for use.
// Patterns are compiled once at package init — this constructor is cheap.
func NewRealScrubber() Scrubber {
	initPatterns()
	return realScrubber{}
}

// Redact applies all scrubbing patterns to text and returns the sanitised result.
//
// Pipeline order (each stage sees the output of the previous):
//  1. RSA private key block — multi-line, must run before per-line patterns.
//  2. Authorization: Bearer header — captures the whole header value.
//  3. Standalone JWT — ey…; runs after Bearer so the header match fires first.
//  4. Gitleaks detector — broadest upstream ruleset (~150 rules).
//  5. extraTokenPatterns — operator-controlled vendor-specific list (this file).
//  6. Email → hash — last, after all structural secrets are gone.
//
// The pipeline is idempotent: if a token was already replaced with "[REDACTED]"
// by an earlier stage, later stages are no-ops for that token.
func (realScrubber) Redact(text string) string {
	if text == "" {
		return text
	}
	initPatterns()

	// 1. RSA private key block (multi-line; must run before per-line patterns).
	text = reRSAKey.ReplaceAllString(text, "[REDACTED]")

	// 2. Authorization: Bearer <token>.
	text = reAuthBearer.ReplaceAllString(text, "${1}[REDACTED]")

	// 3. Standalone JWT (ey...).
	text = reJWT.ReplaceAllString(text, "[REDACTED]")

	// 4. Gitleaks detector (reuses the detector from internal/validate, no duplication).
	text = redactViaGitleaks(text)

	// 5. Vendor-specific token patterns (operator-extendable; see extraTokenPatterns).
	for _, re := range extraTokenPatterns {
		text = re.ReplaceAllString(text, "[REDACTED]")
	}

	// 6. Email → hash replacement (last, after structural secrets are gone).
	text = reEmail.ReplaceAllStringFunc(text, emailToHash)

	return text
}

// redactViaGitleaks calls the shared gitleaks detector from internal/validate.
// If the detector is unavailable (init error) we degrade gracefully — the four
// inline patterns above still cover the critical credential families.
func redactViaGitleaks(text string) string {
	detector := validate.GitleaksDetector()
	if detector == nil {
		return text
	}

	findings := detector.DetectString(text)
	if len(findings) == 0 {
		return text
	}

	// Replace from tail to head to preserve byte offsets.
	for i := 0; i < len(findings)-1; i++ {
		for j := i + 1; j < len(findings); j++ {
			if findings[j].StartColumn > findings[i].StartColumn {
				findings[i], findings[j] = findings[j], findings[i]
			}
		}
	}

	b := []byte(text)
	for _, f := range findings {
		start := f.StartColumn - 1 // gitleaks columns are 1-indexed
		end := f.EndColumn
		if start < 0 || end > len(b) || start >= end {
			continue
		}
		b = append(b[:start], append([]byte("[REDACTED]"), b[end:]...)...)
	}
	return string(b)
}

// emailToHash replaces a matched email with a stable 8-char SHA256 hex prefix.
// Same email always produces the same hash (deterministic grouping in Axiom)
// but the original address is not recoverable.
func emailToHash(email string) string {
	sum := sha256.Sum256([]byte(email))
	return fmt.Sprintf("[email:%x]", sum[:4])
}

// ── Recursive attribute scrubbing ────────────────────────────────────────────

// redactString applies Redact to a single string using the active scrubber.
func redactString(s string) string {
	scrubberMu.RLock()
	sc := activeScrubber
	scrubberMu.RUnlock()
	return sc.Redact(s)
}

// scrubAttributeValue redacts secrets inside an attribute.Value.
// Strings are scrubbed directly.  Other types pass through unchanged.
// Nested maps/slices are handled via scrubAny.
func scrubAttributeValue(v attribute.Value) attribute.Value {
	switch v.Type() {
	case attribute.STRING:
		return attribute.StringValue(redactString(v.AsString()))
	case attribute.STRINGSLICE:
		sl := v.AsStringSlice()
		out := make([]string, len(sl))
		for i, s := range sl {
			out[i] = redactString(s)
		}
		return attribute.StringSliceValue(out)
	default:
		return v
	}
}

// scrubAttributes returns a new slice with all string values redacted.
// For SQL attributes ("db.statement", "db.query.text") an additional
// whitespace-collapse pass runs after scrubbing so that multi-line backtick
// Go strings arrive in Axiom as single-line, human-readable SQL (AC-J.5).
func scrubAttributes(attrs []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, len(attrs))
	for i, kv := range attrs {
		scrubbed := attribute.KeyValue{
			Key:   kv.Key,
			Value: scrubAttributeValue(kv.Value),
		}
		// Normalize whitespace in SQL statement attrs after scrubbing.
		// Single pass: scrub first (secrets), then collapse (readability).
		if (kv.Key == "db.statement" || kv.Key == "db.query.text") &&
			scrubbed.Value.Type() == attribute.STRING {
			normalized := spaceCollapse.ReplaceAllString(scrubbed.Value.AsString(), " ")
			scrubbed.Value = attribute.StringValue(normalized)
		}
		out[i] = scrubbed
	}
	return out
}

// scrubAny recursively descends into map[string]any and []any, redacting
// string leaves and returning a new value (original is not mutated).
// AC-F.7: depth is unbounded but protected by Go's call stack.
func scrubAny(v any) any {
	switch val := v.(type) {
	case string:
		return redactString(val)
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, vv := range val {
			out[k] = scrubAny(vv)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, vv := range val {
			out[i] = scrubAny(vv)
		}
		return out
	default:
		return v
	}
}

// ── ScrubSpanExporter ─────────────────────────────────────────────────────────

// ScrubSpanExporter wraps an underlying SpanExporter and redacts PII/secrets
// from every span's attributes, name, and event messages before export.
//
// Because ReadOnlySpan is immutable, scrubbing is done via scrubbedSpan, a
// thin struct that overrides the string-producing methods.
type ScrubSpanExporter struct {
	delegate sdktrace.SpanExporter
}

// NewScrubSpanExporter wraps delegate with scrubbing.
func NewScrubSpanExporter(delegate sdktrace.SpanExporter) *ScrubSpanExporter {
	initPatterns()
	return &ScrubSpanExporter{delegate: delegate}
}

// ExportSpans redacts each span then delegates to the wrapped exporter.
func (e *ScrubSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	scrubbed := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		scrubbed[i] = newScrubbedSpan(s)
	}
	return e.delegate.ExportSpans(ctx, scrubbed)
}

// Shutdown forwards to the delegate.
func (e *ScrubSpanExporter) Shutdown(ctx context.Context) error {
	return e.delegate.Shutdown(ctx)
}

// scrubbedSpan is a ReadOnlySpan adapter that lazily redacts string attributes.
// All other methods delegate to the original span transparently.
type scrubbedSpan struct {
	sdktrace.ReadOnlySpan
	scrubbedAttrs []attribute.KeyValue
}

func newScrubbedSpan(s sdktrace.ReadOnlySpan) *scrubbedSpan {
	return &scrubbedSpan{
		ReadOnlySpan:  s,
		scrubbedAttrs: scrubAttributes(s.Attributes()),
	}
}

// Attributes returns the redacted attribute set.
func (s *scrubbedSpan) Attributes() []attribute.KeyValue {
	return s.scrubbedAttrs
}

// Name returns the scrubbed span name (defensive — span names rarely carry PII
// but we scrub them anyway to avoid accidental leakage).
func (s *scrubbedSpan) Name() string {
	return redactString(s.ReadOnlySpan.Name())
}

// ── ScrubLogProcessor ─────────────────────────────────────────────────────────

// ScrubLogProcessor wraps an underlying sdklog.Processor and redacts PII/secrets
// from the log Body and all attributes before delegating to the downstream processor.
type ScrubLogProcessor struct {
	delegate sdklog.Processor
}

// NewScrubLogProcessor wraps delegate with scrubbing.
func NewScrubLogProcessor(delegate sdklog.Processor) *ScrubLogProcessor {
	initPatterns()
	return &ScrubLogProcessor{delegate: delegate}
}

// OnEmit redacts the record body and attributes, then delegates.
// sdklog.Record is mutable so edits happen directly.
func (p *ScrubLogProcessor) OnEmit(ctx context.Context, record *sdklog.Record) error {
	// Scrub body string if present.
	if record.Body().Kind() == logapi.KindString {
		record.SetBody(logapi.StringValue(redactString(record.Body().AsString())))
	}

	// Collect + redact attributes.
	var redacted []logapi.KeyValue
	record.WalkAttributes(func(kv logapi.KeyValue) bool {
		redacted = append(redacted, scrubLogKV(kv))
		return true
	})
	if len(redacted) > 0 {
		record.SetAttributes(redacted...)
	}

	return p.delegate.OnEmit(ctx, record)
}

// Shutdown forwards to the delegate.
func (p *ScrubLogProcessor) Shutdown(ctx context.Context) error {
	return p.delegate.Shutdown(ctx)
}

// ForceFlush forwards to the delegate.
func (p *ScrubLogProcessor) ForceFlush(ctx context.Context) error {
	return p.delegate.ForceFlush(ctx)
}

// scrubLogKV redacts the value of a log attribute key-value pair.
func scrubLogKV(kv logapi.KeyValue) logapi.KeyValue {
	switch kv.Value.Kind() {
	case logapi.KindString:
		return logapi.String(kv.Key, redactString(kv.Value.AsString()))
	case logapi.KindMap:
		pairs := kv.Value.AsMap()
		out := make([]logapi.KeyValue, len(pairs))
		for i, p := range pairs {
			out[i] = scrubLogKV(p)
		}
		return logapi.Map(kv.Key, out...)
	case logapi.KindSlice:
		elems := kv.Value.AsSlice()
		out := make([]logapi.Value, len(elems))
		for i, v := range elems {
			if v.Kind() == logapi.KindString {
				out[i] = logapi.StringValue(redactString(v.AsString()))
			} else {
				out[i] = v
			}
		}
		return logapi.Slice(kv.Key, out...)
	default:
		return kv
	}
}

// ── Package init — releases AC-A.8 boot guard ────────────────────────────────

func init() {
	RegisterScrubber(NewRealScrubber())
}
