# Contributing

AMQ accepts focused fixes, features, and documentation improvements.

## Ground rules

- Be respectful and professional. See the [code of conduct](CODE_OF_CONDUCT.md).
- Keep changes focused and small; prefer incremental PRs.

## Development

Use the Go toolchain in `go.mod`. `make lint` requires the version of
`golangci-lint` pinned in the Makefile. Documentation generation uses Python 3.

Start by installing the git hooks so every push runs the static checks
locally. Run this in your primary clone before creating worktrees, so the
hook lands in the shared `.git/hooks` path that all worktrees use:

```bash
./scripts/install-hooks.sh
```

The pre-push hook runs `make check` (format, vet, lint, module, skill, and
CLI-doc checks); if it fails, fix the issues before pushing and never bypass
it with `--no-verify`. If you installed an older hook, rerun the script with
`--force` to replace it.

Then build and test:

```bash
make build
make fmt
go test ./internal/format  # Example: check the package you changed.
```

Run the tests of the packages you changed locally. CI runs the full `make ci`
gate: `make check` plus all tests, race tests, and smoke checks. Do not run
installation or shared-home tests against a working agent environment.

When command help changes, run `make docs-cli` and include the regenerated
[CLI reference](docs/cli.md). Edit canonical agent skills under `skills/` and
run `make check-skills`; the other skill paths are symlinks.

Do not change release or skill version fields for an ordinary contribution.
Run `scripts/release.sh X.Y.Z` from this checkout to request a release. The
command dispatches Release Please on `main`, waits up to ten minutes, and prints
its PR URL. Release Please alone generates the changelog and all version changes;
review and merge that PR through the normal checks. The current release must be
published before a newer version can be requested.

After merge, `release-tag.yml` binds the release to its merged PR and verifies
metadata, tests, smoke checks, and embedded versions before the repository App
creates its tag. `release.yml` publishes on that tag push. Tag creation requires
`RELEASE_APP_CLIENT_ID` and `RELEASE_APP_PRIVATE_KEY`; there is no legacy token
fallback. Tags are immutable: a retry accepts only the same commit.

For a failed publication, dispatch **Release** with the existing tag. Leave
`skip-skill-publish` enabled unless skill publication also needs recovery. Use
`goreleaser-config-from-workflow-ref` only when recovering a tag whose packaging
configuration failed; it builds the tag source with the dispatched ref's
packaging configuration. Do not create or move a tag to retry publication.

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
AMQ_TMUX_LIVE=1 go test ./internal/launch -run '^TestTmuxBackendLifecycleAndRecovery$' -count=1 -v
AMQ_AGY_LIVE=1 go test ./internal/acp/host -run '^TestAgyLiveRemembersAcrossHostRestart$' -count=1 -v
AMQ_WAKE_SELF_UPGRADE_PTY_LIVE=1 go test ./internal/cli -run '^Test(Darwin|Linux)WakeSelfUpgradeRealPTYStableSymlink$' -count=1 -v
```

Read the selected test before running it. The tmux proof starts a private
tmux server on its own socket and removes it. The terminal probes create and close
windows or workspaces; use a dedicated application instance with no concurrent
users. The Claude check creates and resumes a scratch conversation. The agy check
runs the installed `agy` with its own login, in a temporary directory, for two
turns across a host restart. The Codex
check submits to the exact live thread you supply; replace the placeholder
with a disposable thread UUID and keep its writer open. Never omit that UUID:
the test otherwise selects an active thread. A skipped check is not a live result.
The wake self-upgrade proof replaces a binary through a real PTY-attached
wake daemon and takes about 40 seconds; run it alone in an isolated
environment, never in a shared agent session.

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
