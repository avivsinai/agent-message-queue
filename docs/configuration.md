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
settings files, `amq-keepalive supervise` reloads its settings file,
`amq-remote serve` reloads its relay shares and its links when the manifest
bytes change (a deleted manifest means no links and no shares), and `amq-acp`
reads `~/.amq/acp.settings` at the start of each turn.

- A change applies without a restart.
- A refused edit (bad syntax, unknown key, failed validation, failed trust
  check) keeps the last good value and is reported once, not on every check.
- The self-upgrade kill switch (`self_upgrade`) in `amq wake` and
  `amq-keepalive supervise`: `false` stops new replacements live, and `true`
  enables them again in a process that started enabled. `true` cannot enable
  a process started with `--no-self-upgrade`, because enabling needs the
  baseline a process captures at start; restart it.
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

Launch trust state lives in `~/.amq/state/launch`. The legacy platform state
directory (`<legacy>` below) is `$XDG_STATE_HOME/amq`, else the user config
directory on macOS and Windows, else `~/.local/state/amq`.

- When `<legacy>/launch` exists, launch trust stays in the legacy directory,
  so every `amq` binary reads and writes the same trust records.
- `XDG_STATE_HOME` only locates the legacy directory. It does not move
  `~/.amq/state`.
- `amq doctor --migrate-home` moves `<legacy>/launch` to
  `~/.amq/state/launch` in one rename, so launches keep their trust. Use
  `--dry-run` to see the move first. First upgrade or remove every `amq`
  below the migration version floor (`migrateHomeVersionFloor` in
  `internal/cli/doctor_migrate_home.go`): a binary below it writes
  `<legacy>/launch` again. The command reads the version of each `amq` on
  `PATH` without running it, and refuses (exit 6) when one is below the floor
  or its version cannot be read. It also
  refuses when `~/.amq/state/launch` already exists, or when the two
  directories are on different filesystems. A refusal changes nothing.
- The command moves the `<legacy>` that its own `XDG_STATE_HOME` selects.
  Programs that embed the `launchapi` package cannot be checked. One built
  below the floor asks for trust again and recreates `<legacy>/launch`.
- Do not delete `<legacy>/launch` by hand. If `~/.amq/state/launch` also
  exists, its older trust records become active again without a new confirm.
- When both exist, `<legacy>/launch` is authoritative and
  `~/.amq/state/launch` is inert. Find the binary or program below the floor
  that wrote `<legacy>/launch`, remove
  `~/.amq/state/launch` (the only effect is a new trust prompt for the
  projects it holds), and run `amq doctor --migrate-home` again. Never
  remove `<legacy>/launch`.
- An `amq launch` that runs during the migration can recreate
  `<legacy>/launch`. The next `--migrate-home` then refuses with
  `both_exist`, and the safe fix is the same: remove `~/.amq/state/launch`
  (the cost is new trust prompts for the moved projects). To avoid this,
  run the migration while no launch runs.

## Keepalive home

`amq-keepalive` keeps its registry, hook script, hook log, and adapter state
in `~/.amq/keepalive`.

- When the legacy `~/.amq-keepalive` exists, keepalive uses it, so every
  binary shares one registry.
- When both `~/.amq-keepalive` and `~/.amq/keepalive` exist, every command
  that uses the default path refuses until you remove one. Usually a binary
  below the version floor recreated `~/.amq-keepalive`, and the error tells
  you to remove it.
  A supervisor started with `--registry` continues to use that path.
- A symlink or a non-directory at either path is refused.
- `amq doctor --migrate-home` moves `~/.amq-keepalive` to `~/.amq/keepalive`
  in one rename. Then it changes `--registry` in the supervisor plist,
  changes the AMQ SessionStart hooks in `~/.claude/settings.json` and
  `~/.codex/hooks.json` that run the moved script, and bootstraps the
  supervisor job again. It bootstraps the job only when it moves the home or
  rewrites the plist; otherwise it prints the command to start it. Registered
  agents and their wakes do not change.
  Use `--dry-run` to see the plan and every blocker first.
- Before you run it:
  1. Stop the supervisor job and keep its plist:
     `launchctl bootout gui/$(id -u)/io.github.avivsinai.amq-keepalive`.
     Do not run `amq-keepalive uninstall`, because it removes the plist.
  2. Stop the amq-remote login service
     (`launchctl bootout gui/$(id -u)/io.github.avivsinai.amq-remote`, or
     `systemctl --user stop amq-remote.service`), and every other
     `amq-keepalive` or `amq-remote` process.
  3. Upgrade or remove each binary that the dry run names.
- After it: start the amq-remote login service again
  (`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.github.avivsinai.amq-remote.plist`,
  or `systemctl --user start amq-remote.service`). On Linux, start your own
  supervisor again with no `--registry`, or with
  `--registry ~/.amq/keepalive/registry.json`.
- The command refuses (exit 6) while any `amq-keepalive` or `amq-remote`
  process runs, whatever its version: a running one may hold the legacy path
  and create it again. It also refuses while the supervisor job or the
  amq-remote login service is loaded, or while an `up` or inject lock is held.
  It refuses when an `amq`, `amq-keepalive` or `amq-remote` below the
  version floor can run: on `PATH`, in the plist (with the plist's `PATH`), in a hook, as a
  wake's injector, or as the amq-remote login service. It refuses when
  another launchd or systemd user unit names `~/.amq-keepalive`, and when the
  supervisor plist is a symlink or differs from what
  `amq-keepalive install-launchd` writes (the rewrite would drop the change).
  A refusal changes nothing.
- An interrupted run is finished by running the command again. If a process
  created `~/.amq-keepalive` again during the move, a rerun removes it when
  it holds only a registry with no rows and lock files that no process
  holds: `registry.json.lock`, `registry.json.registration.lock`,
  `registry.json.up-*.lock`, `.selfupgrade.lock` and
  `claude-print/*/inject.lock`. Otherwise the command refuses and names what
  it holds.

## Locations outside `~/.amq`

These locations stay outside the per-user home:

- Darwin wake restart stages are written to `<legacy>/wake-stages`, on every
  machine; a wake also accepts, validates, and prunes a stage under
  `~/.amq/state/wake-stages`
  ([issue 1046](https://github.com/avivsinai/agent-message-queue/issues/1046)).
