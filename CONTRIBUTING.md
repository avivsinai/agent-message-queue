# Contributing

AMQ accepts focused fixes, features, and documentation improvements.

## Ground rules

- Be respectful and professional. See the [code of conduct](CODE_OF_CONDUCT.md).
- Keep changes focused and small; prefer incremental PRs.

## Development

Use the Go toolchain in `go.mod`. `make lint` requires the version of
`golangci-lint` pinned in the Makefile. Documentation generation uses Python 3.

Start by installing the git hooks so every push runs the full CI gate
locally. Run this in your primary clone before creating worktrees, so the
hook lands in the shared `.git/hooks` path that all worktrees use:

```bash
./scripts/install-hooks.sh
```

The pre-push hook runs `make ci`; if it fails, fix the issues before pushing
and never bypass it with `--no-verify`.

Then build and test:

```bash
make build
make fmt
go test ./internal/format  # Example: check the package you changed.
```

Run focused checks locally. CI runs the full `make ci` gate, including format,
vet, lint, tests, smoke checks, and generated-document checks. Do not run
installation or shared-home tests against a working agent environment.

When command help changes, run `make docs-cli` and include the regenerated
[CLI reference](docs/cli.md). Edit canonical agent skills under `skills/` and
run `make check-skills`; the other skill paths are symlinks.

Release Please maintains release PRs from conventional squash commits on
`main`. Do not change release or skill version fields for an ordinary contribution.

## Live proofs (opt-in)

These checks start real provider processes or use a terminal surface. Run only
the relevant check in an operator-approved, isolated environment, not in a
shared agent session. They skip unless their environment switch is `1`.
Provider checks need the matching installed CLI and credentials; terminal
checks need the matching macOS application.

```bash
AMQ_CMUX_LIVE=1 go test ./internal/keepalive/adapter -run '^TestCmuxLiveDiscoverProbe$' -count=1 -v
AMQ_GHOSTTY_LIVE=1 go test ./internal/keepalive/adapter -run '^TestGhosttyLiveDiscoverProbe$' -count=1 -v
AMQ_CLAUDE_LIVE=1 go test ./internal/keepalive/adapter -run '^TestClaudePrintLiveResumeAck$' -count=1 -v
AMQ_CODEX_LIVE=1 AMQ_CODEX_LIVE_THREAD="<scratch-thread-uuid>" go test ./internal/keepalive/adapter -run '^TestCodexQueueLiveEnqueue$' -count=1 -v
```

Read the selected test before running it. The terminal probes create and close
windows or workspaces; use a dedicated application instance with no concurrent
users. The Claude check creates and resumes a scratch conversation. The Codex
check submits to the exact live thread you supply; replace the placeholder
with a disposable thread UUID and keep its writer open. Never omit that UUID:
the test otherwise selects an active thread. A skipped check is not a live result.

## Pull requests

- Include a clear description of the change and why it matters.
- Use a conventional title such as `feat: add routing` or
  `fix(wake): preserve input` so the squash commit drives release notes.
- Add a small happy-path test for new behavior, or a regression test that
  reproduces an observed defect. Keep tests focused and deterministic.
- Use a separate worktree when other people or agents share the checkout.
- Avoid reformatting unrelated code.

## Reporting issues

Please include:

- OS and AMQ version (and Go version if built from source)
- Exact command(s) run
- Expected vs actual behavior
- Relevant logs, with secrets and private message contents removed
