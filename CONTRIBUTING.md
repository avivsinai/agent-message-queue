# Contributing

Thanks for your interest in AMQ!

## Ground rules

- Be respectful and professional. See `CODE_OF_CONDUCT.md`.
- Keep changes focused and small; prefer incremental PRs.

## Development

```bash
make fmt
make test
make vet
make lint
make ci
```

Run `make ci` before opening a PR; it is the canonical local gate and includes
format, vet, lint, test, and smoke-test coverage. Install local hooks with
`scripts/install-hooks.sh` when working in this repo, and use
`scripts/release.sh` for release PRs.

## Pull requests

- Include a clear description of the change and why it matters.
- Add or update tests when behavior changes.
- Avoid reformatting unrelated code.

## Reporting issues

Please include:
- OS + Go version
- Exact command(s) run
- Expected vs actual behavior
- Any relevant logs
