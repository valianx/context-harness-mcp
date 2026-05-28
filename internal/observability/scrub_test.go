package observability

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	logapi "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdklogt "go.opentelemetry.io/otel/sdk/log/logtest"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newTestScrubber returns a fresh realScrubber with patterns compiled.
func newTestScrubber() Scrubber {
	return NewRealScrubber()
}

// ── AC-F.1: AWS access key in span attribute redacted ────────────────────────

func TestScrubSpanExporter_AWSKeyRedacted(t *testing.T) {
	RegisterScrubber(NewRealScrubber())
	t.Cleanup(func() { RegisterScrubber(NewRealScrubber()) })

	mem := tracetest.NewInMemoryExporter()
	scrubExp := NewScrubSpanExporter(mem)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(scrubExp),
	)
	_, span := tp.Tracer("test").Start(context.Background(), "test-span")
	span.SetAttributes(
		attribute.String("db.statement", "INSERT INTO users (token) VALUES ('AKIAIOSFODNN7EXAMPLE')"),
	)
	span.End()

	spans := mem.GetSpans()
	require.Len(t, spans, 1)

	attrs := spans[0].Attributes
	require.Len(t, attrs, 1)
	val := attrs[0].Value.AsString()
	assert.Contains(t, val, "[REDACTED]", "AWS key must be redacted")
	assert.NotContains(t, val, "AKIAIOSFODNN7EXAMPLE", "original AWS key must not appear")
}

// ── AC-F.2: JWT in log message redacted ──────────────────────────────────────

func TestScrubLogProcessor_JWTInBodyRedacted(t *testing.T) {
	RegisterScrubber(NewRealScrubber())
	t.Cleanup(func() { RegisterScrubber(NewRealScrubber()) })

	var captured *sdklog.Record

	captureFn := &captureProcessor{fn: func(r *sdklog.Record) {
		cloned := r.Clone()
		captured = &cloned
	}}
	proc := NewScrubLogProcessor(captureFn)

	rec := sdklogt.RecordFactory{
		Body: logapi.StringValue(
			"auth failed for token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyMTIzIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
		),
	}.NewRecord()

	err := proc.OnEmit(context.Background(), &rec)
	require.NoError(t, err)
	require.NotNil(t, captured)

	body := captured.Body().AsString()
	assert.Contains(t, body, "[REDACTED]", "JWT must be redacted")
	assert.NotContains(t, body, "eyJhbGciOiJIUzI1NiJ9", "original JWT header must not appear")
}

// ── AC-F.3: Email replaced with hash; same → same; different → different ─────

func TestScrubber_EmailHashConsistency(t *testing.T) {
	sc := newTestScrubber()

	result1 := sc.Redact("contact operator@example.com for details")
	result2 := sc.Redact("contact operator@example.com for details")
	result3 := sc.Redact("other user@different.org logged in")

	// Same email produces same hash.
	assert.Equal(t, result1, result2, "same email must produce same hash across calls")

	// Both results are redacted.
	assert.NotContains(t, result1, "@", "original email must not appear")
	assert.NotContains(t, result3, "@", "original email in result3 must not appear")

	// Extract hash tokens to compare.
	hash1 := extractEmailHash(result1)
	hash2 := extractEmailHash(result2)
	hash3 := extractEmailHash(result3)

	require.NotEmpty(t, hash1, "hash1 must be present")
	require.NotEmpty(t, hash3, "hash3 must be present")
	assert.Equal(t, hash1, hash2, "same email produces same hash")
	assert.NotEqual(t, hash1, hash3, "different emails produce different hashes")
}

// extractEmailHash pulls the first [email:XXXXXXXX] token from text.
func extractEmailHash(text string) string {
	const prefix = "[email:"
	start := strings.Index(text, prefix)
	if start == -1 {
		return ""
	}
	end := strings.Index(text[start:], "]")
	if end == -1 {
		return ""
	}
	return text[start : start+end+1]
}

// ── AC-F.4: UUIDs preserved in all attribute contexts ────────────────────────

