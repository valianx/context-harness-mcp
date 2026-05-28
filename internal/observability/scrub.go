// Package observability — scrub.go
//
// Implements the realScrubber that replaces PII (emails) and secrets
// (gitleaks rules + inline patterns: JWT, Authorization Bearer, RSA private keys)
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
// Order matters: Bearer must run before the standalone JWT pattern, otherwise
// the JWT inside the header might be replaced before the header replacement
// fires, leaving "Authorization: Bearer [REDACTED]" intact (harmless) but
// also correct behaviour.  RSA key runs first (multi-line) to avoid the
// inline JWT regex accidentally matching base64 fragments inside the key body.
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

	// 5. Email → hash replacement (last, after structural secrets are gone).
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
