# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
