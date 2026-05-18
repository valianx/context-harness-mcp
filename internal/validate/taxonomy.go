package validate

import (
	"regexp"
	"sort"
	"strings"
)

// EntityTypes is the closed enum of allowed entity_type values, sourced from
// claude-dev-team/docs/kg-content-policy.md and mirrored in the `check`
// constraint in migrations/00001_init.sql. A future policy update must touch
// all three places atomically.
var EntityTypes = map[string]bool{
	"pattern":        true,
	"error":          true,
	"constraint":     true,
	"decision":       true,
	"tool-gotcha":    true,
	"process-insight": true,
	"project":        true,
	"service":        true,
	"stack-profile":  true,
}

// RelationTypes is the closed enum of allowed relation_type values.
var RelationTypes = map[string]bool{
	"relates_to":  true,
	"belongs-to":  true,
	"calls":       true,
	"uses-stack":  true,
	"depends-on":  true,
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

// projectNameForbidden matches characters that are illegal in a project entity
// name. Project-type entities must use bare kebab-case repo names (e.g.
// "zippy-backoffice"), never paths that include slashes, backslashes, or
// colons.
var projectNameForbidden = regexp.MustCompile(`[/\\:]`)

// checkTaxonomy is Layer 3 of the Content Filter. It validates entity types,
// relation types, absolute-path presence in observation text, and project-name
// format. Returns the first violation found, or nil.
func checkTaxonomy(p Payload) *Error {
	if err := checkEntityTypes(p); err != nil {
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

// checkEntityTypes validates that every entity's EntityType is in the allowed
// enum. Returns on the first invalid type.
func checkEntityTypes(p Payload) *Error {
	for idx, entity := range p.Entities {
		if !EntityTypes[entity.EntityType] {
			eIdx := idx
			return &Error{
				Code:                "policy/taxonomy-violation",
				Message:             "El tipo de entidad '" + entity.EntityType + "' no es válido. Usa uno de: " + joinEntityTypes() + ".",
				Layer:               LayerTaxonomy,
				RejectedEntityIndex: &eIdx,
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
			eIdx := idx
			return &Error{
				Code:                "policy/taxonomy-violation",
				Message:             "El tipo de relación '" + rel.RelationType + "' no es válido. Usa uno de: " + joinRelationTypes() + ".",
				Layer:               LayerTaxonomy,
				RejectedEntityIndex: &eIdx,
			}
		}
	}
	return nil
}

// checkAbsolutePaths scans every observation text (embedded in entities and
// standalone) for absolute filesystem paths. Absolute paths with user names
// are forbidden by kg-content-policy.md because they are machine-specific and
// not portable.
func checkAbsolutePaths(p Payload) *Error {
	for entityIdx, entity := range p.Entities {
		for obsIdx, obs := range entity.Observations {
			if matchedPath(obs) {
				eIdx := entityIdx
				oIdx := obsIdx
				return &Error{
					Code:                     "policy/taxonomy-violation",
					Message:                  "La observación contiene una ruta absoluta de sistema de archivos. Las rutas absolutas no son portables. Usa el nombre del repositorio o una ruta relativa en su lugar.",
					Layer:                    LayerTaxonomy,
					RejectedEntityIndex:      &eIdx,
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

// checkProjectNames validates that every entity with EntityType "project" has
// a name that is a plain kebab-case repository name (no slashes, backslashes,
// or colons). Per kg-content-policy.md the project name must be the bare repo
// name (e.g. "zippy-backoffice"), not a path.
func checkProjectNames(p Payload) *Error {
	for idx, entity := range p.Entities {
		if entity.EntityType != "project" {
			continue
		}
		if projectNameForbidden.MatchString(entity.Name) {
			eIdx := idx
			return &Error{
				Code:                "policy/taxonomy-violation",
				Message:             "El nombre de entidad tipo 'project' debe ser el nombre bare del repositorio en kebab-case (p. ej. 'zippy-backoffice'), sin rutas ni separadores de directorio.",
				Layer:               LayerTaxonomy,
				RejectedEntityIndex: &eIdx,
			}
		}
	}
	return nil
}

// joinEntityTypes returns a sorted, comma-separated list of allowed entity
// types for use in error messages. Sorted output ensures message stability
// across Go versions (map iteration order is intentionally random).
func joinEntityTypes() string {
	types := make([]string, 0, len(EntityTypes))
	for t := range EntityTypes {
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
