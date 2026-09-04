# AGENTS.md - Antigravity Account Switcher Agent Guide

## Project Mission & Overview
**Antigravity Account Switcher** is an in-process transparent proxy supervisor, multi-account pool manager, and live quota tracker designed for **Google Antigravity 2.0** and its command-line interface (`agy`).

When developers work intensively with Antigravity 2.0, requests to Google Cloud Code PA (`daily-cloudcode-pa.googleapis.com`) frequently encounter rate limits (`HTTP 429 RESOURCE_EXHAUSTED` or rolling 5-hour/weekly model limits) across Claude, GPT, and Gemini model tiers.

The switcher runs as a high-performance local supervisor that:
1. Intercepts outgoing requests to Google Cloud Code PA.
2. Buffers request bodies up to 100MB in memory.
3. Automatically rotates to the next available account in the pool upon receiving an HTTP 429 or quota-exhausted response.
4. Seamlessly replays the in-flight request in memory without interrupting agent thinking or breaking SSE token streaming.
5. Monitors account quota reset windows to automatically re-enable accounts once restored.

---

## Antigravity 2.0 Runtime Insights (Reverse Engineering)

Understanding the internal design of Google Antigravity 2.0 is crucial when diagnosing bugs or extending functionality:

- **Runtime Composition**: Google Antigravity 2.0 is an **Electron** desktop application bundled with a native background backend process named **`language_server` (compiled Go binary)**.
- **Local Language Server Discovery**:
  - The Electron frontend spawns `language_server` with the flag `--csrf_token <token>`.
  - The frontend communicates with `language_server` over localhost TCP sockets using HTTP/gRPC-Web endpoints (e.g. `/exa.language_server_pb.LanguageServerService/RetrieveUserQuotaSummary`).
  - All local requests require the header `x-codeium-csrf-token: <csrf_token>`.
  - The switcher discovers running `language_server` instances by inspecting `/proc/<pid>/cmdline`, socket inodes in `/proc/<pid>/fd`, and listening ports in `/proc/net/tcp` (see `internal/quota/language_server.go`).
- **Binary Resolution Priority**:
  When locating the Antigravity 2.0 binary to launch or supervise, inspect paths in this exact order:
  1. Saved configuration: `~/.config/antigravity-account-switcher/config.json` (`antigravity_bin` key).
  2. Environment variable: `ANTIGRAVITY_BIN`.
  3. User XDG directories: `~/.local/bin/antigravity`, `~/.local/share/antigravity/antigravity`.
  4. User tools directory: `~/tools/Antigravity/Antigravity-x64/antigravity`.
  5. System FHS paths: `/opt/antigravity/antigravity`, `/usr/local/bin/antigravity`.
  6. System `$PATH`: `antigravity`, `agy`.
- **Existing Credentials (Auto-Import)**:
  - Antigravity stores active Google OAuth credentials in `~/.gemini/antigravity-acp/acp_token.json` or `~/.gemini/antigravity-cli/acp_token.json`.
  - The switcher automatically imports these credentials if the account pool is empty on first boot (`internal/oauth/auto_import.go`).
- **Electron Auto-Updater (`APPIMAGE`)**:
  - On Linux, Antigravity 2.0 uses Electron's `AppImageUpdater`. When extracted from `.tar.gz` without an AppImage runtime, in-app `Help -> Check for Updates` fails with `ERR_UPDATER_OLD_FILE_NOT_FOUND` if `process.env.APPIMAGE` is missing.
  - The supervisor explicitly injects `APPIMAGE=<antigravity_bin_path>` into the child process environment to allow seamless updates.
- **Voice & Speech-to-Text (`speech.googleapis.com`)**:
  - Antigravity communicates with `speech.googleapis.com` for voice input.
  - To prevent interference, the supervisor injects `NO_PROXY=speech.googleapis.com` and provides raw bidirectional RFC 7231 TCP tunneling on HTTP `CONNECT` requests.

---

## Macro Architecture & Design Invariants

The codebase adheres strictly to **Clean Architecture / Hexagonal Principles**:

