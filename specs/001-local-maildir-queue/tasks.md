# Engineering Tasks

## Repo setup

- [ ] Confirm CLI name (amq) and root env vars
- [ ] Add AGENTS.md or CLAUDE.md for tool usage (optional)

## Core packages

- [ ] internal/format: frontmatter marshal/unmarshal
- [ ] internal/fsq: tmp/new/cur operations with fsync
- [ ] internal/config: load/write config
- [ ] internal/ack: create ack files
- [ ] internal/thread: thread reconstruction
- [ ] internal/presence: presence set/list

## CLI commands

- [ ] init
- [ ] send
- [ ] list
- [ ] read
- [ ] ack
- [ ] thread
- [ ] presence set
- [ ] presence list
- [ ] cleanup

## Tests

- [ ] Message id format and uniqueness
- [ ] Frontmatter round-trip
- [ ] Atomic delivery flow
- [ ] Read moves new -> cur
- [ ] Ack writes both files
- [ ] Thread merge ordering
- [ ] Cleanup removes old tmp files only

## Documentation

- [ ] Update README with final CLI usage
- [ ] Add examples for two-agent workflow
