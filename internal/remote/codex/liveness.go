package codex

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sync"
	"syscall"

	"github.com/avivsinai/agent-message-queue/internal/launch"
	"github.com/avivsinai/agent-message-queue/internal/remote/binding"
)

// View asks each app-server daemon at most once: one daemon pid and one
// loaded-thread list per socket, reused by every binding inspected through
// it. A caller makes a new View for each pass, so an answer is never older
// than that pass. It is read-only and safe for concurrent use.
type View struct {
	loadedOf func(sock string) ([]string, error)
	pidOf    func(sock string) (int, error)

	mu     sync.Mutex
	pids   map[string]answer[int]
	loaded map[string]answer[[]string]
}

type answer[T any] struct {
	v   T
	err error
}

// NewView is a View that asks a daemon with loaded (thread/loaded/list) and
// pid (the peer process of its socket).
func NewView(loaded func(sock string) ([]string, error), pid func(sock string) (int, error)) *View {
	return &View{loadedOf: loaded, pidOf: pid, pids: map[string]answer[int]{}, loaded: map[string]answer[[]string]{}}
}

// DaemonPID is the pid of the daemon process serving sock.
func (v *View) DaemonPID(sock string) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	a, ok := v.pids[sock]
	if !ok {
		a.v, a.err = v.pidOf(sock)
		v.pids[sock] = a
	}
	return a.v, a.err
}

// LoadedThreads is the threads the daemon serving sock has loaded.
func (v *View) LoadedThreads(sock string) ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	a, ok := v.loaded[sock]
	if !ok {
		a.v, a.err = v.loadedOf(sock)
		v.loaded[sock] = a
	}
	return a.v, a.err
}

// Liveness is a binding's liveness. A Codex thread's comes from the
// app-server daemon that had it loaded when it was bound: the thread has
// ended only when that same daemon process, still running, no longer has it
// loaded. A restarted or other daemon, or an unreachable one, proves nothing,
// so the thread is unknown. A binding without a thread is b.Liveness.
func (v *View) Liveness(b binding.Binding) (binding.State, string) {
	state, evidence := b.Liveness()
	if state == binding.Ended || b.AppServer.Thread == "" {
		return state, evidence
	}
	loaded, err := v.LoadedThreads(b.AppServer.Socket)
	if err != nil {
		return binding.Unknown, fmt.Sprintf("app-server unreachable: %v", err)
	}
	if slices.Contains(loaded, b.AppServer.Thread) {
		return binding.Live, "thread is loaded in the app-server"
	}
	recorded := b.AppServer.Daemon
	if recorded.PID <= 0 {
		return binding.Unknown, "thread is not loaded, and no app-server process was recorded to compare"
	}
	if pid, err := v.DaemonPID(b.AppServer.Socket); err != nil || pid != recorded.PID {
		return binding.Unknown, "thread is not loaded in another app-server process than the one it was bound in"
	}
	if st, ev := launch.InspectHolder(recorded); st != launch.LeaseValid {
		return binding.Unknown, "thread is not loaded, and the app-server process is not confirmed: " + ev
	}
	return binding.Ended, "thread is no longer loaded in the app-server it was bound in"
}

// Absent reports whether b's thread is confirmed not running at its socket:
// no daemon listens there (the socket path is gone or refuses the
// connection), or the daemon that answers there does not have it loaded.
// Any other failed probe, such as a timeout or a protocol error, proves
// nothing, so it is not absence.
func (v *View) Absent(b binding.Binding) bool {
	if b.AppServer.Thread == "" {
		return false
	}
	loaded, err := v.LoadedThreads(b.AppServer.Socket)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
	}
	return !slices.Contains(loaded, b.AppServer.Thread)
}
