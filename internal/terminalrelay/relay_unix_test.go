//go:build darwin || linux

package terminalrelay

import (
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case HelperCommand:
			os.Exit(RunHelper(os.Args[2:]))
		case "relay-target":
			if _, err := term.MakeRaw(int(os.Stdin.Fd())); err != nil {
				os.Exit(1)
			}
			_ = syscall.Kill(os.Getpid(), syscall.SIGTSTP)
			_, _ = os.Stdout.WriteString("resumed target\n")
			var data [1]byte
			if _, err := io.ReadFull(os.Stdin, data[:]); err != nil {
				os.Exit(1)
			}
			_, _ = os.Stdout.WriteString("raw byte " + string(data[:]) + "\n")
			os.Exit(23)
		case "relay-outer":
			code, err := Run(os.Args[0], []string{os.Args[0], "relay-target"}, os.Environ(), os.Stdout)
			if err != nil {
				_, _ = os.Stderr.WriteString(err.Error())
				os.Exit(1)
			}
			os.Exit(code)
		}
	}
	os.Exit(m.Run())
}

// P4 pre-merge review, agent-message-queue-feh (relay stop/resume): a direct
// PTY child is an orphaned group, so SIGTSTP was discarded. This real process
// boundary also checks the outer terminal is restored before the shell sees
// the stop; parser/unit tests cannot exercise kernel job control.
func TestTargetStopRestoresTerminalAndResumes(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = master.Close() }()
	defer func() { _ = slave.Close() }()
	initial, err := term.GetState(int(master.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "relay-outer")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	var waitDone chan struct{}
	defer func() {
		_ = cmd.Process.Kill()
		if waitDone == nil {
			_ = cmd.Wait()
		} else {
			<-waitDone
		}
	}()
	output := make(chan string, 1)
	ready := make(chan struct{})
	go func() {
		var text strings.Builder
		buf := make([]byte, 1024)
		announced := false
		for {
			n, err := master.Read(buf)
			text.Write(buf[:n])
			if !announced && strings.Contains(text.String(), "resumed target") {
				close(ready)
				announced = true
			}
			if err != nil {
				output <- text.String()
				return
			}
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(cmd.Process.Pid, &status, syscall.WUNTRACED|syscall.WNOHANG, nil)
		if err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			if status&0x7f != 0x7f {
				t.Fatalf("relay exited instead of stopping: %v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("target SIGTSTP did not stop relay")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopped, err := term.GetState(int(master.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(initial, stopped) {
		t.Fatal("outer terminal was not restored before stop")
	}
	if err := cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("target did not resume")
	}
	if _, err := master.Write([]byte("Q")); err != nil {
		t.Fatal(err)
	}
	waitDone = make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
		if waitErr == nil || cmd.ProcessState.ExitCode() != 23 {
			t.Fatalf("target status not preserved: %v", waitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay did not resume and exit")
	}
	select {
	case data := <-output:
		if !strings.Contains(data, "raw byte Q") {
			t.Fatalf("missing resumed raw byte echo: %q", data)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("relay output stayed open")
	}
}
