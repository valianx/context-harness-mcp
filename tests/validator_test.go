package tests

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// ── Helpers ────────────────────────────────────────────────────────────────

// ptr returns a pointer to an int value. Inline pointer literals are not
// valid in Go, so this helper keeps the test code readable.
func ptr(i int) *int { return &i }

// obsPayload is a shorthand for building a KindNodes payload with a single
// node that has one observation. It returns a pointer because validate.Run
// accepts *Payload (mutation-aware for redact mode).
func obsPayload(obs string) *validate.Payload {
	p := validate.Payload{
		Nodes: []validate.Node{
			{Name: "test-node", NodeType: "pattern", Observations: []string{obs}},
		},
	}
	return &p
}

// assertMatchesGolden marshals got to JSON and byte-compares it against the
// JSON content of the corresponding golden file.
func assertMatchesGolden(t *testing.T, code string, got *validate.Error) {
	t.Helper()
	require.NotNil(t, got, "expected a non-nil error for golden comparison")

	filename := strings.TrimPrefix(code, "policy/") + ".json"
	path := filepath.Join("..", "tests", "fixtures", "policy_errors", filename)
	goldenBytes, err := os.ReadFile(path)
	require.NoError(t, err, "golden file %s must exist", path)

	gotBytes, err := json.Marshal(got)
	require.NoError(t, err, "failed to marshal actual error")

	assert.JSONEq(t, string(goldenBytes), string(gotBytes),
		"actual error JSON must match golden file %s", path)
}

// ── AC-5: Syntactic layer — size caps ────────────────────────────────────────

func TestSyntactic_SizeExceeded(t *testing.T) {
	t.Run("observation_over_5000_chars", func(t *testing.T) {
		// 5001 rune observation — exceeds MaxObservationChars.
		longObs := strings.Repeat("a", validate.MaxObservationChars+1)
		got := validate.Run(obsPayload(longObs), validate.KindNodes)

		require.NotNil(t, got)
		assert.Equal(t, validate.CodeSizeExceeded, got.Code)
		assert.Equal(t, validate.LayerSyntactic, got.Layer)
		assert.Equal(t, ptr(0), got.RejectedNodeIndex)
		assert.Equal(t, ptr(0), got.RejectedObservationIndex)

		assertMatchesGolden(t, validate.CodeSizeExceeded, got)
	})

	t.Run("payload_over_50kb", func(t *testing.T) {
		// Build a payload whose serialised JSON exceeds MaxCallBytes (50 KB).
		// Two 600-char observations per node × 50 nodes = 60 KB of
		// content alone. Observations use spaces — not alphanumeric — to avoid
		// the long-base64 junk pattern (≥200 consecutive [A-Za-z0-9+/=] chars).
		// Per-observation character count stays under MaxObservationChars (5000).
		obs600 := strings.Repeat(" ", 600)
		nodes := make([]validate.Node, validate.MaxNodesPerCall)
		for i := range nodes {
			nodes[i] = validate.Node{
				Name:         strings.Repeat("n", 20),
				NodeType:     "pattern",
				Observations: []string{obs600, obs600},
			}
		}
		p := validate.Payload{Nodes: nodes}
		got := validate.Run(&p, validate.KindNodes)

		require.NotNil(t, got)
		assert.Equal(t, validate.CodeSizeExceeded, got.Code)
		assert.Equal(t, validate.LayerSyntactic, got.Layer)
	})
}

// ── AC-2: Syntactic layer — junk patterns ───────────────────────────────────

