Here is a comprehensive review of the `agent-message-queue` codebase.

### Summary
The codebase is clean, well-structured, and strictly adheres to the provided specification. The implementation of Maildir-style atomic writes (`tmp` -> `new` -> `cur`) and `fsync` usage demonstrates a strong understanding of POSIX durability requirements.

However, there are significant performance bottlenecks regarding how data is queried (O(N) disk reads), an invalid Go version, and strictness issues with the "Read" lifecycle that could lead to double-processing in failure scenarios.

---

### 1. Critical & Major Issues

#### 1.1. Invalid Go Version
**File:** `go.mod`
**Issue:** `go 1.25`
**Details:** Go 1.25 has not been released yet (current stable is 1.24). This will prevent the project from building on standard toolchains.
**Fix:** Downgrade to `1.24` or `1.23`.

#### 1.2. Scalability Bottleneck (O(N) Disk Reads)
**File:** `internal/cli/list.go`, `internal/thread/thread.go`
**Issue:** The `list` and `thread` commands iterate over directory entries and open/parse **every single file** to check headers (Subject, Thread ID, etc.).
**Impact:** As the mailbox grows (e.g., >1000 messages), CLI responsiveness will degrade linearly. `amq thread` is particularly expensive as it scans *all* folders of *all* agents.
**Recommendation:**
1.  **Short term:** Implement a "lazy load" that trusts the file modification time (`os.Stat`) for sorting and only reads headers for the requested page/limit.
2.  **Long term:** Maintain a separate sidecar index (e.g., `sqlite` or a simple `jsonl` log) updated on `send`/`ack`, as noted in your research notes.

#### 1.3. "At-Least-Once" vs "Exactly-Once" Processing Risk
**File:** `internal/cli/read.go`
**Context:**
```go
msg, err := format.ReadMessageFile(path) // 1. Read content
if err != nil { return err }

if box == fsq.BoxNew {
    if err := fsq.MoveNewToCur(root, common.Me, filename); err != nil { // 2. Change state
        return err
    }
}
```
**Issue:** If the agent crashes or the network fails between Step 1 (Read) and Step 2 (Move), the message remains in `new`. The agent will process the message again next time.
**Fix:** In strict Maildir consumer implementations, the atomic move (`new` -> `cur` or `new` -> `tmp_processing`) happens **before** returning the content to the logic layer.
*   **Proposed Flow:** Move `new` -> `cur`. If successful, read from `cur`. If move fails (file gone), error.

#### 1.4. Linter Configuration
**File:** `.golangci.yml`
**Issue:** `errcheck` is explicitly disabled.
```yaml
disable:
  - errcheck
```
**Impact:** In a filesystem-based queue, ignoring errors (especially from `file.Close()` or `fmt.Fprint`) can hide data corruption or "disk full" scenarios.
**Fix:** Enable `errcheck` and explicitly handle or ignore errors in code (e.g., `_ = file.Close()`).

---

### 2. correctness & Logic

#### 2.1. Frontmatter Parsing Fragility
**File:** `internal/format/message.go`
**Function:** `splitFrontmatter`
**Issue:** The parser looks for the byte sequence `\n---\n`.
```go
idx := bytes.Index(payload, []byte(frontmatterEnd))
```
**Risk:** If the JSON frontmatter itself (e.g., inside the `subject` or `refs` strings) contains the sequence `\n---\n`, the parser will split prematurely, returning invalid JSON.
**Fix:** While unlikely in this specific schema, a robust implementation should decode the JSON stream incrementally using `json.Decoder` until it finishes the object, rather than byte splitting.

#### 2.2. Atomic Write Race Conditions (Send)
**File:** `internal/cli/send.go`
**Issue:** The `send` command performs three distinct write operations:
1. Deliver to Recipient A
2. Deliver to Recipient B (if multiple)
3. Write to Sender Outbox
**Risk:** If step 1 succeeds but step 3 fails (disk full), the message is sent but not recorded in `sent`. There is no transaction rollback.
**Fix:** This is acceptable for a file-based system, but the CLI should output a warning to `stderr` if the outbox write fails, alerting the user that the audit trail is incomplete.