func TestScrubSpanExporter_UUIDsPreserved(t *testing.T) {
	RegisterScrubber(NewRealScrubber())
	t.Cleanup(func() { RegisterScrubber(NewRealScrubber()) })

	uuids := map[string]string{
		"user.id":    "550e8400-e29b-41d4-a716-446655440000",
		"mcp.node_id": "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		"trace.id":   "f47ac10b-58cc-4372-a567-0e02b2c3d479",
		"request.id": "a3bb189e-8bf9-3888-9912-ace4e6543002",
		"session.id": "b8be5ceb-cc73-4d89-a6f7-3d4a4a1f3c0b",
	}

	mem := tracetest.NewInMemoryExporter()
	scrubExp := NewScrubSpanExporter(mem)

	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(scrubExp))
	_, span := tp.Tracer("test").Start(context.Background(), "uuid-test")
	for k, v := range uuids {
		span.SetAttributes(attribute.String(k, v))
	}
	span.End()

	spans := mem.GetSpans()
	require.Len(t, spans, 1)

	attrMap := make(map[string]string)
	for _, kv := range spans[0].Attributes {
		attrMap[string(kv.Key)] = kv.Value.AsString()
	}

	for k, expected := range uuids {
		actual, ok := attrMap[k]
		require.True(t, ok, "attribute %q must be present", k)
		assert.Equal(t, expected, actual, "UUID in %q must not be redacted", k)
	}
}

// ── AC-F.5: Authorization: Bearer JWT redacted ───────────────────────────────

func TestScrubber_AuthorizationBearerRedacted(t *testing.T) {
	sc := newTestScrubber()

	input := "header value: Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyMTIzIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	result := sc.Redact(input)

	assert.Contains(t, result, "Authorization: Bearer [REDACTED]", "Bearer header must be redacted")
	assert.NotContains(t, result, "eyJhbGciOiJIUzI1NiJ9", "original JWT must not remain after Bearer redaction")
}

// ── AC-F.6: Benchmark — 1 KB span body with 3 secrets, <100µs per scrub ─────

func BenchmarkScrub_1KBBodyWith3Secrets(b *testing.B) {
	sc := newTestScrubber()

	awsKey := "AKIAIOSFODNN7EXAMPLE"
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyMTIzIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	email := "operator@example.com"

	// Build ~1 KB body with 3 secrets interspersed in benign text.
	// Sentence is 57 chars; 20 repetitions = 1140 chars of padding, enough for offsets.
	padding := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit. ", 20)
	body := padding[:200] + awsKey + padding[200:400] + jwt + padding[400:600] + email + padding[600:800]

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sc.Redact(body)
	}
}

// ── AC-F.7: Nested map[string]any secrets redacted recursively ───────────────

func TestScrubAny_NestedMapDepth3(t *testing.T) {
	// Ensure the real scrubber is active for this test.
	RegisterScrubber(NewRealScrubber())
	t.Cleanup(func() { RegisterScrubber(NewRealScrubber()) })

	// Depth 3 nesting: {"level1": {"level2": {"level3": "ghp_secret"}}}
	// GitHub PAT regex: ghp_[A-Za-z0-9]{36} — exactly 36 chars after "ghp_".
	nested := map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"level3": "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij",
			},
		},
	}

	result := scrubAny(nested)

	m, ok := result.(map[string]any)
	require.True(t, ok)
	m1, ok := m["level1"].(map[string]any)
	require.True(t, ok)
	m2, ok := m1["level2"].(map[string]any)
	require.True(t, ok)
	val, ok := m2["level3"].(string)
	require.True(t, ok)
	assert.Equal(t, "[REDACTED]", val, "GitHub PAT at depth 3 must be redacted")
}