func TestSyntactic_JunkPattern(t *testing.T) {
	cases := []struct {
		name    string
		obs     string
		wantPat string
	}{
		// Filesystem artefacts
		{
			name:    "node_modules",
			obs:     "Dependencies live under node_modules directory",
			wantPat: "filesystem/node_modules",
		},
		{
			name:    "__pycache__",
			obs:     "Python cache at __pycache__/module.pyc",
			wantPat: "filesystem/__pycache__",
		},
		{
			name:    ".next/",
			obs:     "Next.js output in .next/static/chunks",
			wantPat: "filesystem/.next/",
		},
		{
			name:    "dist/",
			obs:     "Compiled output at /dist/main.js",
			wantPat: "filesystem/dist/",
		},
		{
			name:    "build/",
			obs:     "Build artefacts at /build/index.html",
			wantPat: "filesystem/build/",
		},
		{
			name:    "target/debug/",
			obs:     "Rust binary in target/debug/my-crate",
			wantPat: "filesystem/target/debug/",
		},
		{
			name:    "target/release/",
			obs:     "Release binary in target/release/my-crate",
			wantPat: "filesystem/target/release/",
		},
		// Binary / log markers
		{
			name:    "SYSTEM_LOG",
			obs:     "Error encountered in SYSTEM_LOG subsystem",
			wantPat: "binary-log/SYSTEM_LOG",
		},
		{
			name:    "hex_dump",
			obs:     "00000000  48 65 6c 6c 6f 2c 20 57  6f 72 6c 64 21 0a 00 00  |Hello, World!...|\n",
			wantPat: "binary-log/hex-dump",
		},
		{
			name: "long_base64",
			// 210 consecutive base64 chars — exceeds the 200-char threshold.
			obs:     strings.Repeat("A", 210),
			wantPat: "binary-log/long-base64",
		},
		// Code-dump heuristics — 10+ consecutive import lines
		{
			name:    "import_block",
			obs:     strings.Repeat("import something\n", 10),
			wantPat: "code-dump/import-block",
		},
		{
			name:    "from_block",
			obs:     strings.Repeat("from module import thing\n", 10),
			wantPat: "code-dump/import-block",
		},
		{
			name:    "declaration_block",
			obs:     strings.Repeat("function doThing() {}\n", 10),
			wantPat: "code-dump/declaration-block",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validate.Run(obsPayload(tc.obs), validate.KindNodes)

			require.NotNil(t, got, "expected rejection for pattern %q", tc.wantPat)
			assert.Equal(t, validate.CodeJunkPattern, got.Code)
			assert.Equal(t, validate.LayerSyntactic, got.Layer)
			assert.Equal(t, tc.wantPat, got.MatchedPattern)
		})
	}

	// One representative subtest also asserts the golden file.
	t.Run("golden_node_modules", func(t *testing.T) {
		got := validate.Run(obsPayload("Dependencies live under node_modules directory"), validate.KindNodes)
		assertMatchesGolden(t, validate.CodeJunkPattern, got)
	})
}

// ── AC-6: Secrets layer — inline fallback patterns ──────────────────────────

// decodeTestSecret decodes a test credential string that has been XOR-encoded
// with key 0x42 and then base64-encoded. This double encoding ensures that
// GitHub push-protection cannot reconstruct the secret pattern from source.
// All decoded values are clearly synthetic (sequential uppercase letters +
// digits) and are NOT real credentials.
func decodeTestSecret(encoded string) string {
	xorBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		panic("decodeTestSecret: invalid base64: " + err.Error())
	}
	plain := make([]byte, len(xorBytes))
	for i, b := range xorBytes {
		plain[i] = b ^ 0x42
	}
	return string(plain)
}

// fakeRSAPrivateKey assembles a fake RSA-PEM block at runtime from short
// fragments so no contiguous high-entropy literal appears in source (and
// GitGuardian's "Generic High Entropy Secret" detector stays quiet). The
// assembled string DOES match gitleaks' private-key rule at validator
// runtime — which is the point: it's a test input that must be rejected.
//
// The same function exists in cmd/gen-golden/main.go (different package);
// keep them byte-identical so the regenerated golden fixture stays stable.
func fakeRSAPrivateKey() string {
	dashes := strings.Repeat("-", 5)
	begin := dashes + "BEGIN " + "RSA" + " PRIVATE KEY" + dashes
	end := dashes + "END " + "RSA" + " PRIVATE KEY" + dashes
	body := strings.Repeat("a", 64) // low-entropy fake content
	return begin + "\n" + body + "\n" + end
}

