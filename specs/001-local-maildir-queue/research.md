# Research Notes

This spec relies on well-known file semantics and existing formats.

## Maildir semantics

Maildir defines the tmp -> new -> cur lifecycle and discourages readers from scanning tmp. It recommends writing to tmp, closing, and then atomically moving to new.
Reference: https://manpages.debian.org/jessie/qmail/maildir.5.en.html

## Atomic rename and durability

- POSIX rename is atomic for replacing a path on the same filesystem.
  Reference: https://man7.org/linux/man-pages/man2/rename.2.html

- fsync also applies to directories and is required to ensure directory entries are durable after rename.
  Reference: https://man7.org/linux/man-pages/man2/fsync.2.html

## Go standard library semantics

- os.Rename may not be atomic on non-Unix platforms.
  Reference: https://pkg.go.dev/os#Rename

- File.Sync commits file contents to stable storage.
  Reference: https://pkg.go.dev/os#File.Sync

## Rust standard library semantics

- std::fs::rename is a thin wrapper around platform rename calls.
  Reference: https://doc.rust-lang.org/std/fs/fn.rename.html

## Node.js semantics (for TS alternative)

- fs.rename provides access to the underlying OS rename.
  Reference: https://nodejs.org/api/fs.html#fsrenameoldpath-newpath-callback

## JSON Lines (optional thread index)

- JSON Lines is a convenient append-only record format for logs.
  Reference: https://jsonlines.org/
