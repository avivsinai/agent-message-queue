# AGENTS.md

This file provides guidance for Codex and other AI coding agents. **The authoritative source is `CLAUDE.md`** - this file exists for compatibility with agents that look for `AGENTS.md`.

## Quick Reference

See [`CLAUDE.md`](./CLAUDE.md) for complete instructions including:

- Project overview and architecture
- Build commands (`make build`, `make test`, `make ci`)
- CLI usage and common flags
- Multi-agent coordination patterns
- Security practices (0700/0600 permissions, `--strict` flag)
- Contributing guidelines and commit conventions

## Essential Commands

```bash
make ci              # Run before committing: vet, lint, test, smoke
./amq --version      # Check installed version
```

## Environment

```bash
export AM_ROOT=.agent-mail
export AM_ME=<your-agent-handle>
```

## Key Constraints

- Go 1.25+ required
- Handles must be lowercase: `[a-z0-9_-]+`
- Never edit message files directly; use the CLI
- Cleanup is explicit (`amq cleanup`), never automatic
