package validate

import (
	"fmt"
	"regexp"
)

// DefaultProjectID is the implicit project for all nodes and relations created
// before v0.4.0 and for any write call that omits the project field.
const DefaultProjectID = "global"

// projectIDPattern is the allowed format for project identifiers:
// starts with a lowercase letter; if length > 1, the remaining chars are
// lowercase letters, digits, or dashes, and the last char must be alphanumeric
// (no trailing dash). Total length 1–64 characters.
//
// Note: the architecture spec listed ^[a-z][a-z0-9-]{0,63}$ but AC-4 explicitly
// rejects "foo-" (trailing dash). This pattern ^[a-z]([a-z0-9-]{0,62}[a-z0-9])?$
// satisfies all AC-4 boundary cases while preserving the same intent.
var projectIDPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,62}[a-z0-9])?$`)

// Check returns nil if name matches ^[a-z][a-z0-9-]{0,63}$, or a *Error
// (Code=CodeProjectNamingViolation, Layer=LayerProject) otherwise.
//
// Allowed examples: "global", "zippy-backoffice", "a", "foo-123".
// Rejected examples: "", "Foo", "-foo", "foo-", "foo_bar", "123abc",
// strings of 65+ characters.
func Check(name string) *Error {
	if projectIDPattern.MatchString(name) {
		return nil
	}
	return &Error{
		Code:    CodeProjectNamingViolation,
		Message: fmt.Sprintf("El project id %q no es válido. Debe coincidir con ^[a-z][a-z0-9-]{0,63}$ (minúsculas, dígitos y guiones, empieza con letra, máximo 64 caracteres).", name),
		Layer:   LayerProject,
	}
}
