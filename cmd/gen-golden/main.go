// gen-golden is a one-shot CLI that generates the golden fixture files in
// tests/fixtures/policy_errors/. Run it from the repo root:
//
//	go run ./cmd/gen-golden/
//
// Each golden file is the JSON-marshalled *validate.Error for a canonical
// known-bad input. The validator_test.go suite loads each file and asserts
// byte-for-byte (JSONEq) equality against the live output.
//
// This command is intended to be run once (or whenever the error message
// changes), not as part of normal CI.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// fakeRSAPrivateKey assembles a fake RSA-PEM block at runtime from short
// fragments so no contiguous high-entropy literal appears in source (which
// previously triggered GitGuardian's "Generic High Entropy Secret" detector).
// The assembled string DOES match gitleaks' private-key rule at validator
// runtime — which is the point: it's a test input that must be rejected.
//
// The same function exists in tests/validator_test.go (different package);
// keep them byte-identical so the regenerated golden fixture stays stable.
func fakeRSAPrivateKey() string {
	dashes := strings.Repeat("-", 5)
	begin := dashes + "BEGIN " + "RSA" + " PRIVATE KEY" + dashes
	end := dashes + "END " + "RSA" + " PRIVATE KEY" + dashes
	body := strings.Repeat("a", 64) // low-entropy fake content
	return begin + "\n" + body + "\n" + end
}

func main() {
	dir := "tests/fixtures/policy_errors"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir: %v\n", err)
		os.Exit(1)
	}

	cases := []struct {
		filename string
		payload  validate.Payload
		kind     validate.Kind
	}{
		{
			filename: "size-exceeded.json",
			payload: validate.Payload{
				Nodes: []validate.Node{
					{
						Name:         "test-node",
						NodeType:     "pattern",
						Observations: []string{strings.Repeat("a", validate.MaxObservationChars+1)},
					},
				},
			},
			kind: validate.KindNodes,
		},
		{
			filename: "junk-pattern.json",
			payload: validate.Payload{
				Nodes: []validate.Node{
					{
						Name:         "test-node",
						NodeType:     "pattern",
						Observations: []string{"Dependencies live under node_modules directory"},
					},
				},
			},
			kind: validate.KindNodes,
		},
		{
			// RSA private-key PEM block — assembled at runtime from short
			// fragments via fakeRSAPrivateKey() so no high-entropy literal
			// lives in source. The assembled string matches gitleaks at
			// validator runtime, producing the policy/secret-detected error
			// that this golden file captures.
			filename: "secret-detected.json",
			payload: validate.Payload{
				Observations: []validate.Observation{
					{NodeName: "test-node", Text: fakeRSAPrivateKey()},
				},
			},
			kind: validate.KindObservations,
		},
		{
			filename: "taxonomy-violation.json",
			payload: validate.Payload{
				Nodes: []validate.Node{
					{
						Name:         "test-node",
						NodeType:     "invalid-type",
						Observations: []string{"some observation"},
					},
				},
			},
			kind: validate.KindNodes,
		},
	}

	for _, c := range cases {
		result := validate.Run(&c.payload, c.kind)
		if result == nil {
			fmt.Fprintf(os.Stderr, "ERROR: payload for %s did not produce a rejection\n", c.filename)
			os.Exit(1)
		}

		data, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(os.Stderr, "marshal error for %s: %v\n", c.filename, err)
			os.Exit(1)
		}

		path := dir + "/" + c.filename
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write error for %s: %v\n", path, err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s (%d bytes)\n", path, len(data))
	}
}
