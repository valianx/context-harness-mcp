// Package validate implements the three-layer Content Filter that guards every
// write path in context-harness-mcp. Layers are evaluated in order (syntactic →
// secrets → taxonomy) with fail-fast semantics: the first non-nil *Error from
// any layer short-circuits the remaining layers. The handler in PR-4 opens a
// pgx.Tx only after Run returns nil.
package validate

import (
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// Stable policy error codes. Downstream consumers (PR-4 tool handlers, Claude
// itself) branch on these — do NOT change them after PR-3 ships.
const (
	CodeSizeExceeded      = "policy/size-exceeded"
	CodeJunkPattern       = "policy/junk-pattern"
	CodeSecretDetected    = "policy/secret-detected"
	CodeTaxonomyViolation = "policy/taxonomy-violation"
)

// Layer names for the Layer field.
const (
	LayerSyntactic = "syntactic"
	LayerSecrets   = "secrets"
	LayerTaxonomy  = "taxonomy"
)

// Kind identifies which logical operation a Payload represents, so Run can
// apply layer-specific rules accordingly.
type Kind int

const (
	KindEntities     Kind = iota // create_entities
	KindObservations             // add_observations
	KindRelations                // create_relations
)

// Entity is the input shape for a single entity in a create_entities call.
type Entity struct {
	Name         string   `json:"name"`
	EntityType   string   `json:"entityType"`
	Observations []string `json:"observations"`
}

// Observation is the input shape for a single observation in an
// add_observations call.
type Observation struct {
	EntityName string `json:"entityName"`
	Text       string `json:"text"`
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
	Entities     []Entity
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

	// RejectedEntityIndex is the zero-based index of the entity that triggered
	// the rejection. Nil when not applicable.
	RejectedEntityIndex *int `json:"rejected_entity_index"`

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
func (e *Error) ToMCPResult() *mcp.CallToolResult {
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
