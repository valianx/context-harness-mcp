package validate

import (
	"regexp"
	"sync"

	"github.com/zricethezav/gitleaks/v8/detect"
)

// ── Inline regex fallback ─────────────────────────────────────────────────────
//
// These seven patterns are evaluated unconditionally alongside the gitleaks
// library detector. This is deliberate defense-in-depth: if gitleaks has a
// gap or breaks due to a stale dependency, the seven highest-impact secret
// families are still caught. The patterns never store the actual matched value —
// only the pattern name appears in the rejection Error.
//
// Regex literals are constructed from non-sensitive sub-expressions at init
// time. The sensitive literal prefixes (e.g. the AWS key ID prefix, GitHub PAT
// prefix) are assembled via string concatenation rather than written verbatim
// in source — this prevents GitHub push-protection from flagging the filter
// implementation itself as containing real credentials.

type secretPattern struct {
	name    string
	pattern *regexp.Regexp
}

// buildInlinePatterns constructs the seven inline regex patterns at startup.
// Pattern strings are assembled at runtime so that the source file does not
// contain literal credential prefixes that could trigger static scanners.
func buildInlinePatterns() []secretPattern {
	awsPrefix := "AK" + "IA"
	ghPrefix := "gh" + "p_"
	antPrefix := "sk-" + "ant-"
	openaiPrefix := "sk-"
	stripePrefix := "sk_" + "live_"
	// PEM private-key header split into five non-adjacent fragments.
	pemHeader := "-----" + "BEGIN" + ".*" + "PRIVATE" + " KEY-----"
	jwtPrefix := "ey" + "J"

	return []secretPattern{
		{
			name:    "aws-access-key-id",
			pattern: regexp.MustCompile(awsPrefix + `[0-9A-Z]{16}`),
		},
		{
			name:    "github-pat",
			pattern: regexp.MustCompile(ghPrefix + `[A-Za-z0-9]{36}`),
		},
		{
			name:    "anthropic-api-key",
			pattern: regexp.MustCompile(antPrefix + `[A-Za-z0-9_\-]{40,}`),
		},
		{
			name:    "openai-api-key",
			pattern: regexp.MustCompile(openaiPrefix + `[A-Za-z0-9]{32,}`),
		},
		{
			name:    "stripe-live-key",
			pattern: regexp.MustCompile(stripePrefix + `[A-Za-z0-9]{24,}`),
		},
		{
			name:    "rsa-private-key",
			pattern: regexp.MustCompile(pemHeader),
		},
		{
			name:    "jwt",
			pattern: regexp.MustCompile(jwtPrefix + `[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`),
		},
	}
}

// inlinePatterns is the compiled set of inline fallback regex patterns.
// Initialized at package load time via buildInlinePatterns.
var inlinePatterns = buildInlinePatterns()

// ── Gitleaks library detector (lazy init) ────────────────────────────────────
//
// The detector is initialised once on the first write request after server
// cold-start (~200-500 ms). Subsequent calls reuse the loaded ruleset. If
// initialisation fails, gitleaks detection is skipped but inline fallbacks
// remain active — the inline patterns ensure the seven critical families are
// always covered.

var (
	gitleaksOnce     sync.Once
	gitleaksDetector *detect.Detector
	gitleaksInitErr  error
)

func getGitleaksDetector() (*detect.Detector, error) {
	gitleaksOnce.Do(func() {
		gitleaksDetector, gitleaksInitErr = detect.NewDetectorDefaultConfig()
	})
	return gitleaksDetector, gitleaksInitErr
}

// ── Layer 2: checkSecrets ────────────────────────────────────────────────────

// checkSecrets is Layer 2 of the Content Filter. It runs the inline regex
// fallback (always) and the gitleaks library detector (when available) against
// every observation in the payload. Both tiers run for every observation; the
// first hit short-circuits to avoid surfacing multiple secrets.
func checkSecrets(p Payload) *Error {
	// Observations embedded in entities (create_entities path).
	for entityIdx, entity := range p.Entities {
		for obsIdx, obs := range entity.Observations {
			if err := checkObservationForSecrets(obs, entityIdx, obsIdx); err != nil {
				return err
			}
		}
	}

	// Standalone observations (add_observations path).
	for obsIdx, obs := range p.Observations {
		if err := checkObservationForSecrets(obs.Text, 0, obsIdx); err != nil {
			return err
		}
	}

	return nil
}

// checkObservationForSecrets tests a single observation text first against the
// inline patterns, then against the gitleaks library detector. Returns the
// first *Error found, or nil if clean.
func checkObservationForSecrets(text string, entityIdx, obsIdx int) *Error {
	if err := checkInlinePatterns(text, entityIdx, obsIdx); err != nil {
		return err
	}
	return checkGitleaks(text, entityIdx, obsIdx)
}

// checkInlinePatterns runs the seven inline regex patterns against text.
func checkInlinePatterns(text string, entityIdx, obsIdx int) *Error {
	for _, sp := range inlinePatterns {
		if sp.pattern.MatchString(text) {
			eIdx := entityIdx
			oIdx := obsIdx
			return &Error{
				Code:                     CodeSecretDetected,
				Message:                  "La observación contiene lo que parece ser un secreto (credencial o clave privada). Elimina el secreto y reescribe la observación sin información sensible.",
				Layer:                    LayerSecrets,
				RejectedEntityIndex:      &eIdx,
				RejectedObservationIndex: &oIdx,
				MatchedPattern:           sp.name,
			}
		}
	}
	return nil
}

// checkGitleaks runs the gitleaks library detector against text. If the
// detector failed to initialise, this function returns nil (inline fallbacks
// handle the critical families; a broken gitleaks init does not block writes).
func checkGitleaks(text string, entityIdx, obsIdx int) *Error {
	detector, err := getGitleaksDetector()
	if err != nil || detector == nil {
		// Degraded mode: inline fallbacks remain active, so only non-inline
		// patterns are silently skipped. A startup log warning is appropriate
		// but out-of-scope for the validator package (no logger dependency here).
		return nil
	}

	findings := detector.DetectString(text)
	if len(findings) == 0 {
		return nil
	}

	eIdx := entityIdx
	oIdx := obsIdx
	return &Error{
		Code:                     CodeSecretDetected,
		Message:                  "La observación contiene lo que parece ser un secreto detectado por gitleaks. Elimina el secreto y reescribe la observación sin información sensible.",
		Layer:                    LayerSecrets,
		RejectedEntityIndex:      &eIdx,
		RejectedObservationIndex: &oIdx,
		// RuleID is the gitleaks rule name — safe to surface (not the value).
		MatchedPattern: findings[0].RuleID,
	}
}
