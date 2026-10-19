# Configuration

AMQ configuration has four scopes. Each scope has one home, and a setting
lives in the narrowest scope that owns it.

| Scope | Location | Examples |
| --- | --- | --- |
| Agent | `<root>/agents/<me>/` | `.wake.settings` |
| Root | `<root>/meta/` | `config.json` |
| Project | the project directory | `.amqrc`, `.amq/launch.json` |
| User and host | `~/.amq/` | `wake.settings`, `remote/binding.json`, `codex-threads/`, `state/` |

The user scope belongs to one user on one machine. It never travels with a
root: a root on shared storage meets each host's own user scope.

## Settings precedence

For a setting that has layers, the most specific layer that sets a key wins,
and each key resolves on its own:

built-in default, then `~/.amq/wake.settings`, then the agent's `.wake.settings`

A key that a layer does not set falls through to the next layer. An explicit
agent value pins its key, including a value equal to the default. See
[Wake operations](wake-operations.md#live-settings) for the wake keys and the
commands that edit each layer.

Root routing is a separate chain with its own eligibility rules, not a
per-key overlay. See [Session routing and safety](session-routing.md).

## Hot reload

A long-running AMQ process must re-read its configuration while it runs.
This is the project rule. The wake reloads the agent and machine wake
settings files, `amq-remote serve` reloads its relay shares, and `amq-acp`
reads `~/.amq/acp.settings` at the start of each turn. One process does not
yet follow the rule:
[issue 1022](https://github.com/avivsinai/agent-message-queue/issues/1022)
(the self-upgrade kill switch in `amq wake` and `amq-keepalive`).

- A change applies without a restart.
- A refused edit (bad syntax, unknown key, failed validation, failed trust
  check) keeps the last good value and is reported once, not on every check.
- Identity is the exception. A value bound into a lock, a target, or resume
  state is fixed for the life of the process: `--me`, `--root`, `--inject-*`,
  `--interrupt-cmd`, and `--retry-until`. Changing it live would let a running
  process disagree with the claim that other processes verify, so it needs a
  restart through the owning terminal or supervisor.

A start has no last good value. A start or resume that finds a refused user
file runs without that layer, so a refused file never blocks a start or a
self-upgrade. A self-upgrade under a refused machine file can therefore change
the effective settings. While the machine file is refused, a running wake keeps
its last good machine settings, and `amq wake config` shows the values without
the machine layer. See [Wake operations](wake-operations.md#live-settings).

## The per-user home

`~/.amq` is the one per-user directory. `internal/amqhome` is its only
resolver: it returns the path and creates the directory with mode 0700. It
refuses only a symlink or a non-directory, and it does not check mode or
owner. A feature takes the path from `amqhome` and never joins its own home
path.

The machine wake settings file, `~/.amq/wake.settings`, has stricter rules for
read and write:

- `~/.amq` is a real directory (no symlink), owned by the user, and not
  group-writable or world-writable.
- The file is a regular file (no symlink), owned by the user, mode 0600.
- A file or directory that fails a check is refused, not repaired.
- A missing `~/.amq/wake.settings` always means no machine layer, even when
  `~/.amq` fails the checks.

## Launch trust state

On a new machine, launch trust state lives in `~/.amq/state`. The legacy
platform state directory (`<legacy>` below) is `$XDG_STATE_HOME/amq`, else the
user config directory on macOS and Windows, else `~/.local/state/amq`. Older
`amq` binaries keep trust there.

- When `<legacy>/launch` exists, launch trust stays in the legacy directory.
  Older and newer `amq` binaries then read and write the same trust records.
- `XDG_STATE_HOME` only locates the legacy directory. It does not move
  `~/.amq/state`.
- AMQ does not move existing trust to `~/.amq/state`. See
  [issue 1047](https://github.com/avivsinai/agent-message-queue/issues/1047).
- Do not delete `<legacy>/launch` by hand. If `~/.amq/state/launch` also
  exists, its older trust records become active again without a new confirm.

## Keepalive home

On a new install, `amq-keepalive` keeps its registry, hook script, hook log,
and adapter state in `~/.amq/keepalive`. Older binaries use `~/.amq-keepalive`.

- When `~/.amq-keepalive` exists, keepalive continues to use it. Older and
  newer binaries then share one registry.
- When both `~/.amq-keepalive` and `~/.amq/keepalive` exist, every command
  that uses the default path refuses until you remove one. Normally an older
  binary recreated `~/.amq-keepalive`, and the error tells you to remove it.
  A supervisor started with `--registry` continues to use that path.
- A symlink or a non-directory at either path is refused.
- AMQ does not move an existing `~/.amq-keepalive`. See
  [issue 1047](https://github.com/avivsinai/agent-message-queue/issues/1047).

## Locations outside `~/.amq`

These locations predate the rule and stay where they are:

- Darwin wake restart stages live in `<legacy>/wake-stages`, also on a new
  machine. See
  [issue 1046](https://github.com/avivsinai/agent-message-queue/issues/1046).