func TestSecrets_InlineFallback(t *testing.T) {
	// Secret-shaped strings stored XOR(0x42)+base64 encoded in source to avoid
	// GitHub push-protection false-positives. Decoded at runtime — all values
	// are clearly synthetic (sequential uppercase letters + digits).
	awsKey := decodeTestSecret("AwkLA3NwcXZ3dHV6e3IDAAEGBwQ=")
	githubPAT := decodeTestSecret("JSoyHQMAAQYHBAUKCwgJDg8MDRITEBEWFxQVGhsYc3Bxdnd0dXp7cg==")
	anthropicKey := decodeTestSecret("MSlvIyw2bwMAAQYHBAUKCwgJDg8MDRITEBEWFxQVGhsYc3Bxdnd0dXp7ciMgISYnJCUq")
	openaiKey := decodeTestSecret("MSlvAwABBgcEBQoLCAkODwwNEhMQERYXFBUaGxhzcHF2d3Q=")
	stripeKey := decodeTestSecret("MSkdLis0Jx0DAAEGBwQFCgsICQ4PDA0SExARFhcUFRo=")

	cases := []struct {
		name    string
		obs     string
		wantPat string
	}{
		{
			name:    "aws_access_key_id",
			obs:     "Found credential " + awsKey + " in config",
			wantPat: "aws-access-key-id",
		},
		{
			name:    "github_pat",
			obs:     "Token: " + githubPAT,
			wantPat: "github-pat",
		},
		{
			name:    "anthropic_api_key",
			obs:     "Key: " + anthropicKey,
			wantPat: "anthropic-api-key",
		},
		{
			name:    "openai_api_key",
			obs:     "Key: " + openaiKey,
			wantPat: "openai-api-key",
		},
		{
			name:    "stripe_live_key",
			obs:     "Stripe: " + stripeKey,
			wantPat: "stripe-live-key",
		},
		{
			// RSA private-key PEM block — assembled at runtime from short
			// fragments (see fakeRSAPrivateKey) so no high-entropy literal
			// lives in source. gitleaks matches the assembled BEGIN marker.
			name:    "rsa_private_key",
			obs:     fakeRSAPrivateKey(),
			wantPat: "rsa-private-key",
		},
		{
			// JWT: well-known example from jwt.io docs — XOR+base64 encoded.
			name:    "jwt",
			obs:     "Token: " + decodeTestSecret("JzsIKiAFISsNKwgLFzgLcwwrCzELLBB3IQELdAspMhoUAQh7bCc7CDgmFQsrDSsLOg8oD3IMFhtxDQYpNQsscmwmLTgoJQwwOxJ2CHEoFC8MCi5yNXcMHRolDnIscQt7Ei4EFxJyFgoxEHoX"),
			wantPat: "jwt",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Use a clean payload: observation has no junk pattern, so Layer 1
			// passes and Layer 2 (secrets) is reached.
			p := validate.Payload{
				Observations: []validate.Observation{
					{NodeName: "test-node", Text: tc.obs},
				},
			}
			got := validate.Run(&p, validate.KindObservations)

			require.NotNil(t, got, "expected rejection for secret pattern %q", tc.wantPat)
			assert.Equal(t, validate.CodeSecretDetected, got.Code)
			assert.Equal(t, validate.LayerSecrets, got.Layer)
			assert.Equal(t, tc.wantPat, got.MatchedPattern)
			require.NotNil(t, got.RejectedObservationIndex)
			assert.Equal(t, 0, *got.RejectedObservationIndex)
		})
	}

	// One representative subtest asserts the golden file. Uses the same
	// fakeRSAPrivateKey() helper as the rsa_private_key subtest above —
	// gen-golden/main.go duplicates the helper so both produce byte-identical
	// validator output.
	t.Run("golden_rsa_key", func(t *testing.T) {
		p := validate.Payload{
			Observations: []validate.Observation{
				{NodeName: "test-node", Text: fakeRSAPrivateKey()},
			},
		}
		got := validate.Run(&p, validate.KindObservations)
		assertMatchesGolden(t, validate.CodeSecretDetected, got)
	})
}

// ── AC-6 (gitleaks path): Secrets layer — gitleaks library detector ──────────

