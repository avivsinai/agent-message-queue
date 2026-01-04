# Security Policy

## Security Model

AMQ is designed for **local inter-process communication** on a single machine. It assumes all agents operate under the same user account and share filesystem access.

### Threat Model

AMQ protects against:
- **Partial writes**: Maildir atomic delivery prevents corrupt messages from appearing in inboxes
- **Path traversal**: Agent handles and message IDs are strictly validated to prevent directory escape
- **Permission leakage**: Directories use 0700, files use 0600 (owner-only access)
- **Log injection**: User input is never interpolated into format strings

AMQ does **not** protect against:
- **Malicious agents with same-user access**: If an attacker has shell access as the same user, they can read/write queue files directly
- **Multi-user scenarios**: AMQ is not designed for use across user accounts

### Known Risks

#### TIOCSTI Terminal Injection (`amq wake`)

The `amq wake` command uses TIOCSTI (terminal input character stuffing) to inject notification text into the terminal. This is an **experimental feature** with inherent security considerations:

- TIOCSTI allows a process to inject input characters as if they were typed
- On some systems (hardened Linux kernels), TIOCSTI is disabled for security reasons
- The injected text is user-controlled notification content, not arbitrary commands
- `amq wake` only operates on terminals it owns (verified via session ID check)

If you're concerned about TIOCSTI, use the notify hook fallback instead:
```toml
# ~/.codex/config.toml
notify = ["python3", "/path/to/scripts/codex-amq-notify.py"]
```

### File Permissions

AMQ enforces strict permissions:
- **Directories**: 0700 (owner read/write/execute only)
- **Files**: 0600 (owner read/write only)
- **Handles**: Validated as `[a-z0-9_-]+` (no path separators)
- **Message IDs**: Cannot start with `.`, cannot contain path separators

## Reporting a Vulnerability

Please report security issues by opening a GitHub Security Advisory for this repository. If that is not available, open a regular issue and label it `security`.

We will acknowledge receipt as soon as possible and work to provide a fix or mitigation.

## Security Updates

- **2026-01-04**: Fixed AppleScript injection in `codex-amq-notify.py` (message titles with quotes could break notification script)
- **2026-01-04**: Fixed `read` command to parse before moving to `cur` (prevents stuck corrupt messages)
- **2026-01-04**: Fixed `setup-coop.sh` to avoid config overwrite when `jq` unavailable
