# Contributing to Antigravity Account Switcher

[English](CONTRIBUTING.md) | [Português (Brasil)](CONTRIBUTING.pt-BR.md)

Thank you for your interest in contributing to **Antigravity Account Switcher**!

We welcome bug reports, improvements, documentation updates, and feature requests. Please follow these guidelines to ensure a smooth contribution process.

---

## Development Prerequisites

- **Go:** Version 1.24+ ([Download Go](https://go.dev/dl/))
- **Git:** Standard git client
- **Make:** GNU Make (optional, but recommended)
- **GCC / C Toolchain:** Required only when running tests with the Go race detector (`-race`). The release binary compiles with `CGO_ENABLED=0` (pure-Go SQLite via `modernc.org/sqlite`).
- **golangci-lint:** Version 1.60+ ([Installation Guide](https://golangci-lint.run/welcome/install/))

---

## Getting Started

1. **Fork and Clone:**
   ```bash
   git clone https://github.com/Muriel-Gasparini/antigravity-account-switcher.git
   cd antigravity-account-switcher
   ```

2. **Verify Environment:**
   Run the test suite to ensure your environment is configured properly:
   ```bash
   make test
   ```

3. **Build the Static Binary:**
   ```bash
   make build
   ./bin/antigravity-account-switcher version
   ```

---

## Running Tests & Linters

Every Pull Request is validated automatically by GitHub Actions. Before submitting code, run the following locally:

```bash
# 1. Run unit and integration tests with the data race detector
make test-race

# 2. Run static analysis and linting
make lint

# 3. Format Go code according to standard style
make fmt
```

All tests must pass with `0` data races, and `golangci-lint` must complete with zero warnings.

---

## Project Structure

The project follows Clean Architecture / Hexagonal principles:

```text
├── cmd/
│   └── antigravity-account-switcher/ # CLI commands and entry points
├── internal/
│   ├── config/                       # Persistent config.json and path discovery
│   ├── domain/                       # Core business entities, ports, and interfaces
│   ├── launcher/                     # Process supervisor, PR_SET_PDEATHSIG, and desktop installer
│   ├── metrics/                      # Token usage calculations and aggregations
│   ├── oauth/                        # RFC 8252 OAuth2 loopback server and token management
│   ├── proxy/                        # Reverse proxy, 429 failover engine, and SSE token interceptor
│   ├── quota/                        # Live quota monitor daemon and language_server discovery
│   ├── store/sqlite/                 # Thread-safe SQLite store in WAL mode (pure Go)
│   └── web/                          # Embedded web dashboard (HTML5, Tailwind CSS, Vanilla JS)
├── scripts/                          # Desktop installation and automation scripts
└── test/                             # Mocks and E2E integration test suites
```

---

## Conventional Commits

We follow the [Conventional Commits](https://www.conventionalcommits.org/) specification:

- `feat:` A new feature or capability
- `fix:` A bug fix
- `docs:` Documentation updates
- `refactor:` Code refactoring without changing observable behavior
- `test:` Adding or improving tests
- `chore:` Build scripts, dependencies, or toolchain updates

Example:
```bash
git commit -m "fix(proxy): handle RFC 7231 compliance in CONNECT tunnels"
```

---

## Submitting a Pull Request

1. Create a descriptive feature branch:
   ```bash
   git checkout -b fix/proxy-audio-bypass
   ```
2. Make your changes and add corresponding automated unit/integration tests.
3. Ensure all tests and linters pass:
   ```bash
   make lint && make test-race
   ```
4. Push to your fork and open a Pull Request against `main`.
5. Clearly describe the problem, the solution, and how you tested the change.
