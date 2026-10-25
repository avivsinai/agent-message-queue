package command

import "testing"

// usage slice review finding 7: output after a complete reply that crossed
// the output bound cleared the reply and failed the run.
func TestWatchKeepsACompleteReply(t *testing.T) {
	w := &watch{max: 8, done: func(out []byte) bool { return string(out) == "ok\n" }, overflow: func() {}, onDone: func() {}}
	_, _ = w.Write([]byte("ok\n"))
	_, _ = w.Write([]byte("trailing noise"))
	out, over, done := w.result()
	if string(out) != "ok\n" || over || !done {
		t.Fatalf("result = %q over=%v done=%v, want the reply kept", out, over, done)
	}
}