#### 2.3. Thread ID Canonicalization
**File:** `internal/cli/send.go`
**Function:** `canonicalP2P`
**Issue:** The function lowercases agent handles for the thread ID (`strings.ToLower`). However, the directory paths rely on the exact casing of the agent handle.
**Risk:** If `AM_ME` is "Codex" but the directory is "codex", `canonicalP2P` works, but file operations might fail on case-sensitive filesystems (Linux) if not consistent.
**Fix:** Enforce lowercase handles globally during `init` and `send`, or treat handles as case-insensitive everywhere.

---

### 3. Code Quality & Best Practices

#### 3.1. Missing Timeout on Lock/Sync
**File:** `internal/fsq/atomic.go`
**Issue:** File operations like `SyncDir` and `os.Rename` are blocking system calls.
**Recommendation:** While not strictly required for a CLI, wrapping operations in a context or having a global timeout (like the 5m in golangci-lint) prevents the CLI from hanging indefinitely on a bad NFS mount.

#### 3.2. Hardcoded Permissions
**File:** Various
**Issue:** `0o755` and `0o644` are hardcoded.
**Recommendation:** This assumes a specific umask/group setup. It is generally safe for personal tools, but for a shared "agent queue," ensuring the group bit is set correctly (which you do) is important. No action needed unless strict multi-user Unix security is a goal.

#### 3.3. Test Coverage Gaps
**File:** `internal/fsq/maildir_test.go`
**Issue:** Tests cover the "Happy Path".
**Missing:**
1.  **Disk Full:** What happens if `Write` fails halfway?
2.  **Permission Denied:** What if `new` is not writable?
3.  **Corrupt JSON:** What does `list` do if a file in `new` is 0 bytes or garbage? (Currently, `runList` returns an error and stops listing entirely, which renders the mailbox unusable).

**Fix for 3.3 (List Robustness):**
In `internal/cli/list.go`, if `format.ReadHeaderFile` fails, log a warning to stderr and **continue** to the next message. Do not block the entire list command due to one bad file.

---

### 4. OSS Readiness

1.  **License:** The repo is missing a `LICENSE` file.
2.  **Documentation:** The `README.md` is excellent.
3.  **Module Path:** `github.com/avivsinai/agent-message-queue`. Ensure this repository exists or the `go mod init` path matches your intended distribution.

### 5. Refactoring Suggestions (Actionable)

#### Fix the List/Read Loop (Robustness)
*In `internal/cli/list.go`:*

```go
// Current
header, err := format.ReadHeaderFile(path)
if err != nil {
    return err // <--- One bad file breaks the whole inbox
}

// Recommended
header, err := format.ReadHeaderFile(path)
if err != nil {
    fmt.Fprintf(os.Stderr, "skipping corrupt message %s: %v\n", entry.Name(), err)
    continue
}
```

#### Atomic Read Logic
*In `internal/cli/read.go`:*

```go
// Recommended Logic
targetPath := path
if box == fsq.BoxNew {
    // Attempt move first
    if err := fsq.MoveNewToCur(root, common.Me, filename); err != nil {
         return fmt.Errorf("failed to mark message as read: %w", err)
    }
    // Update path to point to cur
    targetPath = filepath.Join(root, "agents", common.Me, "inbox", "cur", filename)
}

msg, err := format.ReadMessageFile(targetPath)
// ...
```

### Conclusion
The project is fundamentally sound and well-designed for its specific "local agent IPC" use case. The atomic file operations are handled correctly. The primary risks are the `go 1.25` version and the O(N) scaling issues in `list/thread` commands. Fixing the `list` error handling and the `read` atomicity will make it production-ready for local workloads.