package validate

import (
	"encoding/json"
)

// Size caps for syntactic validation. These are defined in-code, not as env
// vars, so a misconfigured deploy cannot accidentally relax the filter.
const (
	// MaxObservationChars is the maximum number of UTF-8 characters allowed in
	// a single observation. Observations longer than this are almost certainly
	// pasted files rather than concise knowledge entries.
	MaxObservationChars = 5_000

	// MaxCallBytes is the maximum serialised size (in bytes) of an entire write
	// call payload. A payload larger than 50 KB is outside the expected usage
	// envelope for knowledge-graph writes.
	MaxCallBytes = 50 * 1_024

	// MaxNodesPerCall is the maximum number of nodes in a single create_nodes
	// call. Batches larger than this are atypical and may indicate automated
	// scraping or abuse.
	MaxNodesPerCall = 50
)

// checkSyntactic is Layer 1 of the Content Filter. It validates size caps and
// the junk-pattern denylist. Any violation short-circuits the remaining layers.
func checkSyntactic(p Payload) *Error {
	if err := checkPayloadByteSize(p); err != nil {
		return err
	}
	if err := checkNodeCap(p); err != nil {
		return err
	}
	return checkObservationTexts(p)
}

// checkPayloadByteSize rejects payloads whose serialised JSON exceeds
// MaxCallBytes. The JSON encoding is a consistent proxy for "how much data is
// the caller trying to write in one shot".
func checkPayloadByteSize(p Payload) *Error {
	data, _ := json.Marshal(p) // Payload is always marshallable.
	if len(data) > MaxCallBytes {
		return &Error{
			Code:    CodeSizeExceeded,
			Message: "El payload supera el límite de 50 KB por llamada. Divide los nodos u observaciones en llamadas más pequeñas.",
			Layer:   LayerSyntactic,
		}
	}
	return nil
}

// checkNodeCap rejects calls that attempt to create more than MaxNodesPerCall
// nodes in a single request.
func checkNodeCap(p Payload) *Error {
	if len(p.Nodes) > MaxNodesPerCall {
		return &Error{
			Code:    CodeSizeExceeded,
			Message: "La llamada supera el límite de 50 nodos por llamada. Divide en llamadas más pequeñas.",
			Layer:   LayerSyntactic,
		}
	}
	return nil
}

// checkObservationTexts iterates every observation across all nodes (for
// KindNodes payloads) and standalone observations (for KindObservations
// payloads). It checks the per-observation character cap and the junk-pattern
// denylist, returning the first violation found.
func checkObservationTexts(p Payload) *Error {
	// Observations embedded in nodes (create_nodes path).
	for nodeIdx, node := range p.Nodes {
		for obsIdx, obs := range node.Observations {
			if err := checkSingleObservation(obs, nodeIdx, obsIdx); err != nil {
				return err
			}
		}
	}

	// Standalone observations (add_observations path).
	for obsIdx, obs := range p.Observations {
		if err := checkSingleObservation(obs.Text, 0, obsIdx); err != nil {
			return err
		}
	}

	return nil
}

// checkSingleObservation applies the per-observation size cap and junk-pattern
// denylist to a single text string. nodeIdx is the node's index when the
// observation is embedded in a node; for standalone observations it is 0.
func checkSingleObservation(text string, nodeIdx, obsIdx int) *Error {
	if len([]rune(text)) > MaxObservationChars {
		nIdx := nodeIdx
		oIdx := obsIdx
		return &Error{
			Code:                     CodeSizeExceeded,
			Message:                  "Una observación supera el límite de 5.000 caracteres. Divide el contenido en observaciones más cortas.",
			Layer:                    LayerSyntactic,
			RejectedNodeIndex:        &nIdx,
			RejectedObservationIndex: &oIdx,
		}
	}

	if pattern, found := containsJunkPattern(text); found {
		nIdx := nodeIdx
		oIdx := obsIdx
		return &Error{
			Code:                     CodeJunkPattern,
			Message:                  "La observación contiene un patrón no permitido (artefacto de filesystem, log binario o volcado de código). Reescribe la observación como conocimiento conciso.",
			Layer:                    LayerSyntactic,
			RejectedNodeIndex:        &nIdx,
			RejectedObservationIndex: &oIdx,
			MatchedPattern:           pattern,
		}
	}

	return nil
}
