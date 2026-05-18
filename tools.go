//go:build tools

// Package main declares build-time tool dependencies so that `go mod tidy`
// pins their versions in go.sum. These imports are never compiled into the
// server binary (the build constraint excludes them).
//
// Removed as each PR introduces the real import sites:
//   - pgx/v5, pgvector-go → removed in PR-2 (imported by internal/store/pool.go)
//   - validator/v10       → removed in PR-3 (unused; gitleaks covers the validation need)
//   - gitleaks/v8         → imported for real by internal/validate/secrets.go (PR-3)
//   - goose/v3            → PR-2 migration runner / Dockerfile goose binary
package main

import (
	// Migration CLI (PR-2 / Dockerfile goose binary)
	_ "github.com/pressly/goose/v3"
)
