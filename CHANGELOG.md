# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]
### Fixed

- `amq wake` raw injection submits again in fast-reading TUIs (codex-tui, busy
  Claude Code): the v0.41.0 drain-wait completes within microseconds when the
  TUI is actively reading, so the submit CR landed inside the TUI's paste-burst
  window and was inserted as a pasted newline instead of pressing Enter. The
  injector now holds the CR for a 50ms settle delay after the text drains and
  restores the second rescue CR (a no-op when the first CR already submitted),
  skipping the rescue only when the first CR is provably still queued.

## [0.41.0] - 2026-07-08
### Added

- `amq reply --wait-for <stage> --wait-timeout <duration>` blocks on the
  recipient's delivery receipt, mirroring `amq send --wait-for`, so reply
  delivery can be confirmed instead of assumed.
- `amq who` text output now prints a `Base root:` header naming the tree it is
  reading, so per-root `active`/`stale` presence is not mistaken for global
  liveness when the same session name exists in another root (JSON output is
  unchanged).

### Fixed

- `amq wake` raw TIOCSTI injection now waits for the terminal input queue to
  drain after writing notification text, then injects a single carriage return,
  preventing Ghostty/Claude Code from intermittently receiving text and Enter
  in one paste-shaped stdin chunk (#208).
- `make lint` now uses a checkout-local golangci-lint cache, preventing stale
  analyzer results from deleted git worktrees from leaking into future lint runs
  and failing pre-push checks (#199).
- `amq coop exec` now pins the session root to an absolute path before
  starting wake and exporting `AM_ROOT`/`AM_BASE_ROOT`. A relative root
  (e.g. from the `.agent-mail` default) re-resolved against every future cwd
  of the agent process, silently splitting one session name into
  per-directory mailbox trees across git worktrees — peers on the "same"
  session could not see each other and sends queued where nobody was reading.
- `amq env` shell output (plain and `--export`) now emits absolute
  `AM_ROOT`/`AM_BASE_ROOT` values for the same reason: these exports exist to
  pin a terminal to one mailbox, and a relative export re-splits per cwd
  (JSON output is unchanged).


## [0.40.0] - 2026-07-05
### Added

- `amq coop init --no-gitignore` now leaves `.gitignore` unchanged, for users
  who manage ignore rules globally or manually (#173, closes #172).
- `amq coop exec --no-gitignore` passes the gitignore opt-out through auto-init,
  so `coop exec` can start a session without modifying `.gitignore` (#192,
  closes #179).
- Contributor workflow: pull requests now require a `CHANGELOG.md` Unreleased
  entry unless skipped by Dependabot, release branches, or the `no-changelog`,
  `docs`, or `chore` labels (#182).

### Changed

- `amq wake --help` no longer lists internal readiness-coordination flags;
  managed wake startup flows can still pass them (#189).
- Bump `github.com/coder/websocket` from 1.8.14 to 1.8.15 (#169)
- Bump `actions/checkout` from 6.0.3 to 7.0.0 (#170)

### Fixed

- Hardened message, DLQ-envelope, and receipt parsing by rejecting queue files
  that are themselves symlinks or non-regular files before reading them (#186).
- Inbox and DLQ operations now reject malformed or non-canonical `.md` queue
  filenames at queue boundaries (#185).
- Wake metadata and readiness writes now refuse symlinked destination files and
  install atomically with fsync (#187).
- Wake identity mismatches now stay `unverified` unless AMQ can prove the
  recorded PID is not an `amq wake` process (#187).
- `amq wake --inject-via` now accepts symlinked executables, such as
  Homebrew-installed injectors, by resolving them before validation; validation,
  persistence, and execution all use the resolved physical path (closes #197).
- On Windows, atomic file writes now replace existing files atomically instead
  of temporarily deleting the destination during rename retries (#188).
- Atomic queue-file writes now fail with `io.ErrShortWrite` if the filesystem
  reports success after writing only part of the file (#184).
- Contributor workflow: pre-push smoke tests no longer write synthetic release
  refs into the caller repository when run from a git hook (#196, closes #195).
- Test-only: wake tests now create their sandboxes under the physical
  repository path, so symlink-spelled checkouts no longer fail loop/inject-via
  assertions (#194, closes #193).
- Test-only: made inject-via wake notification tests deterministic by replacing
  shell-redirection capture with a helper process (#183).

### Compatibility

The stricter queue validation above applies to queue files themselves, not to
how their directories are reached. A message, receipt, DLQ envelope, or wake
metadata file that is itself a symlink (or any non-regular file) is now
rejected; queue roots reached through symlinked parent directories — for
example a symlinked checkout or home layout — are unaffected. Files created by
the AMQ CLI are always regular files, so only hand-placed symlinks inside queue
directories are impacted.


## [0.39.0] - 2026-06-30
### Fixed

- `amq drain` and drain-mode `amq monitor` now claim inbox messages before
  parsing them, preventing duplicate consumption under concurrent drains.
- DLQ moves and retries now claim or update queue state before redelivery, and
  reject tampered original filenames before restoring messages.
- Multi-recipient delivery now preserves already committed inbox messages when
  a later recipient fails, and `amq send --project` now rejects multiple
  recipients instead of applying undefined cross-project partial semantics.
- AMQ-owned `--inject-via` wake processes started by `coop exec` or
  `wake repair` now exit when their recorded owner process is gone or no
  longer matches, preventing stale terminal injectors from blocking session
  reopen recovery.


## [0.38.0] - 2026-06-22
### Added

- `amq env --export` now prints eval-safe shell exports for opt-in terminal
  pinning, including `AM_BASE_ROOT` only when the resolved root is a session
  root (#149).

### Fixed

- `amq who`, `amq presence list`, and `amq doctor --ops` now present the
  reserved `user` mailbox as a human operator gate instead of a stale agent
  process (#139).
- The human operator handle `user` is now reserved for configured projects, and
  `amq coop init` seeds `claude,codex,user` by default so strict operator gates
  no longer require custom coop setup (#139).
- Release publishing now detects release commits inside normally merged release
  PRs while ordinary feature merge commits no-op before tag, artifact, or skill
  publishing jobs (#163).


## [0.37.1] - 2026-06-22

### Fixed

- Hardened general stale `.wake.lock` cleanup paths so `amq wake` acquisition
  and `doctor --ops --fix-wake-locks` refuse to remove locks whose live wake PID
  only appears stale because boot-id or process-start metadata was tampered or
  mismatched (#156).
- `amq send --root <path>` now shows the root basename as the session label for
  root-only local sends that are not routed by project, session, or
  from-session flags (#150).

## [0.37.0] - 2026-06-22
### Added

- Document operator-gate conventions in the `amq-cli` and `amq-spec` skills,
  covering structural human handoffs, initialized human handles, and spec
  approval gates (#136).
- Report and optionally fix identity-verified stale `.wake.lock` files from
  `amq doctor --ops`, including roots whose config is missing or corrupt (#151).
- Add `amq wake repair` plus `coop exec --wake-inject-via` support so managed
  launchers can restart a dead external-injector wake for a still-running agent
  session from a digest-bound, private saved target (#154).

### Fixed

- `amq coop exec --require-wake` can reuse an existing usable wake process, while
  still failing closed when the existing wake cannot safely inject (#153).


## [0.36.0] - 2026-06-13
### Changed

- `amq send` now refuses an explicit `--root` that targets a different base tree
  than the caller's active session (`AM_ROOT`/`AM_BASE_ROOT`) when no routing
  dimension (`--project`/`--session`/`--from-session`) is given. A direct `--root`
  is root selection, not federation routing: such a message carried no
  sender-origin metadata, so the recipient could not reply and a naive reply
  looped back into their own tree. Replyable cross-tree messaging must use
  `--project`/`--session`. Bare-root sends with no session env set are
  unaffected (#144).
- `amq send` no longer stamps `reply_to` on ordinary same-session sends; it is
  stamped only for actual cross-session/cross-project routes. The stray
  same-session `reply_to` is what made a direct cross-root send look replyable
  while looping into the replier's own tree (#144).


## [0.35.0] - 2026-06-13
### Added

- `amq send` and `amq reply` accept `--allow-empty` to deliver an intentionally
  blank body (for example when the subject carries the full message) (#143).

### Changed

- `amq send` and `amq reply` now treat `--body -` (and `--body @-` or an omitted
  `--body`) as stdin per the standard CLI convention, and **fail closed** when the
  resolved body is empty or whitespace-only instead of silently delivering a
  blank message. Previously `--body -` shipped a literal hyphen, so a dropped or
  mistyped body could reach the recipient blank with no warning. Pass
  `--allow-empty` to send a blank body deliberately (#143).
- Bumped the Go toolchain directive to 1.25.11 so CI and release checks pick up
  the standard-library fixes for GO-2026-5039 (`net/textproto`) and GO-2026-5037
  (`crypto/x509`) that `govulncheck` now flags.


## [0.34.1] - 2026-05-11
### Added

- `amq coop exec --require-wake` now refuses to launch the agent command unless
  the background wake process starts and confirms it acquired the wake lock,
  giving managed launchers a safe mode for wake health enforcement (#120).

### Changed

- Bumped the Go toolchain directive to 1.25.10 so CI and release checks use the
  standard-library vulnerability fixes required by `govulncheck`.


## [0.34.0] - 2026-04-28
### Added

- `amq wake` now supports an explicit external injection transport via `--inject-via <executable>`, repeatable `--inject-arg <arg>`, and bounded `--inject-timeout` (default `5s`), letting orchestrators and no-controlling-TTY environments receive wake notifications without TIOCSTI. AMQ appends the sanitized notification payload as the final argv element and does not run the command through a shell. `--bell` is honored on the inject-via path, and a one-time fallback warning is emitted before writing to stderr when the external injector fails (#99, closes #98).

### Fixed

- Release tooling preserves CHANGELOG compare links when preparing release PRs (#116).



## [0.33.0] - 2026-04-28
### Added

- `amq env --json` now emits the documented v1 machine-readable contract with `schema_version`, `amq_version`, `base_root`, `in_session`, `root_source`, always-present string fields, and `{}` for unconfigured `peers` (#101).
- Reserved extension metadata namespaces under `<AM_ROOT>/extensions/<layer>/` and `<AM_ROOT>/agents/<handle>/extensions/<layer>/`; `amq doctor --json` now reports passive root extension manifests and malformed extension metadata diagnostics without executing extension code (#102).
- `amq route explain --json` now reports canonical route resolution with routability, structured `argv`, display command, source/delivery roots, project, and session metadata for same-session, cross-session, and cross-project sends (#103).
- `amq send --from-session <source-session>` supports setup-terminal cross-session sends from a base root, writing the sender outbox in the source session and stamping `reply_to` for replies back to that session (#104).

### Fixed

- Explicit `--root`/`--from-root` project lookups no longer fall back to the current working directory's `.amqrc`, and global `~/.amqrc` no longer infers project identity from the home directory basename.
- `amq env --json` now emits `.amqrc` peer paths as resolved absolute paths so consumers do not need to reimplement AMQ's peer path resolution.
- Extension layer names now reject `..` substrings, and `amq doctor --json` only reads passive extension manifests that are regular files below the size cap.


## [0.32.2] - 2026-04-27
### Added

- `amq wake` now has an enabled-by-default, best-effort input-activity deferral gate before non-interrupt TIOCSTI injection. The gate only runs after a wake notification is pending, samples the controlling terminal for unread input and recent reads, and is bounded by `--input-poll-interval`, `--input-quiet-for`, and `--input-max-hold`. This does not prove the foreground app's prompt buffer is empty; a paused in-progress prompt can still be injected into and submitted. Atime sampling uses stdin when it is a TTY (the `/dev/tty` alias inode does not track underlying ttys reads on macOS, and a freshly opened `/dev/tty` fd is not always in the tty's open-file list on Linux); Linux tty atime is maintained at ~8s granularity, so `--input-quiet-for` values shorter than that are advisory.


## [0.32.1] - 2026-04-13
### Fixed

- `scripts/claude-session-start.sh` now rotates oversized `$HOOK_LOG` files opportunistically at hook start, keeping stderr logging bounded without affecting hook output or exit behavior.


## [0.32.0] - 2026-04-13
### Added

- `scripts/claude-session-start.sh` phase 2: SessionStart hook now re-injects coop context (session, project, peers, unread count) as `additionalContext` after `/clear` or context compaction, restoring the awareness Claude Code loses when its context is reset (#84, fixes #71). Composes existing CLI primitives — no new Go surface.
- Smoke test coverage for the SessionStart hook: phase 1 env-file write, phase 2 JSON shape, `/clear` recovery, quoted-root path round-trip, and `/clear` with env-file-only `AM_ME=<non-default>`.

### Fixed

- SessionStart hook is now safe under stock macOS `/bin/bash` 3.2 + `set -u`; replaced empty-array expansion (`"${ROOT_FLAGS[@]}"`) with explicit rooted/non-rooted command branches.
- SessionStart hook correctly round-trips POSIX single-quote-escaped roots (e.g. paths containing `'`); replaced fragile `sed` decoding with isolated `/bin/sh` eval of the matched `export` line.
- SessionStart hook `/clear` recovery now reloads `AM_ME` from the env file symmetrically to `AM_ROOT`, so phase 2 targets the correct handle when only the env file carries identity.


## [0.31.3] - 2026-04-12
### Changed

- Added token-efficiency guidance to the `amq-cli` skill: send file paths instead of inlining large file contents, and run multi-round AMQ review loops in background workers or subagents so intermediate rounds stay out of the main context.


## [0.31.2] - 2026-04-10
### Changed

- Doc sweep to align CLAUDE.md, README.md, skills, and CLI help text with the receipt ledger model — no more stale ack references in agent-facing or user-facing docs.
- Removed the unused `Header.AckRequired` field and `ack_required` JSON tag from the message format. Outgoing messages no longer carry the dead `"ack_required": false` field in their frontmatter.
- Dropped dead `--ack=false` branches from drain test helpers; simplified signatures to match the current drain API.


## [0.31.1] - 2026-04-10
### Changed

- `amq read` now applies the same strict header validator as `drain` and `monitor`, so messages with malformed headers are moved to DLQ and get a `dlq` receipt instead of staying in `inbox/new`.
- Simplified `receipt.WaitFor()` by collapsing the redundant `agent` parameter; callers now pass only the consumer that owns the receipt namespace.


## [0.31.0] - 2026-04-09
### Added

- Added delivery receipts with `drained` and `dlq` stages, plus the new `amq receipts list` and `amq receipts wait` commands for querying receipt history and waiting on receipt arrival.
- Added `amq send --wait-for <stage>` so senders can block for delivery confirmation on single-recipient handoffs.
- Added `receipt.WaitFor()` for targeted receipt polling by message id, consumer, and stage.

### Changed

- Replaced the old ack model with a receipt ledger stored under agent `receipts/` directories.
- Simplified receipt emission to consumer-local writes, with send-side waits reading from the actual delivery root instead of relying on mirrored receipt files.
- Bumped the Go toolchain to 1.25.9.

### Removed

- Removed the `amq ack` command, `--ack` flags, `ack_required` header field, and `acks/` directories from the active protocol and docs.

### Fixed

- Validated `header.ID` in `amq read` before emitting receipts, closing a path-manipulation risk on malformed message headers.

## [0.30.1] - 2026-04-05
### Added

- Regression tests for session name detection: 2 Go monitor tests (JSON session field) and 5 Python tests covering all resolution paths (`AM_BASE_ROOT`, `.amqrc`, `.agent-mail`, sibling sessions, non-session roots).
- Python session-name tests integrated into `smoke-test.sh` for CI coverage.


## [0.30.0] - 2026-04-05
### Added

- Notifications from `wake`, `monitor`, and hook scripts now include the session name (e.g., `AMQ [stream3]: message from codex - ...`) so agents can identify which session a message belongs to in multi-session setups.
- `monitor` JSON output includes a new `session` field when inside a session context.
- Python hook scripts (`codex-amq-notify.py`, `claude-amq-user-prompt-submit.py`) mirror the full Go `classifyRoot` logic including `AM_BASE_ROOT` and `.amqrc` resolution for session detection.


## [0.29.1] - 2026-04-05
### Fixed

- `amq send` and `amq reply` no longer silently drop positional arguments; they now return a usage error (exit 2) suggesting `--body`.


## [0.29.0] - 2026-04-04
### Fixed

- `--root` flag now overrides `AM_ROOT` when explicitly provided, fixing cross-session and cross-project sends from within active coop sessions.
- `classifyRoot()` no longer blindly trusts stale `AM_BASE_ROOT`; validates the supplied root is actually under the base before using it.
- Consolidated skill publishing into `release.yml` so it runs directly after the release job instead of relying on a tag-triggered workflow (tags pushed with `GITHUB_TOKEN` do not trigger other workflows).

### Changed

- `classifyRoot()` recognizes the default `.agent-mail` directory convention, enabling session detection in projects without `.amqrc`.
- Removed `guardRootOverride()` and dead `validate()` call sites across all command handlers (-140 lines).
- `send` and `reply` emit a `note:` to stderr when `--root` overrides `AM_ROOT` for visibility.
- `configuredBaseRoot()` now logs `.amqrc` parse/permission errors to stderr instead of swallowing them silently.

- SHA-pinned all remaining GitHub Actions across every workflow.
- Added concurrency groups and timeouts to all workflows.
- Scoped `release.yml` permissions per job instead of top-level `contents: write`.
- Reduced `publish-skill.yml` to a manual `workflow_dispatch` fallback (no longer triggered by tag push).
- Added `skip-skill-publish` dispatch input to `release.yml` for manual reruns.
- Updated `release.sh` PR body to reflect the consolidated release flow.


## [0.28.8] - 2026-04-02
### Fixed

- Passed the temp release-notes path directly to GoReleaser so GitHub Actions preserves the `--release-notes` argument during publishing.

### Fixed

- Wrote generated GitHub release notes to the runner temp directory so GoReleaser can publish without dirtying the checked-out tree.


## [0.28.7] - 2026-04-02
### Fixed

- Let release verification honor an explicit `VERSION` override so CI checks the tagged binary instead of a `git describe` snapshot.


## [0.28.6] - 2026-04-02
### Changed

- Switched releases to the shared PR-based `scripts/release.sh` flow, with `CHANGELOG.md` supplying the GitHub release notes and CI creating the version tag only after the merged release commit verifies.

### Fixed

- Removed deprecated release shims so there is exactly one supported release entrypoint.


## [0.28.5] - 2026-04-01

### Fixed

- Pinned the GitHub release workflow to the GoReleaser v2 series instead of floating `latest`, so upstream releases cannot silently change the AMQ release pipeline.
- Keyed manual release reruns to the requested tag in the workflow concurrency group, so rerunning an older release no longer shares the default-branch concurrency slot.

## [0.28.4] - 2026-04-01

### Fixed

- Treated `Version already exists` as success when a skill publish reruns after retrying without an alias, preventing false-negative publish failures after a successful publish without alias fallback.

## [0.28.3] - 2026-04-01

### Changed

- Aligned the shared release helper and GitHub workflows so manual tag reruns, metadata verification, marketplace notification, and skill publishing all follow the same release path.

## [0.28.2] - 2026-04-01

### Fixed

- Ad-hoc signed macOS release binaries with the stable identifier `io.github.avivsinai.amq` so Keychain approvals survive Homebrew upgrades.

### Changed

- Moved the release workflow onto `macos-latest` so signed darwin artifacts are produced in CI before Homebrew updates.

## [0.28.1] - 2026-04-01

### Fixed

- Avoided retrying skill publishes after an alias failure when the package version had already been uploaded successfully.
- Hardened release metadata validation so skill and plugin manifest versions must match the release tag before publishing.

### Changed

- Added a default-branch marketplace dispatch workflow so plugin updates are announced after merges to `main`.
- Documented the marketplace dispatch behavior and generalized release helper usage from fixed examples to `X.Y.Z`.

## [0.28.0] - 2026-03-30

### Added

- Tag-based skill publishing aligned with versioned releases.
- Tab-title statusline guidance in the AMQ skill documentation.

### Fixed

- Addressed release workflow issues around dispatch input handling, version validation, and variable name collisions.

## [0.27.0] - 2026-03-30

### Added

- `amq env --session-name` flag: prints current session name for statusline embedding (empty + exit 0 when not in a session)
- `session_name` field in `amq env --json` output
- Session-aware routing instructions in amq-cli skill: Claude now discovers sessions via `amq who --json` before cross-session sends
- Statusline snippet documentation in SKILL.md for showing AMQ session in Claude Code status bar
- Wake presence heartbeat: `amq wake` touches presence on startup and every 30s, so `amq who` reports agents as active while working

### Changed

- Moved `classifyRoot` from `send.go` to `common.go` (shared by send, reply, who, env)
- Added `resolveSessionName` helper combining `classifyRoot` + `sessionName`

## [0.26.0] - 2026-03-29

### Added

- Embedded the AMQ design philosophy in the project docs.

### Changed

- Added the main-branch documentation policy and removed frozen implementation
  specs from the docs tree.
- Aligned plugin manifests and metadata for the 0.26.0 release.

## [0.25.1] - 2026-03-28

### Changed

- Bumped the Codex plugin manifest version to 0.25.1.

## [0.25.0] - 2026-03-28

### Added

- Added cross-orchestrator integration surfaces for Symphony, Kanban, and
  `doctor --ops` (#47).
- Added Codex interface metadata to the plugin manifest.

### Fixed

- Corrected cross-project sender identity by preserving `from_project` (#48).

### Changed

- Renamed the spec skill to `amq-spec` to avoid naming collisions.
- Eliminated duplicated skill packaging.

## [0.24.1] - 2026-03-22

### Fixed

- `amq who` always showed agents as "stale" because presence was only updated by explicit `amq presence set` calls
- Presence `LastSeen` is now auto-updated (best-effort) on `send`, `drain`, and `reply`
- `presence.Touch` only creates a default record on missing file — corrupt presence files are no longer silently overwritten

### Added

- `presence.Touch(root, handle)` function for lightweight presence refresh

## [0.24.0] - 2026-03-19

### Added

- Cross-project messaging: send messages between agents in different projects on the same machine
- `.amqrc` extended with `project` (self-identity) and `peers` (name→path registry) fields
- `--project` flag for `amq send` to target a peer project's inbox
- Inline `agent@project:session` addressing syntax for terser cross-project sends
- `reply_project` header field for automatic cross-project reply routing
- `DeliverToExistingInbox`: atomic Maildir delivery that never creates directories in peer projects
- `findAmqrcForRoot`: root-aware `.amqrc` lookup (works when cwd differs from project dir)
- Decision threads protocol: decentralized cross-project decisions using existing AMQ primitives
- Skill docs: Cross-Project Routing and Decision Threads sections (v1.7.0)
- New reference doc: `references/cross-project.md`

### Changed

- `findAmqrcForRoot` prioritizes root-based lookup over cwd when root is provided
- Session detection uses `.amqrc` base root comparison as fallback when `classifyRoot` fails
- `--json` output omits misleading `source_session` when sender is at base root

## [0.23.0] - 2026-03-18

### Added

- Shell completions: `amq completion bash|zsh|fish` generates tab-completion scripts
- Routed help: `amq help <command> [subcommand]` dispatches to command-specific help
- Command registry: centralized command metadata drives help, routing, and completions

### Fixed

- `amq init` and `amq cleanup` now exit with code 2 (not 1) on missing required flags
- Flag parse errors (`amq send --bogus`) now exit with code 2
- Unknown command/subcommand errors include help hints consistently
- `amq completion --help` shows usage instead of erroring
- `amq env --help` no longer shows duplicate "Usage:" header

### Changed

- Top-level and subcommand group help auto-generated from registry (single source of truth)
- Presence help enriched to match dlq/swarm/coop format
- Empty "Options:" section suppressed when command has no flags

## [0.22.0] - 2026-03-18

### Added

- Cross-session messaging via `amq send --session <name>` with reply routing between sessions
- `amq who` command to list sessions and agents with active/stale presence status

### Changed

- Cross-session peer-to-peer threads are now session-qualified to avoid collisions
- `coop exec` now sets `AM_BASE_ROOT` for cross-session resolution

### Fixed

- Tightened cross-session validation around `reply_to`, session context detection, and foreign inbox checks

## [0.21.0] - 2026-03-11

### Added

- `amq swarm fail` and `amq swarm block` for richer task lifecycle tracking
- `amq swarm complete --evidence <json|@file>` for attaching structured proof-of-work

### Changed

- Swarm tasks now support `failed` and `blocked` statuses in listings and bridge events

### Fixed

- Reclaiming failed or blocked tasks now clears stale failure, block, and evidence metadata

## [0.20.0] - 2026-03-11

### Added

- `/amq-spec` slash command for the collaborative specification workflow

### Changed

- Moved the spec workflow out of the core CLI and into the `amq-spec` skill
- Replaced spec-specific core message kinds with generic kinds plus labels

### Fixed

- Tightened spec workflow follow-ups and corrected `NEXT STEP` phase guidance
- Enforced send-first-research-second and prevented partner implementation during spec review
- Bumped Go to 1.25.8 for `govulncheck` advisories `GO-2026-4602` and `GO-2026-4601`

## [0.19.0] - 2026-03-05

### Added

- `amq coop spec` collaborative specification workflow with guided `NEXT STEP` output

### Changed

- `coop exec` now defaults to `--session collab` when neither `--session` nor `--root` is provided
- Message-routing commands now require an explicit AMQ root context instead of inferring one implicitly

### Fixed

- Suppressed duplicate update checks in the `coop exec` wake subprocess
- Corrected `AM_ROOT` guidance in the AMQ skill for usage outside `coop exec`

## [0.18.0] - 2026-02-24

### Added

- `amq shell-setup` command: outputs shell aliases for quick co-op session management
- `--session` flag on `coop exec` and `env`: pure sugar for `--root <base>/<session>`

### Changed

- `--root` is now literal — no implicit session subdirectory appended
- `.amqrc` format simplified: `{"root": "..."}` (removed `default_session`)
- `coop init` no longer prompts for shell alias installation (use `eval "$(amq shell-setup)"` instead)

### Removed

- `--install` flag from `shell-setup` (use `eval "$(amq shell-setup)"` in your rc file)
- `default_session` field from `.amqrc` format
- Interactive prompts from `coop init`

## [0.17.1] - 2026-02-11

### Fixed

- Don't overwrite `.amqrc` when `--root` is explicitly provided in `coop exec`

## [0.17.0] - 2026-02-10

### Added

- `coop exec` command for running agents inside a cooperative session

### Removed

- `coop shell` and `coop start` commands (replaced by `coop exec`)

### Changed

- `.amqrc` is now written to `defaultRoot` instead of CWD

## [0.16.0] - 2026-02-08

### Added

- Agent Teams (swarm) integration with full codebase review fixes
- Homebrew tap auto-update via goreleaser

### Fixed

- CI release race condition with concurrency control
- CI: add `HOMEBREW_TAP_GITHUB_TOKEN` for goreleaser brew push

### Changed

- Bump `actions/setup-node` from 4 to 6
- Bump `actions/checkout` from 4 to 6

## [0.15.0] - 2026-02-04

### Added

- Initiator protocol and wake interrupts for agent coordination

## [0.14.1] - 2026-02-01

### Fixed

- Add `.amqrc` to `.gitignore` during `coop init`
- Skill publish workflow handles existing versions gracefully

### Changed

- Bump `golang.org/x/term` from 0.38.0 to 0.39.0
- Bump `actions/checkout` from 6.0.1 to 6.0.2
- Bump `actions/setup-go` from 6.1.0 to 6.2.0
- Bump `golang.org/x/sys` from 0.39.0 to 0.40.0

## [0.14.0] - 2026-01-28

### Changed

- `coop start` no longer execs agent; auto-starts wake instead

## [0.13.1] - 2026-01-26

### Fixed

- Auto-create `.gitignore` with `agent-mail` directory entry

[Unreleased]: https://github.com/avivsinai/agent-message-queue/compare/v0.41.0...HEAD
[0.41.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.40.0...v0.41.0
[0.40.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.39.0...v0.40.0
[0.39.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.38.0...v0.39.0
[0.38.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.37.1...v0.38.0
[0.37.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.37.0...v0.37.1
[0.37.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.36.0...v0.37.0
[0.36.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.35.0...v0.36.0
[0.35.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.34.1...v0.35.0
[0.34.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.34.0...v0.34.1
[0.34.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.33.0...v0.34.0
[0.33.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.32.2...v0.33.0
[0.32.2]: https://github.com/avivsinai/agent-message-queue/compare/v0.32.1...v0.32.2
[0.32.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.32.0...v0.32.1
[0.32.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.31.3...v0.32.0
[0.31.3]: https://github.com/avivsinai/agent-message-queue/compare/v0.31.2...v0.31.3
[0.31.2]: https://github.com/avivsinai/agent-message-queue/compare/v0.31.1...v0.31.2
[0.31.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.31.0...v0.31.1
[0.31.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.30.1...v0.31.0
[0.30.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.30.0...v0.30.1
[0.30.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.29.1...v0.30.0
[0.29.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.29.0...v0.29.1
[0.29.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.8...v0.29.0
[0.28.8]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.7...v0.28.8
[0.28.7]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.6...v0.28.7
[0.28.6]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.5...v0.28.6
[0.28.5]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.4...v0.28.5
[0.28.4]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.3...v0.28.4
[0.28.3]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.2...v0.28.3
[0.28.2]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.1...v0.28.2
[0.28.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.28.0...v0.28.1
[0.28.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.27.0...v0.28.0
[0.27.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.26.0...v0.27.0
[0.26.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.25.1...v0.26.0
[0.25.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.25.0...v0.25.1
[0.25.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.24.1...v0.25.0
[0.24.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.24.0...v0.24.1
[0.24.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.23.0...v0.24.0
[0.23.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.22.0...v0.23.0
[0.22.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.21.0...v0.22.0
[0.21.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.20.0...v0.21.0
[0.20.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.19.0...v0.20.0
[0.19.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.18.0...v0.19.0
[0.18.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.17.1...v0.18.0
[0.17.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.17.0...v0.17.1
[0.17.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.14.1...v0.15.0
[0.14.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.14.0...v0.14.1
[0.14.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.13.1...v0.14.0
[0.13.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.13.0...v0.13.1
