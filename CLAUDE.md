# Claude Code Guide - Antigravity Account Switcher

Refer to [AGENTS.md](AGENTS.md) for the authoritative macro architecture, reverse-engineering notes on Antigravity 2.0 (Electron + Go language server), and full project invariants.

## Essential Commands

```bash
# Code formatting
make fmt

# Linting & static analysis (golangci-lint + go vet)
make lint

# Test suite with Go race detector
make test-race

# Unit tests only (quick)
make test

# Build static release binary (CGO_ENABLED=0)
make build-static
```

## Architecture & Code Guidelines
- **Zero CGO**: Production code must compile with `CGO_ENABLED=0` (uses pure Go `modernc.org/sqlite`).
- **Clean Architecture**: Never import storage or transport layers into `internal/domain`.
- **Concurrency & Races**: Always verify concurrent code with `make test-race`. Zero data races tolerated.
- **Git Conventions**: Use Conventional Commits (`feat:`, `fix:`, `refactor:`, `test:`, `docs:`, `chore:`).
