package validate

// Run is the single public entry point for the Content Filter.
//
// It evaluates the three layers in order:
//  1. syntactic — size caps + junk-pattern denylist
//  2. secrets — gitleaks-as-library + inline regex fallback
//  3. taxonomy — entity_type enum, relation_type enum, absolute-path rejection,
//     project-name bare-repo-name rule
//
// The first non-nil *Error from any layer is returned immediately
// (fail-fast); the remaining layers are skipped. When all layers pass, Run
// returns nil.
//
// PR-4 tool handlers call this function once per write request, before opening
// any pgx.Tx. A nil return means the payload cleared all policy gates and the
// handler may proceed to insert.
func Run(p Payload, _ Kind) *Error {
	if err := checkSyntactic(p); err != nil {
		return err
	}
	if err := checkSecrets(p); err != nil {
		return err
	}
	return checkTaxonomy(p)
}