```
                      +-----------------------------+
                      |       Antigravity 2.0       |
                      |  (Electron + Go LangServer) |
                      +--------------+--------------+
                                     | HTTP_PROXY & CLOUD_CODE_URL
                                     v
+-------------------------------------------------------------------------+
|                  ANTIGRAVITY ACCOUNT SWITCHER (Single Binary)           |
|                                                                         |
|  [Launcher / Supervisor]       [In-Process Proxy & Failover]            |
|  * Linux PR_SET_PDEATHSIG      * 100MB Replay Request Buffer            |
|  * Scoped Environment Vars     * Dynamic Active Bearer Injection        |
|  * Subprocess Lifecycle        * Instant 429 Failover & Replay          |
|                                * SSE Stream Interceptor & Metrics       |
|                                * Transparent RFC 7231 CONNECT Tunnel   |
|                                                                         |
|  [Quota & Introspection]       [OAuth Engine]                           |
|  * Cloud Code PA Poller        * RFC 8252 Loopback Server               |
|  * /proc LanguageServer Probe  * ACP Token Auto-Import                  |
|                                                                         |
|  [Storage Engine]              [Web Dashboard]                          |
|  * Pure-Go SQLite WAL Mode     * Embedded Web GUI & SSE Realtime Stream |
|  * accounts.db (Zero CGO)      * Telemetry & Account Management         |
+------------------------------------+------------------------------------+
                                     |
                                     v
                  +--------------------------------------+
                  |   Google Cloud Code PA Upstream      |
                  |  (daily-cloudcode-pa.googleapis.com) |
                  +--------------------------------------+
```

### Key Architectural Invariants
1. **Zero CGO at Runtime (Pure Go Static Binary)**:
   - Release binaries are compiled with `CGO_ENABLED=0`.
   - SQLite is handled using the pure-Go driver `modernc.org/sqlite`. Never introduce Cgo or external C libraries into runtime code.
2. **Layer Separation**:
   - `internal/domain/`: Enterprise business rules, core entities (`Account`, `QuotaBucket`, `TokenMetric`, `ProxyEvent`), and interfaces (repository and service contracts). No external dependencies.
   - `internal/store/sqlite/`: Thread-safe persistence adapter in WAL mode (`journal_mode=WAL`, `busy_timeout=5000`).
   - `internal/proxy/`: In-memory reverse and forward proxy, 100MB request buffer (`NewBufferedRequestWithLimit`), dynamic Bearer token replacement, failover engine, and SSE stream parsing.
   - `internal/launcher/`: Process supervisor. Couples process lifecycles via Linux `PR_SET_PDEATHSIG` (if switcher dies, child dies; if child exits, switcher terminates cleanly).
   - `internal/quota/`: Background daemon querying Google Cloud Code PA API endpoints and probing local `language_server` sockets.
   - `internal/oauth/`: RFC 8252 loopback OAuth2 server for account onboarding.
   - `internal/web/`: HTTP server embedding frontend assets (`embed.FS`) and providing SSE event feeds.

---

## Tooling & Verification Commands

Always run these standard commands to validate changes:

```bash
# 1. Format code according to Go standard style
make fmt          # Runs: gofmt -s -w .

# 2. Static analysis and linting (must report 0 errors)
make lint         # Runs: go vet ./... and golangci-lint run ./...

# 3. Unit and integration tests with data race detector (must pass with 0 races)
make test-race    # Runs: go test -v -race -timeout=300s ./...

# 4. Pure Go static compilation check (validates zero CGO requirement)
make build-static # Compiles with CGO_ENABLED=0
```

> **Note on Testing**: `make test-race` uses `-race` (TSan), which requires a C compiler (GCC) during test runs only. Production compilation uses `make build` or `make build-static` (`CGO_ENABLED=0`).

---

## GitHub Workflow & Pull Request Rules

The repository enforces strict continuous integration via GitHub Actions (`.github/workflows/ci.yml` and `.github/workflows/release.yml`).

### Branching & Commit Conventions
- Branch naming: `feat/<name>`, `fix/<name>`, `docs/<name>`, `test/<name>`, `refactor/<name>`.
- Commit messages: Follow [Conventional Commits](https://www.conventionalcommits.org/):
  - `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:`.
  - Example: `fix(proxy): preserve Content-Length header on replayed requests`

### Pre-PR Checklist
Before opening a Pull Request or pushing code:
- [ ] Code is formatted with `make fmt`.
- [ ] Linters pass with zero warnings via `make lint`.
- [ ] All tests pass without data races via `make test-race`.
- [ ] Static binary compiles cleanly via `make build-static`.
- [ ] Fill out all sections of `.github/pull_request_template.md`.
- [ ] Ensure documentation is kept synchronized (`README.md` and `README.pt-BR.md` for user-facing changes).