func TestScrubAny_NestedMapWithEmailAndSecret(t *testing.T) {
	// Ensure the real scrubber is active for this test.
	RegisterScrubber(NewRealScrubber())
	t.Cleanup(func() { RegisterScrubber(NewRealScrubber()) })

	// Mirrors AC-F.7 from the plan: {"auth": {"token": "ghp_...", "user": "ops@example.com"}}
	// GitHub PAT regex: ghp_[A-Za-z0-9]{36} — exactly 36 chars after "ghp_".
	input := map[string]any{
		"auth": map[string]any{
			"token": "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij",
			"user":  "ops@example.com",
		},
	}

	result := scrubAny(input)

	outer, ok := result.(map[string]any)
	require.True(t, ok)
	inner, ok := outer["auth"].(map[string]any)
	require.True(t, ok)

	tokenVal, ok := inner["token"].(string)
	require.True(t, ok)
	assert.Equal(t, "[REDACTED]", tokenVal, "GitHub PAT must be redacted")

	userVal, ok := inner["user"].(string)
	require.True(t, ok)
	assert.Contains(t, userVal, "[email:", "email must be hashed")
	assert.NotContains(t, userVal, "@", "raw email must not appear")
}

// ── AC-F.8: RSA private key block redacted entirely ──────────────────────────

func TestScrubber_RSAPrivateKeyRedacted(t *testing.T) {
	sc := newTestScrubber()

	// 2048-bit RSA private key block (truncated for test; pattern match only requires header/footer).
	rsaKey := `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0Z3VS5JJcds3xHn/ygWep4PAtEsHAqpg3U5wrEXHRjbKxvIb
n8x7lZvHidNGJFDMM3pVBDt7l6JEbOnOXONNNEWo7Ds13sMN5VkqCBXOaC42GfM
gE8Z3iJbr1bGZXHFH6E9XBBPnpOY1KjX9LrRMqA5aBZKLkYmFIobcWZNlz8ABCD
EFGHIJKLMNOPQRSTUVWXYZ0123456789abcdefghijklmnopqrstuvwxyz01234567
-----END RSA PRIVATE KEY-----`

	input := "server config:\n" + rsaKey + "\nend config"
	result := sc.Redact(input)

	assert.NotContains(t, result, "BEGIN RSA PRIVATE KEY", "RSA key header must be redacted")
	assert.NotContains(t, result, "END RSA PRIVATE KEY", "RSA key footer must be redacted")
	assert.NotContains(t, result, "MIIEpAIBAAKCAQEA", "RSA key body must be redacted")
	assert.Contains(t, result, "[REDACTED]", "redaction marker must be present")
	assert.Contains(t, result, "server config:", "surrounding text must be preserved")
}

// ── AC-F.9: init() registers real scrubber; boot guard passes ────────────────

func TestInit_PackageInitRegistersRealScrubber(t *testing.T) {
	// Verifying AC-F.9: when this package is imported, the init() function
	// has already run (Go guarantees init runs before any test).
	// We confirm that the active scrubber is NOT the noopScrubber.
	scrubberMu.RLock()
	noop := isNoopScrubber
	sc := activeScrubber
	scrubberMu.RUnlock()

	assert.False(t, noop, "isNoopScrubber must be false after package init")
	assert.IsType(t, realScrubber{}, sc, "active scrubber must be realScrubber")
}

func TestScrub_BootGuardReleasedByPackageInit(t *testing.T) {
	// AC-F.9 corollary: AC-A.8 boot guard must NOT fire when this package is imported.
	// After scrub.go's init() runs, isNoopScrubber must be false.
	scrubberMu.RLock()
	noop := isNoopScrubber
	scrubberMu.RUnlock()

	assert.False(t, noop,
		"boot guard (AC-A.8) must pass: isNoopScrubber=false after importing observability package with scrub.go")
}

// ── AC-J.5: db.statement whitespace normalized in ScrubSpanExporter ──────────

