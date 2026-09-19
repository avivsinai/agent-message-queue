# Contributing

AMQ accepts focused fixes, features, and documentation improvements.

## Ground rules

- Be respectful and professional. See the [code of conduct](CODE_OF_CONDUCT.md).
- Keep changes focused and small; prefer incremental PRs.

## Development

Use the Go toolchain in `go.mod`. `make lint` requires the version of
`golangci-lint` pinned in the Makefile. Documentation generation uses Python 3.

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
