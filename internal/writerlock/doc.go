// Package writerlock reports whether a Codex thread-writer lock
// (<CODEX_HOME>/thread-writer-locks/<thread>.lock) is held, without acquiring
// it. Codex holds that lock while a thread is loaded.
//
// Held returns (true, nil) when a writer holds the lock, (false, nil) when the
// lock is not held or the file is missing, and (false, err) when the state is
// unknown. Inspection errors fail closed: there is no lsof fallback, because
// an open file is not a held flock.
package writerlock
