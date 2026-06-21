package validate

import (
	"fmt"
	"regexp"
	"time"
)

// Flow-event policy error codes. Wire-stable — do NOT change after merge.
const (
	// CodeFlowEventUnknownType is returned when the event field is not in the
	// closed 8-value enum.  The offending token is enum-shaped (no content).
	CodeFlowEventUnknownType = "policy/flow-event-unknown-type"

	// CodeFlowEventBadField is returned when a per-event field value is not in
	// its allowed set (closed sub-enum or numeric boundary).
	CodeFlowEventBadField = "policy/flow-event-bad-field"
)

// LayerFlowEvent identifies the flow-event validation layer in error responses.
const LayerFlowEvent = "flow-event"

// flowEventTypes is the closed 8-value event enum.
// Byte-identical to the emission catalog in agents/orchestrator.md §Flow Telemetry Emission
// (multi-site invariant — both sites must stay in sync).
var flowEventTypes = map[string]bool{
	"guard.block":     true,
	"gate.fail":       true,
	"verify.reject":   true,
	"iteration.loop":  true,
	"blocked":         true,
	"scope.collapse":  true,
	"mcp.unavailable": true,
	"abandon":         true,
}

// taskTypes is the closed enum for the task_type common field.
var taskTypes = map[string]bool{
	"feature":     true,
	"fix":         true,
	"hotfix":      true,
	"refactor":    true,
	"enhancement": true,
	"docs":        true,
	"research":    true,
}

// semverPattern matches a valid semver string (e.g. "2.117.2", "2.118.0-rc.1",
// "2.118.0+build.5"). The pattern is anchored at BOTH ends so a valid prefix
// followed by arbitrary trailing content (e.g. a user path) is rejected.
// fix(sec-001): anchor at $ to prevent path-suffix bypass.
var semverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.\-]+)?$`)

// FlowEventPayload is the wire shape for a record_flow_event call.
// All fields are bounded enums, ints, booleans, a timestamp, or a version
// string — never free-form content.
type FlowEventPayload struct {
	// Common fields (every event).
	Event     string `json:"event"`
	Ts        string `json:"ts"`
	Project   string `json:"project"`
	TaskType  string `json:"task_type"`
	ThVersion string `json:"th_version"`

	// guard.block
	Hook     string `json:"hook,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Resolved *bool  `json:"resolved,omitempty"`

	// gate.fail
	Gate    string `json:"gate,omitempty"`
	Verdict string `json:"verdict,omitempty"`

	// verify.reject — reuses Gate.Verdict fields; Agent is separate.
	Agent string `json:"agent,omitempty"`

	// iteration.loop
	Stage      *int `json:"stage,omitempty"`
	Iterations *int `json:"iterations,omitempty"`

	// scope.collapse
	ItemsDropped *int `json:"items_dropped,omitempty"`

	// mcp.unavailable
	Op string `json:"op,omitempty"`

	// abandon
	LastStage *int `json:"last_stage,omitempty"`
}

// ContainsPathOrSecret reports whether s contains an absolute filesystem path
// or a recognisable secret pattern. It is used to sanitize attacker-controlled
// strings (e.g. the offending event token in a rejection log) before relay.
//
// fix(sec-001): exported so internal/mcp can sanitize the rejection log without
// duplicating the path/secret detection logic from the validate package.
func ContainsPathOrSecret(s string) bool {
	if matchedPath(s) {
		return true
	}
	// Build a one-observation synthetic payload and run the secrets layer.
	synth := Payload{Observations: []Observation{{NodeName: "rejected-event", Text: s}}}
	return checkSecrets(&synth) != nil
}

// ValidateFlowEvent validates a FlowEventPayload against the closed schema.
//
// Validation order:
//  1. Common fields (event enum, ts RFC3339, project naming, task_type, th_version).
//  2. Secret + user-path check via internal/validate.Run (layers 2 + 3 of the
//     Content Filter, using the string fields as synthetic observation text).
//  3. Per-event field validation against the per-event closed sub-enums.
//
// Returns nil when all layers pass.  Returns the first *Error on any violation.
// Possible error codes: CodeFlowEventUnknownType, CodeFlowEventBadField,
// CodeSecretDetected, CodeTaxonomyViolation, CodeProjectNamingViolation.
func ValidateFlowEvent(p FlowEventPayload) *Error {
	if err := validateFlowEventCommon(p); err != nil {
		return err
	}
	if err := checkFlowEventStringsForSecrets(p); err != nil {
		return err
	}
	return validateFlowEventPerEvent(p)
}

// maxFlowEventStringLen is the maximum byte length permitted for any string
// field in a FlowEventPayload. Fields longer than this are rejected before
// any downstream check — a hard upper bound on content size.
// fix(sec-003): explicit size cap keeps all string fields provably bounded.
const maxFlowEventStringLen = 256

