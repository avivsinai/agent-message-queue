# Contributing

Thanks for your interest in AMQ!

## Ground rules

- Be respectful and professional. See `CODE_OF_CONDUCT.md`.
- Keep changes focused and small; prefer incremental PRs.

## Development

Install the pinned contributor toolchain with mise:

```bash
mise install
mise run ci
```

Mise delegates to the repository's canonical Make targets, which can also be
run directly:

```bash
make fmt
make test
make vet
make lint
make ci
```

Run `make ci` before opening a PR; it is the canonical local gate and includes
format, vet, lint, test, and smoke-test coverage. Install local hooks with
`scripts/install-hooks.sh` when working in this repo. Release Please opens and
updates release PRs from conventional squash commits on `main`.

## Pull requests

- Include a clear description of the change and why it matters.
- Use a conventional title such as `feat: add routing` or
  `fix(wake): preserve input` so the squash commit drives release notes.
- Add or update tests when behavior changes.
- Avoid reformatting unrelated code.

## Reporting issues

Please include:
- OS + Go version
- Exact command(s) run
- Expected vs actual behavior
- Any relevant logs
