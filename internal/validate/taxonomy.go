package validate

import (
	"regexp"
	"sort"
	"strings"
)

// NodeTypes is the closed enum of allowed node_type values, sourced from
// claude-dev-team/docs/kg-content-policy.md and mirrored in the `check`
// constraint in migrations/00001_init.sql. A future policy update must touch
// all three places atomically.
var NodeTypes = map[string]bool{
	"pattern":         true,
	"error":           true,
	"constraint":      true,
	"decision":        true,
	"tool-gotcha":     true,
	"process-insight": true,
	"project":         true,
	"service":         true,
	"stack-profile":   true,
}

// RelationTypes is the closed enum of allowed relation_type values.
var RelationTypes = map[string]bool{
	"relates_to": true,
	"belongs-to": true,
	"calls":      true,
	"uses-stack": true,
	"depends-on": true,
}

// absolutePathPatterns matches absolute filesystem paths that embed user names
// or machine-specific roots. These are forbidden by kg-content-policy.md
// because they are neither portable nor shareable across the team.
//
// Covered formats:
//   - Windows: C:\Users\... or D:\Projects\... (drive letter + backslash)
//   - Windows with forward slashes: C:/Users/... or D:/projects/...
//   - Unix/macOS: /home/... or /root/...
//   - WSL mount points: /mnt/c/Users/...
var absolutePathPatterns = []*regexp.Regexp{
	// Windows drive letter paths (backslash or forward slash variants).
	regexp.MustCompile(`(?i)[A-Za-z]:[/\\]`),
	// Unix home or root paths.
	regexp.MustCompile(`/home/[^/\s]`),
	regexp.MustCompile(`/root/`),
	// WSL mount paths (e.g. /mnt/c/Users/...).
	regexp.MustCompile(`/mnt/[a-z]/`),
}

// projectNameForbidden matches characters that are illegal in a project node
// name. Project-type nodes must use bare kebab-case repo names (e.g.
// "zippy-backoffice"), never paths that include slashes, backslashes, or
// colons.
var projectNameForbidden = regexp.MustCompile(`[/\\:]`)

// checkTaxonomy is Layer 3 of the Content Filter. It validates node types,
// relation types, absolute-path presence in observation text, and project-name
// format. Returns the first violation found, or nil.
func checkTaxonomy(p Payload) *Error {
	if err := checkNodeTypes(p); err != nil {
		return err
	}
	if err := checkRelationTypes(p); err != nil {
		return err
	}
	if err := checkAbsolutePaths(p); err != nil {
		return err
	}
	return checkProjectNames(p)
}

// checkNodeTypes validates that every node's NodeType is in the allowed enum.
// Returns on the first invalid type.
func checkNodeTypes(p Payload) *Error {
	for idx, node := range p.Nodes {
		if !NodeTypes[node.NodeType] {
			nIdx := idx
			return &Error{
				Code:              "policy/taxonomy-violation",
				Message:           "El tipo de nodo '" + node.NodeType + "' no es válido. Usa uno de: " + joinNodeTypes() + ".",
				Layer:             LayerTaxonomy,
				RejectedNodeIndex: &nIdx,
			}
		}
	}
	return nil
}

// checkRelationTypes validates that every relation's RelationType is in the
// allowed enum. Returns on the first invalid type.
func checkRelationTypes(p Payload) *Error {
	for idx, rel := range p.Relations {
		if !RelationTypes[rel.RelationType] {
			nIdx := idx
			return &Error{
				Code:              "policy/taxonomy-violation",
				Message:           "El tipo de relación '" + rel.RelationType + "' no es válido. Usa uno de: " + joinRelationTypes() + ".",
				Layer:             LayerTaxonomy,
				RejectedNodeIndex: &nIdx,
			}
		}
	}
	return nil
}

// checkAbsolutePaths scans every observation text (embedded in nodes and
// standalone) for absolute filesystem paths. Absolute paths with user names
// are forbidden by kg-content-policy.md because they are machine-specific and
// not portable.
func checkAbsolutePaths(p Payload) *Error {
	for nodeIdx, node := range p.Nodes {
		for obsIdx, obs := range node.Observations {
			if matchedPath(obs) {
				nIdx := nodeIdx
				oIdx := obsIdx
				return &Error{
					Code:                     "policy/taxonomy-violation",
					Message:                  "La observación contiene una ruta absoluta de sistema de archivos. Las rutas absolutas no son portables. Usa el nombre del repositorio o una ruta relativa en su lugar.",
					Layer:                    LayerTaxonomy,
					RejectedNodeIndex:        &nIdx,
					RejectedObservationIndex: &oIdx,
				}
			}
		}
	}

	for obsIdx, obs := range p.Observations {
		if matchedPath(obs.Text) {
			oIdx := obsIdx
			return &Error{
				Code:                     "policy/taxonomy-violation",
				Message:                  "La observación contiene una ruta absoluta de sistema de archivos. Las rutas absolutas no son portables. Usa el nombre del repositorio o una ruta relativa en su lugar.",
				Layer:                    LayerTaxonomy,
				RejectedObservationIndex: &oIdx,
			}
		}
	}

	return nil
}

// matchedPath returns true if s contains an absolute path per absolutePathPatterns.
func matchedPath(s string) bool {
	for _, re := range absolutePathPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// checkProjectNames validates that every node with NodeType "project" has a
// name that is a plain kebab-case repository name (no slashes, backslashes, or
// colons). Per kg-content-policy.md the project name must be the bare repo
// name (e.g. "zippy-backoffice"), not a path.
func checkProjectNames(p Payload) *Error {
	for idx, node := range p.Nodes {
		if node.NodeType != "project" {
			continue
		}
		if projectNameForbidden.MatchString(node.Name) {
			nIdx := idx
			return &Error{
				Code:              "policy/taxonomy-violation",
				Message:           "El nombre de nodo tipo 'project' debe ser el nombre bare del repositorio en kebab-case (p. ej. 'zippy-backoffice'), sin rutas ni separadores de directorio.",
				Layer:             LayerTaxonomy,
				RejectedNodeIndex: &nIdx,
			}
		}
	}
	return nil
}

// joinNodeTypes returns a sorted, comma-separated list of allowed node types
// for use in error messages. Sorted output ensures message stability across Go
// versions (map iteration order is intentionally random).
func joinNodeTypes() string {
	types := make([]string, 0, len(NodeTypes))
	for t := range NodeTypes {
		types = append(types, t)
	}
	sort.Strings(types)
	return strings.Join(types, ", ")
}

// joinRelationTypes returns a sorted, comma-separated list of allowed relation
// types for use in error messages.
func joinRelationTypes() string {
	types := make([]string, 0, len(RelationTypes))
	for t := range RelationTypes {
		types = append(types, t)
	}
	sort.Strings(types)
	return strings.Join(types, ", ")
}