// validateFlowEventCommon validates the five common fields present on every event.
func validateFlowEventCommon(p FlowEventPayload) *Error {
	// Reject any string field that exceeds the size cap before further checks.
	// This runs before enum validation so an oversized event string is caught here.
	for _, s := range []string{p.Event, p.Ts, p.Project, p.TaskType, p.ThVersion,
		p.Hook, p.Reason, p.Gate, p.Verdict, p.Agent, p.Op} {
		if len(s) > maxFlowEventStringLen {
			return &Error{
				Code:    CodeFlowEventBadField,
				Message: fmt.Sprintf("Un campo de texto supera el límite de %d bytes permitidos.", maxFlowEventStringLen),
				Layer:   LayerFlowEvent,
			}
		}
	}

	// event — closed 8-value enum.
	if !flowEventTypes[p.Event] {
		return &Error{
			Code:    CodeFlowEventUnknownType,
			Message: fmt.Sprintf("El campo 'event' contiene un valor desconocido: %q. Valores permitidos: guard.block, gate.fail, verify.reject, iteration.loop, blocked, scope.collapse, mcp.unavailable, abandon.", p.Event),
			Layer:   LayerFlowEvent,
		}
	}

	// ts — must be parseable as RFC3339.
	if _, err := time.Parse(time.RFC3339, p.Ts); err != nil {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("El campo 'ts' debe ser una cadena RFC3339 UTC válida (p. ej. '2026-06-21T10:00:00Z'). Valor recibido: %q.", p.Ts),
			Layer:   LayerFlowEvent,
		}
	}

	// project — reuse the existing project-naming validator.
	if verr := Check(p.Project); verr != nil {
		return verr
	}

	// task_type — closed enum.
	if !taskTypes[p.TaskType] {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("El campo 'task_type' contiene un valor desconocido: %q. Valores permitidos: feature, fix, hotfix, refactor, enhancement, docs, research.", p.TaskType),
			Layer:   LayerFlowEvent,
		}
	}

	// th_version — must look like a semver string.
	if !semverPattern.MatchString(p.ThVersion) {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("El campo 'th_version' debe ser una cadena semver (p. ej. '2.117.2'). Valor recibido: %q.", p.ThVersion),
			Layer:   LayerFlowEvent,
		}
	}

	return nil
}

// checkFlowEventStringsForSecrets runs all non-empty string fields through the
// Content Filter's secrets layer (layer 2) and the absolute-path check (layer 3).
// This is the metadata-only guardrail: any field that accidentally contains a
// secret or an absolute path is rejected before relay.
//
// The Payload is constructed from the flow-event's string fields treated as
// synthetic observation texts so the existing Run logic applies without change.
func checkFlowEventStringsForSecrets(p FlowEventPayload) *Error {
	texts := collectFlowEventStrings(p)
	if len(texts) == 0 {
		return nil
	}

	// Build a synthetic Payload with the string fields as observations.
	// The secrets layer checks each observation; the taxonomy layer checks paths.
	obs := make([]Observation, len(texts))
	for i, t := range texts {
		obs[i] = Observation{NodeName: "flow-event", Text: t}
	}
	synth := &Payload{Observations: obs}

	if err := checkSecrets(synth); err != nil {
		return err
	}
	return checkAbsolutePaths(Payload{Observations: obs})
}

// collectFlowEventStrings returns all non-empty string field values from the
// flow event payload that may carry user-influenced content.  Fixed enum tokens
// validated in validateFlowEventCommon are excluded (they are already constrained
// to bounded sets so scanning them is redundant).
//
// Only "free-ish" string fields — those whose values come from TH rather than
// being hardcoded by the server — are included here.  In practice the schema
// defines no truly free-form field, but we include the per-event string fields
// so a drifted client cannot inject content by abusing a field value.
func collectFlowEventStrings(p FlowEventPayload) []string {
	var out []string
	// fix(sec-001): include ThVersion so a path smuggled as a semver suffix is
	// caught by the path-check layer even if it somehow passes the regex gate.
	for _, s := range []string{p.ThVersion, p.Hook, p.Reason, p.Gate, p.Verdict, p.Agent, p.Op} {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// validateFlowEventPerEvent checks the per-event fields against the schema's
// closed sub-enums and numeric boundaries for the given event type.
func validateFlowEventPerEvent(p FlowEventPayload) *Error {
	switch p.Event {
	case "guard.block":
		return validateGuardBlock(p)
	case "gate.fail":
		return validateGateFail(p)
	case "verify.reject":
		return validateVerifyReject(p)
	case "iteration.loop":
		return validateIterationLoop(p)
	case "blocked":
		return validateBlocked(p)
	case "scope.collapse":
		return validateScopeCollapse(p)
	case "mcp.unavailable":
		return validateMCPUnavailable(p)
	case "abandon":
		return validateAbandon(p)
	}
	// Unreachable — event enum was validated in validateFlowEventCommon.
	return nil
}

// ── per-event validators ──────────────────────────────────────────────────────

var (
	hookValues    = map[string]bool{"prepublish": true, "dev": true, "policy": true}
	reasonValues  = map[string]bool{"over-bump": true, "secret": true, "outward": true}
	gateValues    = map[string]bool{"STAGE-GATE-1": true, "STAGE-GATE-2": true, "STAGE-GATE-3": true, "acceptance": true, "plan-review": true}
	verdictValues = map[string]bool{"fail": true, "concerns": true}
	agentValues   = map[string]bool{"qa": true, "security": true, "tester": true, "acceptance": true}
	stageValues   = map[int]bool{1: true, 2: true, 3: true}
	blockedReasons = map[string]bool{"no-dispatch": true, "manual-push": true, "guard": true, "dependency": true}
	opValues      = map[string]bool{"read": true, "write": true}
)

func validateGuardBlock(p FlowEventPayload) *Error {
	if !hookValues[p.Hook] {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("guard.block: 'hook' debe ser uno de {prepublish, dev, policy}. Recibido: %q.", p.Hook),
			Layer:   LayerFlowEvent,
		}
	}
	if !reasonValues[p.Reason] {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("guard.block: 'reason' debe ser uno de {over-bump, secret, outward}. Recibido: %q.", p.Reason),
			Layer:   LayerFlowEvent,
		}
	}
	if p.Resolved == nil {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: "guard.block: el campo 'resolved' (bool) es obligatorio.",
			Layer:   LayerFlowEvent,
		}
	}
	return nil
}