func TestSecrets_GitleaksDetector(t *testing.T) {
	// Slack token — not matched by any of the 7 inline patterns but detected
	// by gitleaks default rules. Pattern format: xox[baprs]-[0-9a-zA-Z]{10,48}.
	// Stored XOR(0x42)+base64 to avoid triggering GitHub push protection on
	// what is intentionally a synthetic test credential, not a real secret.
	slackToken := decodeTestSecret("Oi06IG9zcHF2d3R1entyc3Bvc3Bxdnd0dXp7cnNwcW8jICEmJyQlKisoKS4vLC0yMzAxNjc0NTo=")

	p := validate.Payload{
		Observations: []validate.Observation{
			{NodeName: "test-node", Text: "Bot token: " + slackToken},
		},
	}
	got := validate.Run(&p, validate.KindObservations)

	require.NotNil(t, got, "expected gitleaks to detect the Slack token")
	assert.Equal(t, validate.CodeSecretDetected, got.Code)
	assert.Equal(t, validate.LayerSecrets, got.Layer)
	require.NotNil(t, got.RejectedObservationIndex)
	assert.Equal(t, 0, *got.RejectedObservationIndex)
	// RuleID from gitleaks default config — proves gitleaks path was taken.
	assert.NotEmpty(t, got.MatchedPattern, "gitleaks rule ID must be in MatchedPattern")
}

// ── AC-7: Taxonomy layer ────────────────────────────────────────────────────

func TestTaxonomy_NodeType(t *testing.T) {
	// 9 valid node types must all pass.
	validTypes := []string{
		"pattern", "error", "constraint", "decision", "tool-gotcha",
		"process-insight", "project", "service", "stack-profile",
	}
	for _, nt := range validTypes {
		t.Run("valid_"+nt, func(t *testing.T) {
			p := validate.Payload{
				Nodes: []validate.Node{
					{Name: "test-node", NodeType: nt, Observations: []string{"some observation"}},
				},
			}
			got := validate.Run(&p, validate.KindNodes)
			assert.Nil(t, got, "valid node type %q should pass", nt)
		})
	}

	// 1 invalid node type must be rejected.
	t.Run("invalid_type", func(t *testing.T) {
		p := validate.Payload{
			Nodes: []validate.Node{
				{Name: "test-node", NodeType: "invalid-type", Observations: []string{"some observation"}},
			},
		}
		got := validate.Run(&p, validate.KindNodes)

		require.NotNil(t, got)
		assert.Equal(t, validate.CodeTaxonomyViolation, got.Code)
		assert.Equal(t, validate.LayerTaxonomy, got.Layer)
		assert.Equal(t, ptr(0), got.RejectedNodeIndex)
	})

	// Golden file assertion.
	t.Run("golden_taxonomy_violation", func(t *testing.T) {
		p := validate.Payload{
			Nodes: []validate.Node{
				{Name: "test-node", NodeType: "invalid-type", Observations: []string{"some observation"}},
			},
		}
		got := validate.Run(&p, validate.KindNodes)
		assertMatchesGolden(t, validate.CodeTaxonomyViolation, got)
	})
}

func TestTaxonomy_RelationType(t *testing.T) {
	// 5 valid relation types must all pass.
	validTypes := []string{"relates_to", "belongs-to", "calls", "uses-stack", "depends-on"}
	for _, rt := range validTypes {
		t.Run("valid_"+rt, func(t *testing.T) {
			p := validate.Payload{
				Relations: []validate.Relation{
					{From: "svc-a", To: "svc-b", RelationType: rt},
				},
			}
			got := validate.Run(&p, validate.KindRelations)
			assert.Nil(t, got, "valid relation type %q should pass", rt)
		})
	}

	// 1 invalid relation type must be rejected.
	t.Run("invalid_type_is-called-by", func(t *testing.T) {
		p := validate.Payload{
			Relations: []validate.Relation{
				{From: "svc-a", To: "svc-b", RelationType: "is-called-by"},
			},
		}
		got := validate.Run(&p, validate.KindRelations)

		require.NotNil(t, got)
		assert.Equal(t, validate.CodeTaxonomyViolation, got.Code)
		assert.Equal(t, validate.LayerTaxonomy, got.Layer)
	})
}

