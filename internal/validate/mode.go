package validate

// SecretMode controls how the secrets layer handles a detected credential.
type SecretMode int

const (
	// SecretModeReject (default) aborts the call and returns policy/secret-detected.
	SecretModeReject SecretMode = iota
	// SecretModeRedact replaces every matched secret span with RedactionMarker
	// in-place and lets the call proceed. Matches the OTEL pattern of scrubbing
	// sensitive fields rather than dropping the entire span.
	SecretModeRedact
)

// secretMode is the package-level mode. The zero value (SecretModeReject) is
// the safe default, so new test files that forget to call SetSecretMode
// automatically get rejection behaviour.
var secretMode SecretMode = SecretModeReject

// SetSecretMode configures the secrets-layer mode for the calling process.
// Call once at startup (main.go) or at the top of each test that needs
// redact mode, paired with t.Cleanup(func(){ SetSecretMode(SecretModeReject) }).
func SetSecretMode(m SecretMode) { secretMode = m }
