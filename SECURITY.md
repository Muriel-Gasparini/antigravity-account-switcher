# Security Policy

## Reporting a Vulnerability

We take the security of Antigravity Account Switcher seriously. If you believe you have found a security vulnerability, please report it responsibly:

- **Preferred:** Open a private vulnerability report via [GitHub Security Advisories](https://github.com/Muriel-Gasparini/antigravity-account-switcher/security/advisories/new).
- **Direct Email:** Contact Muriel Gasparini at `muriel.developer@gmail.com` with the subject `[SECURITY] Antigravity Account Switcher`.

Please include:
1. Description of the vulnerability and potential impact.
2. Step-by-step reproduction steps or proof-of-concept.
3. Relevant environment details (OS, Go version, Antigravity IDE version).

We will acknowledge your report within 48 hours and work with you to patch and disclose the issue responsibly.

---

## OAuth 2.0 & Native Application Client Credentials

### RFC 8252 §8.5 Compliance (Public Clients)

Antigravity Account Switcher communicates with Google APIs to manage account authentication and quota tracking. Under the Internet Engineering Task Force (IETF) specification **[RFC 8252 (OAuth 2.0 for Native Apps)](https://datatracker.ietf.org/doc/html/rfc8252#section-8.5)**:

> *"A native app client is considered a public client... unable to maintain the confidentiality of its credentials. Authorization servers MUST NOT treat native app client secrets as confidential."*

The Google OAuth client credentials dynamically discovered or used by this application identify the client application as **Google Cloud Code / Antigravity Desktop**. They do not grant administrative rights or access to private resources without explicit user consent via Google's consent screen.

### Custom Credentials & Enterprise Overrides

If you or your organization prefer to use your own Google Cloud Console OAuth 2.0 Client Credentials, you can override them via environment variables:

```bash
export ANTIGRAVITY_CLIENT_ID="your-client-id.apps.googleusercontent.com"
export ANTIGRAVITY_CLIENT_SECRET="your-client-secret"
```

### Local Storage and Privacy

- **Zero Telemetry Leakage:** All access tokens, refresh tokens, and token metrics are stored strictly on your local filesystem in an encrypted or restricted-permission SQLite database (`~/.config/antigravity-account-switcher/accounts.db`).
- **No Cloud Telemetry:** This application does not send telemetry, tokens, or personal identifiers to any third-party servers. All proxy traffic flows exclusively between your local Antigravity IDE and official Google Cloud endpoints (`daily-cloudcode-pa.googleapis.com` and `accounts.google.com`).
