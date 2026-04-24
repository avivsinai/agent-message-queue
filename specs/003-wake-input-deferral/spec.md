# Spec: Wake Input-Activity Deferral

## Objective

Reduce `amq wake` collisions with active terminal input while preserving the existing TIOCSTI-based communication path. Wake should delay injection when terminal input appears active, then inject the same notification text using the same injection modes once the terminal has been quiet briefly.

## Tech Stack

- Go CLI under `cmd/amq` and `internal/cli`
- Unix terminal APIs on macOS/Linux: `TIOCSTI`, `TIOCINQ`/`FIONREAD`, and `fstat`
- Existing `fsnotify` wake loop remains the mailbox trigger

## Commands

- Build: `make build`
- Test: `go test ./internal/cli`
- Full test: `make test`
- CI: `make ci`

## Project Structure

- `internal/cli/wake.go` - wake notification construction and injection decision logic
- `internal/cli/wake_unix.go` - Unix wake flags and event loop
- `internal/cli/wake_tiocsti_unix.go` - Unix TIOCSTI and TTY sampling helpers
- `internal/cli/wake_input_*.go` - platform-specific input queue ioctl request constants
- `internal/cli/wake_test.go` - pure behavior tests for deferral decisions
- `COOP.md` - user-facing wake option docs

## Code Style

Keep the hot path explicit and boring:

```go
if cfg.deferWhileInput {
	waitForTTYInputQuiet(cfg)
}
return tiocsti.Inject(text)
```

Avoid background goroutines and long-lived polling. Defer only while a message is already pending.

## Testing Strategy

- Unit-test pure deferral logic with synthetic timestamps and pending-byte counts.
- Avoid unit tests that require a real interactive TTY.
- Rely on existing wake tests plus `go test ./internal/cli` for compile coverage on the local platform.

## Boundaries

- Always: preserve TIOCSTI as the default wake transport.
- Always: fail open if terminal activity cannot be sampled.
- Always: bound polling by interval and max hold.
- Ask first: changing notification text or removing the injected carriage return.
- Never: add idle polling while no AMQ message is pending.

## Success Criteria

- `amq wake` exposes an enabled-by-default input-activity deferral gate.
- No additional syscalls occur while wake is idle and no message is pending.
- While a message is pending, wake samples at most one queue-depth ioctl and one `fstat` per poll interval.
- Deferral exits when input queue is empty and the tty read time is older than the quiet window, or when max hold expires.
- Tests cover pending-byte deferral, recent-read deferral, inactive state, and max-hold delay bounding.

## Open Questions

- Terminal `atime` behavior is platform/filesystem dependent; treat it as a heuristic, not a correctness guarantee.
- Future app cooperation could provide a true prompt-empty signal, but that is outside this experiment.