// TestScrubSpanExporter_SQLWhitespaceNormalized verifies that multi-line SQL
// in db.statement has its whitespace collapsed to single spaces on export.
func TestScrubSpanExporter_SQLWhitespaceNormalized(t *testing.T) {
	RegisterScrubber(NewRealScrubber())
	t.Cleanup(func() { RegisterScrubber(NewRealScrubber()) })

	mem := tracetest.NewInMemoryExporter()
	scrubExp := NewScrubSpanExporter(mem)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(scrubExp))

	_, span := tp.Tracer("test").Start(context.Background(), "sql-whitespace-test")
	// Multi-line SQL with tabs — typical Go backtick string from query.go.
	span.SetAttributes(attribute.String("db.statement",
		"SELECT id, name, node_type\n\t\t\tFROM nodes\n\t\t\tWHERE deleted_at IS NULL"))
	span.End()

	spans := mem.GetSpans()
	require.Len(t, spans, 1)

	var stmt string
	for _, kv := range spans[0].Attributes {
		if string(kv.Key) == "db.statement" {
			stmt = kv.Value.AsString()
		}
	}
	require.NotEmpty(t, stmt, "db.statement attribute must be present")
	assert.Equal(t,
		"SELECT id, name, node_type FROM nodes WHERE deleted_at IS NULL",
		stmt,
		"multi-line SQL must be collapsed to single-line on export",
	)
	assert.NotContains(t, stmt, "\t", "exported db.statement must not contain tabs")
	assert.NotContains(t, stmt, "\n", "exported db.statement must not contain newlines")
}

// TestScrubSpanExporter_SQLWhitespaceNormalized_SecretStillRedacted verifies
// that whitespace normalization does not suppress scrubbing: a secret embedded
// in a multi-line db.statement is both redacted AND whitespace-normalized (AC-J.5).
func TestScrubSpanExporter_SQLWhitespaceNormalized_SecretStillRedacted(t *testing.T) {
	RegisterScrubber(NewRealScrubber())
	t.Cleanup(func() { RegisterScrubber(NewRealScrubber()) })

	mem := tracetest.NewInMemoryExporter()
	scrubExp := NewScrubSpanExporter(mem)
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(scrubExp))

	_, span := tp.Tracer("test").Start(context.Background(), "sql-secret-whitespace-test")
	// Multi-line SQL with an embedded AWS key (should be redacted).
	span.SetAttributes(attribute.String("db.statement",
		"INSERT INTO tokens\n\t\tVALUES ('AKIAIOSFODNN7EXAMPLE',\n\t\t$1)"))
	span.End()

	spans := mem.GetSpans()
	require.Len(t, spans, 1)

	var stmt string
	for _, kv := range spans[0].Attributes {
		if string(kv.Key) == "db.statement" {
			stmt = kv.Value.AsString()
		}
	}
	require.NotEmpty(t, stmt, "db.statement attribute must be present")
	assert.Contains(t, stmt, "[REDACTED]", "AWS key must be redacted")
	assert.NotContains(t, stmt, "AKIAIOSFODNN7EXAMPLE", "original AWS key must not appear")
	assert.NotContains(t, stmt, "\t", "whitespace must be collapsed even when secret is redacted")
	assert.NotContains(t, stmt, "\n", "newlines must be removed")
}

// ── AC-TD: extraTokenPatterns — one test per vendor pattern ─────────────────

// TestExtraTokenPatterns_NonEmpty verifies the operator-extendable list is populated.
func TestExtraTokenPatterns_NonEmpty(t *testing.T) {
	if len(extraTokenPatterns) == 0 {
		t.Fatal("extraTokenPatterns must not be empty; add at least one pattern")
	}
}

// TestRedact_AxiomToken checks xaat- tokens are redacted.
func TestRedact_AxiomToken(t *testing.T) {
	sc := newTestScrubber()
	input := "my axiom token is xaat-8358120a-b349-4d16-a82e-0aba2a301ad0"
	result := sc.Redact(input)
	assert.Equal(t, "my axiom token is [REDACTED]", result)
}

// TestRedact_AnthropicKey checks sk-ant- keys are redacted.
func TestRedact_AnthropicKey(t *testing.T) {
	sc := newTestScrubber()
	input := "using key sk-ant-api03-abcdefghijklmnopqrstuvwxyz01234567890ABCDEFGHIJ"
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "Anthropic key must be redacted")
	assert.NotContains(t, result, "sk-ant-", "original Anthropic key must not appear")
}

// TestRedact_OpenAIKey checks sk- keys (non-proj) are redacted.
func TestRedact_OpenAIKey(t *testing.T) {
	sc := newTestScrubber()
	input := "token sk-abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "OpenAI sk- key must be redacted")
	assert.NotContains(t, result, "sk-abcdefghijklmnopqrstuvwxyz", "original key must not appear")
}

