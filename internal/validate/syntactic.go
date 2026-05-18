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

	// MaxEntitiesPerCall is the maximum number of entities in a single
	// create_entities call. Batches larger than this are atypical and may
	// indicate automated scraping or abuse.
	MaxEntitiesPerCall = 50
)

// checkSyntactic is Layer 1 of the Content Filter. It validates size caps and
// the junk-pattern denylist. Any violation short-circuits the remaining layers.
func checkSyntactic(p Payload) *Error {
	if err := checkPayloadByteSize(p); err != nil {
		return err
	}
	if err := checkEntityCap(p); err != nil {
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
			Message: "El payload supera el límite de 50 KB por llamada. Divide las entidades u observaciones en llamadas más pequeñas.",
			Layer:   LayerSyntactic,
		}
	}
	return nil
}

// checkEntityCap rejects calls that attempt to create more than MaxEntitiesPerCall
// entities in a single request.
func checkEntityCap(p Payload) *Error {
	if len(p.Entities) > MaxEntitiesPerCall {
		return &Error{
			Code:    CodeSizeExceeded,
			Message: "La llamada supera el límite de 50 entidades por llamada. Divide en llamadas más pequeñas.",
			Layer:   LayerSyntactic,
		}
	}
	return nil
}

// checkObservationTexts iterates every observation across all entities (for
// KindEntities payloads) and standalone observations (for KindObservations
// payloads). It checks the per-observation character cap and the junk-pattern
// denylist, returning the first violation found.
func checkObservationTexts(p Payload) *Error {
	// Observations embedded in entities (create_entities path).
	for entityIdx, entity := range p.Entities {
		for obsIdx, obs := range entity.Observations {
			if err := checkSingleObservation(obs, entityIdx, obsIdx); err != nil {
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
// denylist to a single text string. entityIdx is the entity's index when the
// observation is embedded in an entity; for standalone observations it is 0.
func checkSingleObservation(text string, entityIdx, obsIdx int) *Error {
	if len([]rune(text)) > MaxObservationChars {
		eIdx := entityIdx
		oIdx := obsIdx
		return &Error{
			Code:                     CodeSizeExceeded,
			Message:                  "Una observación supera el límite de 5.000 caracteres. Divide el contenido en observaciones más cortas.",
			Layer:                    LayerSyntactic,
			RejectedEntityIndex:      &eIdx,
			RejectedObservationIndex: &oIdx,
		}
	}

	if pattern, found := containsJunkPattern(text); found {
		eIdx := entityIdx
		oIdx := obsIdx
		return &Error{
			Code:                     CodeJunkPattern,
			Message:                  "La observación contiene un patrón no permitido (artefacto de filesystem, log binario o volcado de código). Reescribe la observación como conocimiento conciso.",
			Layer:                    LayerSyntactic,
			RejectedEntityIndex:      &eIdx,
			RejectedObservationIndex: &oIdx,
			MatchedPattern:           pattern,
		}
	}

	return nil
}
