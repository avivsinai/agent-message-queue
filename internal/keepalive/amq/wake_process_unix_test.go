//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package amq

import (
	"bytes"
	"os/exec"
	"testing"
)

func TestConfigureWakeProcessDetachesSessionAndNullsStdio(t *testing.T) {
	cmd := exec.Command("ignored")
	cmd.Stdin = bytes.NewBufferString("held stdin")
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	configureWakeProcess(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("SysProcAttr = %#v, want Setsid", cmd.SysProcAttr)
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatalf("wake retained stdio: stdin=%T stdout=%T stderr=%T", cmd.Stdin, cmd.Stdout, cmd.Stderr)
	}
}
