//go:build windows

package claude

// Windows has no O_NOFOLLOW or O_NONBLOCK on os.OpenFile. openRegular's
// same-file check (os.SameFile between the lstat gate and the opened
// description) is what refuses a symlink swapped in after the gate. A
// blocking-open FIFO does not arise: Windows named pipes live in the
// \\.\pipe\ namespace, not at a path under the user's home.
const openNoFollowFlag = 0
