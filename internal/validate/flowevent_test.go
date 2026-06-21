package validate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptr helpers for pointer fields.
func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// syntheticAntropicKey builds a test fixture that looks like an Anthropic API
// key (sk-ant-*) without embedding the literal in source — mirrors the
// technique in validate/secrets.go to avoid static-scanner false positives.
func syntheticAntropicKey() string {
	return "sk-" + "ant-" + "api03-abcdefghijklmnopqrstuvwxyz01234567890ABCDEFGHIJ"
}

// ── well-formed payloads — one for each event type ───────────────────────────

func TestValidateFlowEvent_WellFormed_GuardBlock(t *testing.T) {
	p := FlowEventPayload{
		Event:     "guard.block",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "team-harness",
		TaskType:  "feature",
		ThVersion: "2.117.2",
		Hook:      "dev",
		Reason:    "over-bump",
		Resolved:  boolPtr(true),
	}
	assert.Nil(t, ValidateFlowEvent(p), "well-formed guard.block must pass")
}

func TestValidateFlowEvent_WellFormed_GateFail(t *testing.T) {
	p := FlowEventPayload{
		Event:     "gate.fail",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "my-service",
		TaskType:  "fix",
		ThVersion: "2.117.0",
		Gate:      "STAGE-GATE-2",
		Verdict:   "fail",
	}
	assert.Nil(t, ValidateFlowEvent(p))
}

func TestValidateFlowEvent_WellFormed_VerifyReject(t *testing.T) {
	p := FlowEventPayload{
		Event:     "verify.reject",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "context-harness-mcp",
		TaskType:  "hotfix",
		ThVersion: "2.117.2",
		Agent:     "security",
		Verdict:   "concerns",
	}
	assert.Nil(t, ValidateFlowEvent(p))
}

func TestValidateFlowEvent_WellFormed_IterationLoop(t *testing.T) {
	p := FlowEventPayload{
		Event:      "iteration.loop",
		Ts:         "2026-06-21T10:00:00Z",
		Project:    "my-service",
		TaskType:   "refactor",
		ThVersion:  "2.117.2",
		Stage:      intPtr(2),
		Iterations: intPtr(3),
	}
	assert.Nil(t, ValidateFlowEvent(p))
}

func TestValidateFlowEvent_WellFormed_Blocked(t *testing.T) {
	p := FlowEventPayload{
		Event:     "blocked",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "my-service",
		TaskType:  "docs",
		ThVersion: "2.117.2",
		Reason:    "manual-push",
	}
	assert.Nil(t, ValidateFlowEvent(p))
}

func TestValidateFlowEvent_WellFormed_ScopeCollapse(t *testing.T) {
	p := FlowEventPayload{
		Event:        "scope.collapse",
		Ts:           "2026-06-21T10:00:00Z",
		Project:      "my-service",
		TaskType:     "enhancement",
		ThVersion:    "2.117.2",
		ItemsDropped: intPtr(2),
	}
	assert.Nil(t, ValidateFlowEvent(p))
}

func TestValidateFlowEvent_WellFormed_MCPUnavailable(t *testing.T) {
	p := FlowEventPayload{
		Event:     "mcp.unavailable",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "my-service",
		TaskType:  "research",
		ThVersion: "2.117.2",
		Op:        "write",
	}
	assert.Nil(t, ValidateFlowEvent(p))
}

func TestValidateFlowEvent_WellFormed_Abandon(t *testing.T) {
	p := FlowEventPayload{
		Event:     "abandon",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "my-service",
		TaskType:  "feature",
		ThVersion: "2.117.2",
		LastStage: intPtr(3),
	}
	assert.Nil(t, ValidateFlowEvent(p))
}

// ── AC-1.2: unknown event type → CodeFlowEventUnknownType ───────────────────

func TestValidateFlowEvent_UnknownEvent(t *testing.T) {
	p := FlowEventPayload{
		Event:     "unknown.event",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "team-harness",
		TaskType:  "feature",
		ThVersion: "2.117.2",
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr, "unknown event must be rejected")
	assert.Equal(t, CodeFlowEventUnknownType, verr.Code)
	assert.Equal(t, LayerFlowEvent, verr.Layer)
}

func TestValidateFlowEvent_EmptyEvent(t *testing.T) {
	p := FlowEventPayload{
		Event:     "",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "team-harness",
		TaskType:  "feature",
		ThVersion: "2.117.2",
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventUnknownType, verr.Code)
}

