// Package validate implements the three-layer Content Filter that guards every
// write path in context-harness-mcp. Layers are evaluated in order (syntactic →
// secrets → taxonomy) with fail-fast semantics: the first non-nil *Error from
// any layer short-circuits the remaining layers. The handler opens a pgx.Tx
// only after Run returns nil.
package validate

import (
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mariogutierrez/context-harness-mcp/internal/metrics"
)

// Stable policy error codes. Downstream consumers (tool handlers, Claude
// itself) branch on these — do NOT change them after PR-3 ships.
const (
	CodeSizeExceeded      = "policy/size-exceeded"
	CodeJunkPattern       = "policy/junk-pattern"
	CodeSecretDetected    = "policy/secret-detected"
	CodeTaxonomyViolation = "policy/taxonomy-violation"
	// CodeRateLimited is returned when a client IP exceeds the write-tool rate
	// limit (10 writes per 10 seconds). Clients should back off for the number
	// of seconds indicated in the retry_after_seconds field of the response.
	CodeRateLimited = "policy/rate-limited"

	// CodeProjectNamingViolation rechaza un `project` field que no matchea
	// ^[a-z][a-z0-9-]{0,63}$. Aplicado en handlers de write antes del
	// Content Filter (no es un layer del filter).
	CodeProjectNamingViolation = "policy/project-naming-violation"

	// CodeCrossProjectRelation rechaza una relación entre dos nodos cuyos
	// project_id no coinciden. Aplicado en create_relations y mark_superseded.
	CodeCrossProjectRelation = "policy/cross-project-relation"

	// CodeNodeNotFound es el sentinel structured para "node not found" usado
	// por find_conflicts, mark_superseded, y opcionalmente para reemplazar
	// los errorResult libres de add_observations/update_observations.
	CodeNodeNotFound = "policy/node-not-found"

	// CodeRelationAlreadyExists indica que mark_superseded encontró la
	// relación supersedes(new → old) ya creada. No es un fallo grave —
	// permite al caller manejar idempotencia limpia.
	CodeRelationAlreadyExists = "policy/relation-already-exists"
)

// Layer names for the Layer field.
const (
	LayerSyntactic = "syntactic"
	LayerSecrets   = "secrets"
	LayerTaxonomy  = "taxonomy"
	// LayerRateLimit identifies the per-IP rate-limit layer in error responses.
	// It is separate from the Content Filter layers — rate-limit enforcement
	// happens before the Content Filter runs.
	LayerRateLimit = "rate-limit"
	// LayerProject identifies project-scoping validation errors (naming
	// violations and cross-project relation attempts).
	LayerProject = "project"
)

// RedactionMarker is the replacement text inserted in place of a matched secret
// span when SECRET_MODE=redact. It is defined here so tests can reference the
// same constant without importing the secrets sub-package.
const RedactionMarker = "[REDACTED]"

// Kind identifies which logical operation a Payload represents, so Run can
// apply layer-specific rules accordingly.
type Kind int

const (
	KindNodes        Kind = iota // create_nodes
	KindObservations             // add_observations
	KindRelations                // create_relations
)

// Node is the input shape for a single node in a create_nodes call.
type Node struct {
	Name         string   `json:"name"`
	NodeType     string   `json:"nodeType"`
	Observations []string `json:"observations"`
}

// Observation is the input shape for a single observation in an
// add_observations call.
type Observation struct {
	NodeName string `json:"nodeName"`
	Text     string `json:"text"`
}

// Relation is the input shape for a single relation in a create_relations call.
type Relation struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
}

// Payload is the union of the three write-path shapes. Each call site populates
// only the field that matches its Kind; the others are nil/empty.
type Payload struct {
	Nodes        []Node
	Observations []Observation
	Relations    []Relation
}

// Error is the structured policy rejection returned by any layer. Fields use
// *int pointers so that absent indexes serialise to JSON null rather than 0.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"` // Spanish — Claude surfaces this directly

	// Layer identifies which filter layer produced the rejection.
	Layer string `json:"layer"`

	// RejectedObservationIndex is the zero-based index of the observation that
	// triggered the rejection. Nil when the rejection is not observation-level.
	RejectedObservationIndex *int `json:"rejected_observation_index"`

	// RejectedNodeIndex is the zero-based index of the node that triggered
	// the rejection. Nil when not applicable.
	RejectedNodeIndex *int `json:"rejected_node_index"`

	// MatchedPattern provides debugging context (the pattern name or regex that
	// matched). Never contains the actual secret value.
	MatchedPattern string `json:"matched_pattern,omitempty"`
}

// Error implements the Go error interface so *Error can be returned as error.
func (e *Error) Error() string {
	return fmt.Sprintf("[%s/%s] %s", e.Layer, e.Code, e.Message)
}

// MarshalJSON ensures nil pointer indexes serialise to JSON null rather than
// being omitted or rendering as 0.
func (e *Error) MarshalJSON() ([]byte, error) {
	type Alias Error
	return json.Marshal((*Alias)(e))
}

// ToMCPResult converts a policy *Error into a *mcp.CallToolResult that the MCP
// tool handler can return directly to the caller. The structured error JSON is
// embedded in the text content so Claude can parse and surface it.
// Side-effect: increments mcp_content_filter_rejects_total{code, layer}.
func (e *Error) ToMCPResult() *mcp.CallToolResult {
	metrics.ContentFilterRejects.WithLabelValues(e.Code, e.Layer).Inc()
	payload, _ := json.Marshal(e) // Error is always marshallable; ignore error.
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{
				Type: mcp.ContentTypeText,
				Text: string(payload),
			},
		},
		IsError: true,
	}
}
