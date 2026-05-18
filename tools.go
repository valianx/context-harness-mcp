//go:build tools

// Package main declares build-time tool and future library dependencies so
// that `go mod tidy` pins their versions in go.sum. These imports are never
// compiled into the server binary (the build constraint excludes them). They
// are removed individually as each PR introduces the real import sites:
//   - pgx/v5, pgvector-go → PR-2 (store layer + migrations)
//   - validator/v10       → PR-3 (Content Filter)
//   - goose/v3            → PR-2 (migration runner) / Dockerfile goose binary
package main

import (
	// Postgres driver + pgvector type binding (PR-2)
	_ "github.com/jackc/pgx/v5"
	_ "github.com/pgvector/pgvector-go"
	// Struct validation (PR-3)
	_ "github.com/go-playground/validator/v10"
	// Migration CLI (PR-2 / Dockerfile goose binary)
	_ "github.com/pressly/goose/v3"
)