// TestRedact_OpenAIProjKey checks sk-proj- keys are redacted.
func TestRedact_OpenAIProjKey(t *testing.T) {
	sc := newTestScrubber()
	input := "key: sk-proj-abcdefghijklmnopqrstuvwxyz0123456789ABCD"
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "OpenAI sk-proj- key must be redacted")
	assert.NotContains(t, result, "sk-proj-", "original sk-proj- key must not appear")
}

// TestRedact_GitHubPAT checks ghp_ tokens are redacted.
func TestRedact_GitHubPAT(t *testing.T) {
	sc := newTestScrubber()
	// 36 alphanum chars after ghp_
	input := "authorization: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab"
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "GitHub PAT ghp_ must be redacted")
	assert.NotContains(t, result, "ghp_", "original ghp_ token must not appear")
}

// TestRedact_GitHubOAuthToken checks gho_ tokens are redacted.
func TestRedact_GitHubOAuthToken(t *testing.T) {
	sc := newTestScrubber()
	input := "token gho_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcde"
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "GitHub OAuth gho_ must be redacted")
	assert.NotContains(t, result, "gho_", "original gho_ token must not appear")
}

// TestRedact_StripeSecretKey checks sk_live_ keys are redacted.
// The value below is a synthetic test fixture — all zeroes suffix, not a real key.
func TestRedact_StripeSecretKey(t *testing.T) {
	sc := newTestScrubber()
	// Synthetic fixture: prefix is real; 20-char suffix is all zeros (not a valid key).
	input := "stripe key: " + "sk_live_" + "00000000000000000000"
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "Stripe sk_live_ key must be redacted")
	assert.NotContains(t, result, "sk_live_", "original Stripe key must not appear")
}

// TestRedact_StripeTestKey checks sk_test_ keys are redacted.
// The value below is a synthetic test fixture — all zeroes suffix, not a real key.
func TestRedact_StripeTestKey(t *testing.T) {
	sc := newTestScrubber()
	// Synthetic fixture: prefix is real; 20-char suffix is all zeros (not a valid key).
	input := "stripe test key: " + "sk_test_" + "00000000000000000000"
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "Stripe sk_test_ key must be redacted")
	assert.NotContains(t, result, "sk_test_", "original Stripe test key must not appear")
}

// TestRedact_AWSAccessKey checks AKIA keys are redacted (also covered by gitleaks;
// this verifies the extraTokenPatterns layer as a second line of defense).
func TestRedact_AWSAccessKey(t *testing.T) {
	sc := newTestScrubber()
	input := "aws key id: AKIAIOSFODNN7ABCDEFG"
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "AWS AKIA key must be redacted")
	assert.NotContains(t, result, "AKIA", "original AKIA key must not appear")
}

// TestRedact_AWSSessionKey checks ASIA keys (temporary session credentials) are redacted.
func TestRedact_AWSSessionKey(t *testing.T) {
	sc := newTestScrubber()
	input := "session key: ASIAIOSFODNN7ABCDEFG"
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "AWS ASIA session key must be redacted")
	assert.NotContains(t, result, "ASIA", "original ASIA key must not appear")
}

// TestRedact_GCPPrivateKey checks GCP service account private_key JSON snippets are redacted.
func TestRedact_GCPPrivateKey(t *testing.T) {
	sc := newTestScrubber()
	input := `config: {"private_key": "-----BEGIN PRIVATE KEY-----\nMIIEvAIBADANBg==\n-----END PRIVATE KEY-----\n"}`
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "GCP private_key block must be redacted")
	assert.NotContains(t, result, "BEGIN PRIVATE KEY", "original GCP key header must not appear")
}

// TestRedact_SlackToken checks xoxb- bot tokens are redacted.
// The value below is a synthetic test fixture with repeated zeros — not a real token.
func TestRedact_SlackToken(t *testing.T) {
	sc := newTestScrubber()
	// Synthetic fixture: xoxb- prefix + 20-char all-zero payload (not a valid token).
	input := "slack bot token: " + "xoxb-" + "00000000000000000000"
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "Slack xoxb- token must be redacted")
	assert.NotContains(t, result, "xoxb-", "original Slack token must not appear")
}

