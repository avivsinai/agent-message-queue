# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[0.18.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.17.1...v0.18.0
[0.17.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.17.0...v0.17.1
[0.17.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.14.1...v0.15.0
[0.14.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.14.0...v0.14.1
[0.14.0]: https://github.com/avivsinai/agent-message-queue/compare/v0.13.1...v0.14.0
[0.13.1]: https://github.com/avivsinai/agent-message-queue/compare/v0.13.0...v0.13.1
