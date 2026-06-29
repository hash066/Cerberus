# Repository Guidelines

## Project Structure & Module Organization
Cerberus is a mixed Go and Rust workspace. Go binaries live in `cmd/`, daemon/control-plane code in `daemon/`, and shared Go contract types in `contract/go/`. Rust crates live under `core/` and `contract/rust/`, with the workspace defined by root `Cargo.toml`. Shared interface assets are in `proto/`, `components/wit/`, and `schemas/`. Integration and E2E harnesses live in `test/`; architecture and lane docs live in `docs/`, `ARCHITECTURE.md`, and `CONTRACT.md`.

## Agent-Specific Instructions
Read `CLAUDE.md` and `CONTRACT.md` before changing code. This checkout is for the Platform lane on branch `ws/platform`. Owned directories are `proto/`, `components/`, `build/`, `.github/`, and `test/`. You may write generated outputs under `contract/*/gen`, but never hand-edit contract source files. Contract changes are cross-lane events and must follow `CONTRACT.md`.

## Build, Test, and Development Commands
Prefer Taskfile targets when available:

- `task build` builds Go and Rust (`go build ./...` and `cargo build`).
- `task test` runs Go and Rust tests (`go test ./...` and `cargo test`).
- `task lint` runs `golangci-lint` and `cargo clippy` best-effort.
- `task demo` runs the v0.1 E2E demo with `go run ./test/e2e`.

Platform verification should include `go build ./...`, `go test ./...`, `cargo build --workspace`, `cargo test --workspace`, and `go run ./test/e2e`.

## Coding Style & Naming Conventions
Follow `.editorconfig`: UTF-8, LF endings, final newline, trim trailing whitespace, 4-space default indentation, tabs for Go, and 2 spaces for Markdown/YAML/JSON/Proto/WIT. Use `gofmt` for Go and `cargo fmt` for Rust. Keep Go package names short and lowercase; use Rust `snake_case` modules and tests.

## Testing Guidelines
Use Go’s standard testing framework with `*_test.go` files, as in `contract/go/contract_test.go`. Rust tests should use standard `#[test]` modules or integration tests where appropriate. Add focused tests for owned Platform changes and keep E2E behavior in `test/e2e` reproducible without manual setup.

## Commit & Pull Request Guidelines
Recent history mixes descriptive phase commits with informal messages; use descriptive, imperative commits such as `Add platform E2E harness` or `Regenerate proto bindings`. Pull requests should summarize scope, list verification commands run, link issues when applicable, and call out any contract-impacting changes for cross-lane review.

## Parallel Work
Each task should be independent and safe to run in parallel. Stay within owned directories, stub dependencies from other lanes, and avoid touching shared contract source unless explicitly performing an approved contract update.
