# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Go 1.23 module (`github.com/mariogutierrez/context-harness-mcp`) with direct
  dependencies declared: `mcp-go`, `pgx/v5`, `pgvector-go`, `validator/v10`, `goose/v3`.
- `cmd/server/main.go` — entry point with `-transport=stdio|http` and `-addr` flags,
  `log/slog` JSON structured logging.
- `internal/mcp/server.go` — `mcp-go` server factory; `RegisterHealthz` scaffold ready
  for PR-3/PR-4 to extend.
- `internal/healthz/healthz.go` — `healthz` MCP tool handler returning
  `{"status":"ok","db":"not-configured"}` (DB wiring lands in PR-2).
- `Dockerfile` — multi-stage build: `golang:1.23` server builder, `golang:1.23` goose
  builder, `debian:bookworm-slim` runtime with ONNX shared library at `/usr/local/lib`
  and both `server` + `goose` at `/usr/local/bin`.
- `docker-compose.yml` — Phase 1 stub declaring `postgres`, `migrate`, `mcp` services
  with `depends_on` ordering (full wiring in PR-6).
- `render.yaml` — Render IaC stub: `env: docker`, `plan: free`, `healthCheckPath: /healthz`
  (full env vars and region in PR-7).
- `.env.example` — documented env vars with placeholder values; no secrets.
- `CLAUDE.md` — repo conventions: §1 Purpose, §2 Repo Map, §3 Tech Stack,
  §4 Golden Commands, §5 Architectural Conventions, §6 Mandatory Working Agreements.
- `docs/knowledge.md` — seeded with `[stack]`, `[decisión]`, `[restricción]`, and
  `[patrón]` bullets from the design phase.
- `.github/workflows/ci.yml` — CI stub on `pull_request`: setup-go, `go vet ./...`,
  `staticcheck ./...`, `go build ./...`.