// ── common field validation ──────────────────────────────────────────────────

func TestValidateFlowEvent_BadTimestamp(t *testing.T) {
	p := FlowEventPayload{
		Event:     "guard.block",
		Ts:        "not-a-timestamp",
		Project:   "team-harness",
		TaskType:  "feature",
		ThVersion: "2.117.2",
		Hook:      "dev",
		Reason:    "over-bump",
		Resolved:  boolPtr(false),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_BadProject(t *testing.T) {
	p := FlowEventPayload{
		Event:     "guard.block",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "UPPERCASE",
		TaskType:  "feature",
		ThVersion: "2.117.2",
		Hook:      "dev",
		Reason:    "over-bump",
		Resolved:  boolPtr(false),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeProjectNamingViolation, verr.Code)
}

func TestValidateFlowEvent_BadTaskType(t *testing.T) {
	p := FlowEventPayload{
		Event:     "guard.block",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "team-harness",
		TaskType:  "invalid-type",
		ThVersion: "2.117.2",
		Hook:      "dev",
		Reason:    "over-bump",
		Resolved:  boolPtr(false),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_BadThVersion(t *testing.T) {
	p := FlowEventPayload{
		Event:     "guard.block",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "team-harness",
		TaskType:  "feature",
		ThVersion: "not-semver",
		Hook:      "dev",
		Reason:    "over-bump",
		Resolved:  boolPtr(false),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

// ── AC-1.3: secret in any string field → CodeSecretDetected ─────────────────

func TestValidateFlowEvent_SecretInField_Rejected(t *testing.T) {
	// Embed a synthetic credential in the 'hook' field to simulate a drifted
	// client accidentally passing content through an enum field.
	// Key assembled at runtime — same technique as validate/secrets.go —
	// so the source file does not contain a literal credential prefix.
	p := FlowEventPayload{
		Event:     "guard.block",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "team-harness",
		TaskType:  "feature",
		ThVersion: "2.117.2",
		Hook:      syntheticAntropicKey(),
		Reason:    "over-bump",
		Resolved:  boolPtr(false),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr, "a secret in a field must be rejected")
	assert.Equal(t, CodeSecretDetected, verr.Code, "expected policy/secret-detected, got %s", verr.Code)
}

func TestValidateFlowEvent_AbsolutePathInField_Rejected(t *testing.T) {
	p := FlowEventPayload{
		Event:     "guard.block",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "team-harness",
		TaskType:  "feature",
		ThVersion: "2.117.2",
		Hook:      "dev",
		Reason:    "/home/alice/projects/something",
		Resolved:  boolPtr(false),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr, "an absolute path in a field must be rejected")
	assert.Equal(t, CodeTaxonomyViolation, verr.Code, "expected policy/taxonomy-violation, got %s", verr.Code)
}

// ── per-event field validation ───────────────────────────────────────────────

func TestValidateFlowEvent_GuardBlock_BadHook(t *testing.T) {
	p := FlowEventPayload{
		Event: "guard.block", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		Hook: "unknown-hook", Reason: "over-bump", Resolved: boolPtr(false),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_GuardBlock_BadReason(t *testing.T) {
	p := FlowEventPayload{
		Event: "guard.block", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		Hook: "dev", Reason: "bad-reason", Resolved: boolPtr(false),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_GuardBlock_NilResolved(t *testing.T) {
	p := FlowEventPayload{
		Event: "guard.block", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		Hook: "dev", Reason: "over-bump", Resolved: nil,
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_GateFail_BadGate(t *testing.T) {
	p := FlowEventPayload{
		Event: "gate.fail", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		Gate: "STAGE-GATE-99", Verdict: "fail",
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_GateFail_BadVerdict(t *testing.T) {
	p := FlowEventPayload{
		Event: "gate.fail", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		Gate: "STAGE-GATE-1", Verdict: "pass",
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_VerifyReject_BadAgent(t *testing.T) {
	p := FlowEventPayload{
		Event: "verify.reject", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		Agent: "unknown-agent", Verdict: "fail",
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_IterationLoop_BadStage(t *testing.T) {
	p := FlowEventPayload{
		Event: "iteration.loop", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		Stage: intPtr(5), Iterations: intPtr(2),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_IterationLoop_IterationsLessThan2(t *testing.T) {
	p := FlowEventPayload{
		Event: "iteration.loop", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		Stage: intPtr(1), Iterations: intPtr(1),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_Blocked_BadReason(t *testing.T) {
	p := FlowEventPayload{
		Event: "blocked", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		Reason: "unknown-reason",
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_ScopeCollapse_ItemsDroppedZero(t *testing.T) {
	p := FlowEventPayload{
		Event: "scope.collapse", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		ItemsDropped: intPtr(0),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_MCPUnavailable_BadOp(t *testing.T) {
	p := FlowEventPayload{
		Event: "mcp.unavailable", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		Op: "delete",
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

func TestValidateFlowEvent_Abandon_BadLastStage(t *testing.T) {
	p := FlowEventPayload{
		Event: "abandon", Ts: "2026-06-21T10:00:00Z",
		Project: "team-harness", TaskType: "feature", ThVersion: "2.117.2",
		LastStage: intPtr(0),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr)
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

// ── SEC-001 regression: semver prefix bypass must be rejected ────────────────

// TestValidateFlowEvent_ThVersionPathSuffix_Rejected asserts that a value that
// has a valid semver prefix followed by a user path is rejected by the semver
// validator. Before SEC-001 the unanchored regex admitted this value; the `$`
// anchor fix closes that bypass.
func TestValidateFlowEvent_ThVersionPathSuffix_Rejected(t *testing.T) {
	p := FlowEventPayload{
		Event:     "guard.block",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "team-harness",
		TaskType:  "feature",
		ThVersion: "2.117.2 /home/alice/private",
		Hook:      "dev",
		Reason:    "over-bump",
		Resolved:  boolPtr(false),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr, "semver with path suffix must be rejected — SEC-001 regression guard")
	// The value exceeds the semver pattern ($ anchor) so the bad-field error fires
	// before the path-check layer, but either code is acceptable here.
	assert.True(t,
		verr.Code == CodeFlowEventBadField || verr.Code == CodeTaxonomyViolation,
		"expected bad-field or taxonomy-violation, got %s", verr.Code)
}

// TestValidateFlowEvent_ThVersionPreRelease_Accepted asserts that a legitimate
// pre-release semver is still accepted after adding the $ anchor.
func TestValidateFlowEvent_ThVersionPreRelease_Accepted(t *testing.T) {
	p := FlowEventPayload{
		Event:     "gate.fail",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "team-harness",
		TaskType:  "feature",
		ThVersion: "2.118.0-rc.1",
		Gate:      "STAGE-GATE-1",
		Verdict:   "fail",
	}
	assert.Nil(t, ValidateFlowEvent(p), "valid pre-release semver must still be accepted")
}

// TestValidateFlowEvent_ThVersionBuildMeta_Accepted asserts that build-metadata
// semver is still accepted after adding the $ anchor.
func TestValidateFlowEvent_ThVersionBuildMeta_Accepted(t *testing.T) {
	p := FlowEventPayload{
		Event:     "gate.fail",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "team-harness",
		TaskType:  "feature",
		ThVersion: "2.118.0+build.5",
		Gate:      "STAGE-GATE-2",
		Verdict:   "concerns",
	}
	assert.Nil(t, ValidateFlowEvent(p), "valid build-metadata semver must still be accepted")
}

// TestValidateFlowEvent_StringFieldTooLong_Rejected asserts that a string field
// exceeding maxFlowEventStringLen bytes is rejected — SEC-003 size-cap guard.
func TestValidateFlowEvent_StringFieldTooLong_Rejected(t *testing.T) {
	longValue := string(make([]byte, maxFlowEventStringLen+1))
	p := FlowEventPayload{
		Event:     "guard.block",
		Ts:        "2026-06-21T10:00:00Z",
		Project:   "team-harness",
		TaskType:  "feature",
		ThVersion: longValue,
		Hook:      "dev",
		Reason:    "over-bump",
		Resolved:  boolPtr(false),
	}
	verr := ValidateFlowEvent(p)
	require.NotNil(t, verr, "a field exceeding the byte cap must be rejected")
	assert.Equal(t, CodeFlowEventBadField, verr.Code)
}

// ── ClosedEventEnum — all 8 values are in the map ───────────────────────────

func TestFlowEventTypes_Contains8Values(t *testing.T) {
	expected := []string{
		"guard.block", "gate.fail", "verify.reject", "iteration.loop",
		"blocked", "scope.collapse", "mcp.unavailable", "abandon",
	}
	assert.Len(t, flowEventTypes, 8, "flowEventTypes must contain exactly 8 values")
	for _, v := range expected {
		assert.True(t, flowEventTypes[v], "flowEventTypes must contain %q", v)
	}
}