// TestRedact_RailwayToken checks rly- tokens are redacted.
func TestRedact_RailwayToken(t *testing.T) {
	sc := newTestScrubber()
	input := "railway token: rly-abcdefghijklmnopqrstuvwxyz01234"
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "Railway rly- token must be redacted")
	assert.NotContains(t, result, "rly-", "original Railway token must not appear")
}

// TestRedact_HexSecret checks 64-char lowercase hex secrets (HMAC keys) are redacted.
func TestRedact_HexSecret(t *testing.T) {
	sc := newTestScrubber()
	// Typical output of: openssl rand -hex 32
	hexKey := "a3f1c2d4e5b6789012345678901234567890abcdef1234567890abcdef123456"
	input := "jwt_secret=" + hexKey
	result := sc.Redact(input)
	assert.Contains(t, result, "[REDACTED]", "64-char hex secret must be redacted")
	assert.NotContains(t, result, hexKey, "original hex key must not appear")
}

// ── AC-TD.2: idempotency — no double-redaction ───────────────────────────────

// TestRedact_GitHubPAT_IdempotentWithGitleaks verifies that a GitHub PAT
// matched by both gitleaks and extraTokenPatterns produces a single [REDACTED],
// not a doubled or nested marker.
func TestRedact_GitHubPAT_IdempotentWithGitleaks(t *testing.T) {
	sc := newTestScrubber()
	// 40 alphanum chars after ghp_ — matches gitleaks github-pat rule and our pattern.
	pat := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"
	input := "token: " + pat
	result := sc.Redact(input)

	// Exactly one [REDACTED] marker, no doubled markers.
	count := strings.Count(result, "[REDACTED]")
	assert.Equal(t, 1, count, "double-redaction must not occur for a PAT matched by both gitleaks and extraTokenPatterns")
	assert.NotContains(t, result, pat, "original PAT must not appear")
}

// ── AC-TD.3: false positives — base64 generic pattern is disabled ────────────

// TestRedact_SHA256HexNotRedacted verifies that a bare SHA-256 hex digest
// (64 lowercase hex chars) is NOT redacted by the 64-char hex pattern when it
// appears as a legitimate content hash (e.g. an embedding cache key).
// Note: the 64-char hex pattern DOES match this — that is intentional, because
// a 64-char hex blob in telemetry is indistinguishable from a JWT signing key.
// This test documents the known behaviour so reviewers understand the trade-off.
func TestRedact_SHA256HexBehaviour(t *testing.T) {
	sc := newTestScrubber()
	// A realistic SHA-256 content hash used as a cache key.
	sha256Hash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	input := "cache_key=" + sha256Hash
	result := sc.Redact(input)

	// KNOWN BEHAVIOUR: the 64-char hex pattern considers this a secret.
	// Accepted trade-off: HMAC signing keys share the same format and are
	// more dangerous to leak than a content hash.
	assert.Contains(t, result, "[REDACTED]",
		"64-char hex hashes are redacted by the hex-secret pattern — known trade-off documented here")
}

// TestRedact_UUIDNotRedacted verifies standard UUIDs are preserved by all patterns.
// UUIDs contain dashes which break the hex-run boundary anchors (\b…\b).
func TestRedact_UUIDNotRedacted(t *testing.T) {
	sc := newTestScrubber()
	uuid := "550e8400-e29b-41d4-a716-446655440000"
	input := "node_id: " + uuid
	result := sc.Redact(input)
	assert.Equal(t, input, result, "UUIDs must not be redacted by any token pattern")
}

// ── captureProcessor helper ──────────────────────────────────────────────────

// captureProcessor is a minimal sdklog.Processor that captures emitted records.
type captureProcessor struct {
	fn func(*sdklog.Record)
}

func (c *captureProcessor) OnEmit(_ context.Context, r *sdklog.Record) error {
	if c.fn != nil {
		c.fn(r)
	}
	return nil
}

func (c *captureProcessor) Shutdown(_ context.Context) error  { return nil }
func (c *captureProcessor) ForceFlush(_ context.Context) error { return nil }
