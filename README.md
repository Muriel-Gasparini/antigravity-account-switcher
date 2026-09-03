# Antigravity Account Switcher

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go)](https://go.dev/)
[![CI Status](https://github.com/Muriel-Gasparini/antigravity-account-switcher/actions/workflows/ci.yml/badge.svg)](https://github.com/Muriel-Gasparini/antigravity-account-switcher/actions)

Automatic Multi-Account Pool Management, Real-Time Quota Tracking, and Seamless HTTP 429 Failover for **Google Antigravity 2.0** and CLI (`agy`).

---

## What is Antigravity 2.0 and why is this needed?

**Google Antigravity 2.0** is Google DeepMind's standalone AI-first development application (distinct from the legacy preview VS Code extension).

When working intensely with Antigravity 2.0, developers frequently hit rate limits (`HTTP 429 RESOURCE_EXHAUSTED` or rolling 5-hour limits) across Claude, GPT, and Gemini model tiers.

**Antigravity Account Switcher** runs as a transparent, high-performance local supervisor that intercepts Antigravity 2.0 requests. When your active account exhausts its quota, the switcher **seamlessly rotates to the next available account in your pool and replays the in-flight request in memory** — without interrupting your agent's thinking, breaking generation streams, or causing errors in the editor.

---

## Installing Google Antigravity 2.0 on Linux

> [!IMPORTANT]
> **Google Antigravity 2.0 is NOT distributed via package managers (`apt`, `snap`, `dnf`, `pacman`, or `flatpak`).**  
> Google provides it directly as a standalone `.tar.gz` archive containing the bundled binary and runtime libraries.

On Linux, software distributed this way should be placed in either the **XDG User Directory** (recommended, no root required) or the **System `/opt` Directory** (requires root).

### Option 1: User Installation (Recommended — No `sudo` required)

This follows the **Linux XDG Base Directory Specification** (`~/.local/share/` and `~/.local/bin/`). It does not require administrator privileges and will not affect other users or system files:

```bash
# 1. Create the application directory
mkdir -p ~/.local/share/antigravity

# 2. Extract the downloaded Google archive
tar -xzf ~/Downloads/Antigravity-linux-x64.tar.gz -C ~/.local/share/antigravity --strip-components=1

# 3. Ensure the binary has execution permissions
chmod +x ~/.local/share/antigravity/antigravity

# 4. Create a symlink in ~/.local/bin (in your PATH on modern Linux distros)
mkdir -p ~/.local/bin
ln -sf ~/.local/share/antigravity/antigravity ~/.local/bin/antigravity
```

### Option 2: System-Wide Installation (Requires `sudo` / FHS Standard)

This follows the **Filesystem Hierarchy Standard (FHS)** for add-on third-party software in `/opt`:

```bash
# 1. Create system directory
sudo mkdir -p /opt/antigravity

# 2. Extract the archive
sudo tar -xzf ~/Downloads/Antigravity-linux-x64.tar.gz -C /opt/antigravity --strip-components=1

# 3. Create global symlink
sudo ln -sf /opt/antigravity/antigravity /usr/local/bin/antigravity
```

---

## How the Switcher Connects to Antigravity 2.0

### Automatic Detection (Zero Configuration)
If you followed either of the standard installation methods above, **you do not need to configure any paths**.  
When you run `antigravity-account-switcher launch`, the switcher automatically discovers your binary by checking standard Linux locations:
1. `~/.local/bin/antigravity`
2. `~/.local/share/antigravity/antigravity`
3. `/usr/local/bin/antigravity`
4. `/opt/antigravity/antigravity`
5. `~/tools/Antigravity/Antigravity-x64/antigravity` *(user tools folder fallback)*
6. Any `antigravity` or `agy` command available in your system `$PATH`

### Custom Path Configuration
If you unpacked Antigravity 2.0 into a custom folder, you can explicitly configure the binary location using any of the following methods:

**Method 1: Permanent setting via CLI (Recommended)**
```bash
antigravity-account-switcher config set antigravity_bin /path/to/your/antigravity
```

**Method 2: Environment Variable**
```bash
export ANTIGRAVITY_BIN="/path/to/your/antigravity"
```

**Method 3: Per-launch flag**
```bash
antigravity-account-switcher launch --bin /path/to/your/antigravity
```

---

## Quick Start (3 Steps)

### 1. Build and Install the Switcher
```bash
git clone https://github.com/Muriel-Gasparini/antigravity-account-switcher.git
cd antigravity-account-switcher
make install
```
*Compiles a single static binary with CGO_ENABLED=0 and installs it to `~/.local/bin/antigravity-account-switcher`.*

### 2. Onboard Your Accounts
Authenticate one or more Google accounts:
```bash
antigravity-account-switcher add-account
```
*(If you already have Antigravity 2.0 installed and logged in, the switcher automatically detects and imports your active login on first launch!)*

### 3. Launch Antigravity 2.0
```bash
antigravity-account-switcher launch
```
The switcher starts the background proxy, launches Antigravity 2.0 with scoped proxy variables, and monitors health. When you close Antigravity 2.0, the switcher shuts down automatically.

---

## Desktop Integration (GNOME / KDE / XFCE)

To launch Antigravity 2.0 directly from your application launcher or dock with multi-account supervision:

```bash
antigravity-account-switcher install-desktop
```
This automatically:
- Resolves your Antigravity 2.0 executable.
- Extracts and installs the official application icon to `~/.local/share/icons/antigravity.png`.
- Creates `~/.local/share/applications/antigravity.desktop` pointing to `antigravity-account-switcher launch %F`.

To remove the desktop integration:
```bash
antigravity-account-switcher uninstall-desktop
```

---

## Commands Reference

The CLI provides commands for launch supervision, manual switching, and configuration:

| Command | Description |
| :--- | :--- |
| `launch` | **(Recommended)** Launches Antigravity 2.0 under proxy supervision. |
| `serve` | Runs the proxy, background quota monitor, and web dashboard as a daemon. |
| `wrap -- <cmd>` | Executes any arbitrary command with scoped switcher proxy variables. |
| `add-account` | Initiates RFC 8252 loopback OAuth2 flow to register a Google account. |
| `list-accounts` | Displays all registered accounts, active status, and quota percentages. |
| `refresh-quotas` | Forces an immediate live quota sync from Google for all registered accounts. |
| `status` | Shows the currently active account, token totals, and switcher health. |
| `config` | Inspects or updates persistent settings (`get`, `set`, `list`). |
| `install-desktop` | Installs GNOME / XDG `.desktop` launcher shortcut and application icon. |
| `uninstall-desktop`| Removes GNOME / XDG `.desktop` launcher shortcut. |
| `version` | Displays binary version, commit hash, and build timestamp. |

### Useful Command Flags

- **Open Web Dashboard on Launch:**
  ```bash
  antigravity-account-switcher launch --open
  ```
- **Headless / Remote SSH Account Addition:**
  ```bash
  antigravity-account-switcher add-account --no-browser
  ```
- **Specify Custom Port:**
  ```bash
  antigravity-account-switcher launch --port 1831
  ```

---

## Configuration Reference

Settings are stored in JSON format at `~/.config/antigravity-account-switcher/config.json`.

```bash
# View all current settings
antigravity-account-switcher config list

# Set path to Antigravity 2.0 executable
antigravity-account-switcher config set antigravity_bin ~/.local/share/antigravity/antigravity

# Set default web dashboard port
antigravity-account-switcher config set port 1831

# Adjust background quota check interval
antigravity-account-switcher config set quota_interval 60s
```

### Environment Variables

| Variable | Description |
| :--- | :--- |
| `ANTIGRAVITY_BIN` | Explicit path to the Antigravity 2.0 executable. |
| `ANTIGRAVITY_PORT` | Overrides the switcher listening port. |
| `ANTIGRAVITY_DB_PATH` | Path to the SQLite database file (default: `~/.config/.../accounts.db`). |
| `ANTIGRAVITY_CLIENT_ID` | Optional custom Google Cloud Console OAuth Client ID override. |
| `ANTIGRAVITY_CLIENT_SECRET` | Optional custom Google Cloud Console OAuth Client Secret override. |

---

## Architecture

```text
               +-----------------------------------+
               |          Antigravity 2.0          |
               |     (Child Process via Supervisor)|
               +-----------------+-----------------+
                                 | HTTP_PROXY / CLOUD_CODE_URL
                                 v
+------------------------------------------------------------------+
|              ANTIGRAVITY ACCOUNT SWITCHER (Single Binary)        |
|                                                                  |
|   +--------------------+     +-------------------------------+   |
|   |   Web Dashboard    |     |      In-Process Reverse Proxy |   |
|   |  (HTML5/Tailwind)  |     | * Dynamic Bearer Token        |   |
|   |  http://127.0.0.1  |     | * 100MB Replay Buffer         |   |
|   +---------+----------+     | * RFC 7231 CONNECT Tunnel     |   |
|             |                +---------------+---------------+   |
|             v                                |                   |
|   +--------------------+         HTTP 429    | SSE Tokens        |
|   | SQLite WAL Store   |<--------------------+                   |
|   | * accounts.db      |                     v                   |
|   | * token telemetry  |     +-------------------------------+   |
|   +---------+----------+     | Quota Poller Daemon           |   |
|             ^                | * Auto-restore past reset     |   |
|             +----------------+ * Official Google PA User-Agent|   |
+----------------------------------------------+-------------------+
                                               |
                                               v
                              +--------------------------------+
                              | Google Cloud Code Infrastructure|
                              +--------------------------------+
```

---

## Troubleshooting & FAQ

#### 1. "Could not automatically locate Antigravity binary"
If your Antigravity 2.0 installation is in a custom location, specify the path to the executable:
```bash
antigravity-account-switcher config set antigravity_bin /path/to/antigravity
```

#### 2. Does this interfere with native voice dictation (Speech-to-Text)?
No. The switcher automatically sets `NO_PROXY=speech.googleapis.com` and provides raw RFC 7231 TCP tunneling on `CONNECT` requests, ensuring audio streaming functions with zero interference.

#### 3. Where are my tokens and credentials stored?
Tokens are stored strictly on your local filesystem in SQLite (`~/.config/antigravity-account-switcher/accounts.db`). No credentials or telemetry ever leave your machine.

#### 4. How do I completely uninstall and remove all data?
```bash
# 1. Remove desktop entry
antigravity-account-switcher uninstall-desktop

# 2. Remove binary
make uninstall

# 3. Purge configuration and database
rm -rf ~/.config/antigravity-account-switcher
```

#### 5. Does the in-app auto-updater ("Check for Updates") work?
Yes! On Linux, Antigravity 2.0 uses Electron's `AppImageUpdater`. When Antigravity is run from an extracted `.tar.gz` without an AppImage runtime, `Help -> Check for Updates` normally fails with `ERR_UPDATER_OLD_FILE_NOT_FOUND` because the `APPIMAGE` environment variable is missing.

**Antigravity Account Switcher fixes this automatically.** When launched via `antigravity-account-switcher launch` (or from the `.desktop` launcher created by `install-desktop`), the supervisor automatically injects the exact `APPIMAGE` binary path into the Antigravity 2.0 process, allowing automatic background and manual in-app updates to work seamlessly out-of-the-box.


---

## Security

Please see [SECURITY.md](SECURITY.md) for vulnerability disclosure procedures and details regarding RFC 8252 §8.5 OAuth 2.0 public client credentials.

---

## Contributing

Contributions are welcome! Please read [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, testing with the Go data race detector, and PR guidelines.

---

## License

MIT License © 2026 Muriel Gasparini. See [LICENSE](LICENSE) for details.
