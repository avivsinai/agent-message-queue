//go:build windows

package claude

// Windows has no no-follow, non-blocking open through os.OpenFile, and a
// post-open check cannot undo a create that followed a planted link; on the
// pinned Go, a Windows Lstat does not even capture the file identity that
// os.SameFile later compares (codex #855 r3 item 2). The adapter therefore
// refuses every local-writable file operation on Windows instead of
// claiming a guarantee it cannot give; see openRegular and the Stop
// receiver.
const openNoFollowFlag = 0

// noFollowSupported gates every local-writable file operation.
const noFollowSupported = false
