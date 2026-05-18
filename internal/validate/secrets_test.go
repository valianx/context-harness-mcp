package validate_test

// secrets_test.go: unit tests for the secrets layer (Layer 2) of the Content
// Filter. These tests cover both the existing reject mode (regression) and the
// new redact mode introduced in the security-hardening feature.
//
// Synthetic credential strings are XOR(0x42)+base64 encoded in source — the
// same scheme as tests/validator_test.go — to avoid triggering GitHub
// push-protection on the filter's own test suite.

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// decodeSecret XOR-decodes a base64-encoded test credential.
// All decoded values are clearly synthetic (sequential uppercase + digits)
// and are NOT real credentials.
func decodeSecret(encoded string) string {
	xorBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		panic("decodeSecret: invalid base64: " + err.Error())
	}
	plain := make([]byte, len(xorBytes))
	for i, b := range xorBytes {
		plain[i] = b ^ 0x42
	}
	return string(plain)
}

// awsKeyFixture is a synthetic AWS access key ID (AKIA + 16 uppercase chars).
// Encoded as XOR(0x42)+base64 to avoid GitHub push-protection.
func awsKeyFixture() string {
	return decodeSecret("AwkLA3NwcXZ3dHV6e3IDAAEGBwQ=")
}

// githubPATFixture is a synthetic GitHub PAT (ghp_ prefix + 36 alphanum chars).
func githubPATFixture() string {
	return decodeSecret("JSoyHQMAAQYHBAUKCwgJDg8MDRITEBEWFxQVGhsYc3Bxdnd0dXp7cg==")
}

// ── Redact mode tests ─────────────────────────────────────────────────────────

// TestRedactMode_ReplacesAWSKey verifies that when SECRET_MODE=redact, an
// observation containing an AWS access key has the key replaced with
// [REDACTED] in-place and Run returns nil (call proceeds).
func TestRedactMode_ReplacesAWSKey(t *testing.T) {
	validate.SetSecretMode(validate.SecretModeRedact)
	t.Cleanup(func() { validate.SetSecretMode(validate.SecretModeReject) })

	awsKey := awsKeyFixture()
	obs := "connect with " + awsKey + " for s3"

	p := validate.Payload{
		Observations: []validate.Observation{
			{EntityName: "test-entity", Text: obs},
		},
	}

	result := validate.Run(&p, validate.KindObservations)

	require.Nil(t, result, "redact mode must return nil (call should proceed)")
	assert.NotContains(t, p.Observations[0].Text, awsKey,
		"original secret must not appear in the observation after redaction")
	assert.Contains(t, p.Observations[0].Text, validate.RedactionMarker,
		"observation must contain [REDACTED] after redaction")
	// Context around the match must be preserved.
	assert.Contains(t, p.Observations[0].Text, "connect with",
		"prefix context must be preserved")
	assert.Contains(t, p.Observations[0].Text, "for s3",
		"suffix context must be preserved")
}

// TestRedactMode_MultipleSecrets verifies that when two distinct secret
// patterns appear in the same observation text, both are replaced with
// [REDACTED] and Run returns nil.
func TestRedactMode_MultipleSecrets(t *testing.T) {
	validate.SetSecretMode(validate.SecretModeRedact)
	t.Cleanup(func() { validate.SetSecretMode(validate.SecretModeReject) })

	awsKey := awsKeyFixture()
	ghPAT := githubPATFixture()
	obs := "AWS=" + awsKey + " GH=" + ghPAT

	p := validate.Payload{
		Observations: []validate.Observation{
			{EntityName: "test-entity", Text: obs},
		},
	}

	result := validate.Run(&p, validate.KindObservations)

	require.Nil(t, result, "redact mode must return nil")
	assert.NotContains(t, p.Observations[0].Text, awsKey,
		"AWS key must be redacted")
	assert.NotContains(t, p.Observations[0].Text, ghPAT,
		"GitHub PAT must be redacted")
	assert.GreaterOrEqual(t,
		strings.Count(p.Observations[0].Text, validate.RedactionMarker), 2,
		"at least two [REDACTED] markers must appear for two secret spans")
}

// TestRedactMode_PreservesContextAroundMatch verifies that context before and
// after the secret span is byte-for-byte intact after redaction.
// The prefix and suffix use space characters (not alphanumeric) to avoid
// triggering the binary-log/long-base64 junk pattern (≥200 consecutive
// [A-Za-z0-9+/=] chars) before the secrets layer is reached.
func TestRedactMode_PreservesContextAroundMatch(t *testing.T) {
	validate.SetSecretMode(validate.SecretModeRedact)
	t.Cleanup(func() { validate.SetSecretMode(validate.SecretModeReject) })

	awsKey := awsKeyFixture()
	// Use non-alphanumeric padding so Layer 1 (syntactic) does not reject the
	// payload before Layer 2 (secrets) has a chance to redact.
	prefix := "context-before: " + strings.Repeat("-", 60)
	suffix := strings.Repeat("-", 80) + " :context-after"
	obs := prefix + awsKey + suffix

	p := validate.Payload{
		Observations: []validate.Observation{
			{EntityName: "test-entity", Text: obs},
		},
	}

	result := validate.Run(&p, validate.KindObservations)

	require.Nil(t, result, "redact mode must return nil")

	redacted := p.Observations[0].Text
	assert.True(t, strings.HasPrefix(redacted, prefix),
		"prefix context before the secret must be byte-for-byte intact")
	assert.True(t, strings.HasSuffix(redacted, suffix),
		"suffix context after the secret must be byte-for-byte intact")
	assert.Contains(t, redacted, validate.RedactionMarker,
		"redacted span must contain [REDACTED]")
	assert.NotContains(t, redacted, awsKey,
		"secret must not appear in the redacted observation")
}

// ── Reject mode regression ────────────────────────────────────────────────────

// TestRejectMode_StillWorks verifies that the default reject mode is unaffected
// by the addition of redact mode — a secret payload still returns a non-nil
// *Error with code policy/secret-detected.
func TestRejectMode_StillWorks(t *testing.T) {
	// Default mode is SecretModeReject; no explicit setup needed.
	// Use t.Cleanup to guard against any test running before this one that
	// might have left mode in redact state.
	validate.SetSecretMode(validate.SecretModeReject)
	t.Cleanup(func() { validate.SetSecretMode(validate.SecretModeReject) })

	awsKey := awsKeyFixture()
	obs := "credential: " + awsKey

	p := validate.Payload{
		Observations: []validate.Observation{
			{EntityName: "test-entity", Text: obs},
		},
	}

	result := validate.Run(&p, validate.KindObservations)

	require.NotNil(t, result, "reject mode must return a non-nil *Error")
	assert.Equal(t, validate.CodeSecretDetected, result.Code)
	assert.Equal(t, validate.LayerSecrets, result.Layer)
	// Observation text must be unchanged — no redaction in reject mode.
	assert.Equal(t, obs, p.Observations[0].Text,
		"reject mode must not mutate the observation text")
}