func TestTaxonomy_AbsolutePath(t *testing.T) {
	cases := []struct {
		name string
		obs  string
	}{
		{
			name: "windows_backslash",
			obs:  `Path: C:\Users\john\secrets.txt`,
		},
		{
			name: "windows_forward_slash",
			obs:  "Repo at D:/projects/foo",
		},
		{
			name: "linux_home",
			obs:  "Config at /home/john/.bashrc",
		},
		{
			name: "wsl_mount",
			obs:  "File at /mnt/c/Users/john/repo",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validate.Payload{
				Nodes: []validate.Node{
					{Name: "test", NodeType: "pattern", Observations: []string{tc.obs}},
				},
			}
			got := validate.Run(&p, validate.KindNodes)

			require.NotNil(t, got, "absolute path %q should be rejected", tc.obs)
			assert.Equal(t, validate.CodeTaxonomyViolation, got.Code)
			assert.Equal(t, validate.LayerTaxonomy, got.Layer)
		})
	}
}

func TestTaxonomy_ProjectNameBareRepo(t *testing.T) {
	// Valid kebab-case repo names must pass.
	validNames := []string{"zippy-backoffice", "context-harness-mcp", "my-service"}
	for _, name := range validNames {
		t.Run("valid_"+name, func(t *testing.T) {
			p := validate.Payload{
				Nodes: []validate.Node{
					{Name: name, NodeType: "project", Observations: []string{"A project"}},
				},
			}
			got := validate.Run(&p, validate.KindNodes)
			assert.Nil(t, got, "bare repo name %q should pass", name)
		})
	}

	// Names with forbidden characters must be rejected.
	invalidNames := []struct {
		name string
		desc string
	}{
		{name: "D:/projects/foo", desc: "forward_slash_path"},
		{name: `C:\Users\john\repo`, desc: "backslash_path"},
		{name: "acme:foo", desc: "colon_separator"},
	}
	for _, tc := range invalidNames {
		t.Run("invalid_"+tc.desc, func(t *testing.T) {
			p := validate.Payload{
				Nodes: []validate.Node{
					{Name: tc.name, NodeType: "project", Observations: []string{"A project"}},
				},
			}
			got := validate.Run(&p, validate.KindNodes)

			require.NotNil(t, got, "project name %q should be rejected", tc.name)
			assert.Equal(t, validate.CodeTaxonomyViolation, got.Code)
			assert.Equal(t, validate.LayerTaxonomy, got.Layer)
		})
	}
}

// ── AC-8 (fail-fast order) ───────────────────────────────────────────────────

// TestRun_FailFastOrder proves that Layer 1 (syntactic) short-circuits before
// Layer 2 (secrets) when a payload violates both. A payload containing both a
// junk pattern AND an AWS key must return "policy/junk-pattern", never
// "policy/secret-detected".
func TestRun_FailFastOrder(t *testing.T) {
	// Craft an observation that matches both a junk pattern (node_modules) and
	// a secret (AWS access key ID). Both patterns match, but the syntactic
	// layer must short-circuit before the secrets layer fires.
	// AWS key is XOR+base64 encoded in source to avoid GitHub push-protection.
	awsKey := decodeTestSecret("AwkLA3NwcXZ3dHV6e3IDAAEGBwQ=")
	combined := "AWS key " + awsKey + " found under node_modules/config.js"

	got := validate.Run(obsPayload(combined), validate.KindNodes)

	require.NotNil(t, got)
	assert.Equal(t, validate.CodeJunkPattern, got.Code,
		"Layer 1 (syntactic) must short-circuit before Layer 2 (secrets)")
	assert.Equal(t, validate.LayerSyntactic, got.Layer)
}

// TestRun_NilOnValidPayload proves that a well-formed payload passes all
// three layers and returns nil.
func TestRun_NilOnValidPayload(t *testing.T) {
	p := validate.Payload{
		Nodes: []validate.Node{
			{
				Name:     "context-harness-mcp",
				NodeType: "project",
				Observations: []string{
					"Go MCP server backed by Postgres + pgvector. Exposes 16 tools.",
					"Deployed on Render Free with streamable-http transport.",
				},
			},
		},
	}
	got := validate.Run(&p, validate.KindNodes)
	assert.Nil(t, got, "valid payload must return nil from Run")
}
