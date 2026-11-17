package machineagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/amqhome"
	"github.com/avivsinai/agent-message-queue/internal/format"
)

// settingsFile is the owner's settings file below the per-user home.
const settingsFile = "acp.settings"

// Issue 1023 for an owner turn of the machine agent: the next owner message
// runs on the file's turn_timeout. The startup budget here would expire
// before publish; the file's budget lets the turn end on its reply.
func TestOwnerTurnReadsTurnTimeoutFromSettingsFile(t *testing.T) {
	f, b := mailboxRoute(t)
	home := canonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err := amqhome.EnsureDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, settingsFile), []byte(`{"turn_timeout":"2s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f.s.cfg.TurnTimeout = time.Nanosecond
	f.machine(t)
	in := f.ownerSays(t, machineChanA, "hi")
	done := f.s.Owner(in)
	id, prompt := promptOf(t, b)
	reply := replyAs(t, b.Root, prompt.Header.Thread, id, format.KindAnswer, "done")
	waitClosed(t, done)
	if out, _ := f.s.mailboxAnswered(in.Event.ID.Hex()); out.ReplyID != reply || f.posts(machineChanA, "done") != 1 {
		t.Fatalf("outcome=%+v posts=%q; want the turn to run on the file's turn_timeout and end on its reply", out, f.agentTexts(machineChanA))
	}
}