func validateGateFail(p FlowEventPayload) *Error {
	if !gateValues[p.Gate] {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("gate.fail: 'gate' debe ser uno de {STAGE-GATE-1, STAGE-GATE-2, STAGE-GATE-3, acceptance, plan-review}. Recibido: %q.", p.Gate),
			Layer:   LayerFlowEvent,
		}
	}
	if !verdictValues[p.Verdict] {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("gate.fail: 'verdict' debe ser uno de {fail, concerns}. Recibido: %q.", p.Verdict),
			Layer:   LayerFlowEvent,
		}
	}
	return nil
}

func validateVerifyReject(p FlowEventPayload) *Error {
	if !agentValues[p.Agent] {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("verify.reject: 'agent' debe ser uno de {qa, security, tester, acceptance}. Recibido: %q.", p.Agent),
			Layer:   LayerFlowEvent,
		}
	}
	if !verdictValues[p.Verdict] {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("verify.reject: 'verdict' debe ser uno de {fail, concerns}. Recibido: %q.", p.Verdict),
			Layer:   LayerFlowEvent,
		}
	}
	return nil
}

func validateIterationLoop(p FlowEventPayload) *Error {
	if p.Stage == nil || !stageValues[*p.Stage] {
		stageStr := "<nil>"
		if p.Stage != nil {
			stageStr = fmt.Sprintf("%d", *p.Stage)
		}
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("iteration.loop: 'stage' debe ser uno de {1, 2, 3}. Recibido: %s.", stageStr),
			Layer:   LayerFlowEvent,
		}
	}
	if p.Iterations == nil || *p.Iterations < 2 {
		iters := "<nil>"
		if p.Iterations != nil {
			iters = fmt.Sprintf("%d", *p.Iterations)
		}
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("iteration.loop: 'iterations' debe ser un entero ≥ 2. Recibido: %s.", iters),
			Layer:   LayerFlowEvent,
		}
	}
	return nil
}

func validateBlocked(p FlowEventPayload) *Error {
	if !blockedReasons[p.Reason] {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("blocked: 'reason' debe ser uno de {no-dispatch, manual-push, guard, dependency}. Recibido: %q.", p.Reason),
			Layer:   LayerFlowEvent,
		}
	}
	return nil
}

func validateScopeCollapse(p FlowEventPayload) *Error {
	if p.ItemsDropped == nil || *p.ItemsDropped < 1 {
		dropped := "<nil>"
		if p.ItemsDropped != nil {
			dropped = fmt.Sprintf("%d", *p.ItemsDropped)
		}
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("scope.collapse: 'items_dropped' debe ser un entero ≥ 1. Recibido: %s.", dropped),
			Layer:   LayerFlowEvent,
		}
	}
	return nil
}

func validateMCPUnavailable(p FlowEventPayload) *Error {
	if !opValues[p.Op] {
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("mcp.unavailable: 'op' debe ser uno de {read, write}. Recibido: %q.", p.Op),
			Layer:   LayerFlowEvent,
		}
	}
	return nil
}

func validateAbandon(p FlowEventPayload) *Error {
	if p.LastStage == nil || !stageValues[*p.LastStage] {
		stageStr := "<nil>"
		if p.LastStage != nil {
			stageStr = fmt.Sprintf("%d", *p.LastStage)
		}
		return &Error{
			Code:    CodeFlowEventBadField,
			Message: fmt.Sprintf("abandon: 'last_stage' debe ser uno de {1, 2, 3}. Recibido: %s.", stageStr),
			Layer:   LayerFlowEvent,
		}
	}
	return nil
}
