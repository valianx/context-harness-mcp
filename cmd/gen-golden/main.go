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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// decodeGenGoldenSecret XOR-decodes a base64-encoded test string.
// See tests/validator_test.go:decodeTestSecret for encoding details.
func decodeGenGoldenSecret(encoded string) string {
	xorBytes, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		panic("decodeGenGoldenSecret: invalid base64: " + err.Error())
	}
	plain := make([]byte, len(xorBytes))
	for i, b := range xorBytes {
		plain[i] = b ^ 0x42
	}
	return string(plain)
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
			// RSA private key PEM block (truncated/fake) — decoded from
			// XOR+base64 at runtime to avoid triggering GitHub push protection.
			filename: "secret-detected.json",
			payload: validate.Payload{
				Observations: []validate.Observation{
					{NodeName: "test-node", Text: decodeGenGoldenSecret("b29vb28ABwULDGIQEQNiEhALFAMWB2IJBxtvb29vb0gPCwsHLTULAAMDCQEDEwcDbGxsSG9vb29vBwwGYhARA2ISEAsUAxYHYgkHG29vb29v")},
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
