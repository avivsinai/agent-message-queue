package activity

import (
	"time"
	"unicode/utf8"
)

// coalesceWindow is how long a run of deltas for one item stays open
// before it becomes one frame. One streaming item then costs at most ten
// frames a second, a tenth of the per-body cap of ratePerBody, so tool,
// plan and lifecycle frames and a few parallel streams still fit. The relay
// drains every 250 ms, so in practice a run closes on that tick.
const coalesceWindow = 100 * time.Millisecond

// streamOutputCap bounds the command output kept per open command. Text at
// this size never fits one relay frame, so oneFrame still shows it as a
// truncated prefix.
const streamOutputCap = maxPlainBytes

// streamRun is one open run of deltas: agent text for one message, or the
// output of one command.
type streamRun struct {
	obs   observation
	since time.Time
}

// commandOutput is the output a command has streamed so far. Desktop
// replaces a tool result on each update, so every update carries all of it.
type commandOutput struct {
	title string
	out   []byte
	full  bool
}

// add appends a delta up to streamOutputCap. It reports whether the text
// changed.
func (c *commandOutput) add(delta string) bool {
	if c.full {
		return false
	}
	piece := capText(delta, streamOutputCap-len(c.out))
	c.out = append(c.out, piece...)
	c.full = len(piece) < len(delta)
	return piece != ""
}

// streamState is the Codex stream state of one sink, guarded by Sink.mu.
type streamState struct {
	runs []*streamRun
	// streamed holds agentMessage items that sent deltas: their completed
	// item repeats the text, so it is not queued again.
	streamed map[string]bool
	commands map[string]*commandOutput
}

func (st *streamState) command(id string) *commandOutput {
	if st.commands == nil {
		st.commands = map[string]*commandOutput{}
	}
	c := st.commands[id]
	if c == nil {
		c = &commandOutput{title: id}
		st.commands[id] = c
	}
	return c
}

// textDelta extends the last run when it is text for the same message, and
// opens a new run otherwise, so the text keeps its order.
func (st *streamState) textDelta(obs observation, now time.Time) {
	if st.streamed == nil {
		st.streamed = map[string]bool{}
	}
	st.streamed[obs.ItemID] = true
	if n := len(st.runs); n > 0 {
		last := st.runs[n-1]
		if last.obs.Update == obs.Update && last.obs.ItemID == obs.ItemID {
			last.obs.Text += obs.Text
			return
		}
	}
	st.runs = append(st.runs, &streamRun{obs: obs, since: now})
}

// outputDelta records command output. An open run for the same command
// already carries it: the frame takes the whole output when it is built.
func (st *streamState) outputDelta(obs observation, now time.Time) {
	c := st.command(obs.ToolID)
	if !c.add(obs.Text) {
		return
	}
	for _, run := range st.runs {
		if run.obs.Update == obs.Update && run.obs.ToolID == obs.ToolID {
			return
		}
	}
	obs.Title = c.title
	st.runs = append(st.runs, &streamRun{obs: obs, since: now})
}

// flushDueLocked closes the open runs once the oldest one has been open for
// coalesceWindow. The caller holds s.mu.
func (s *Sink) flushDueLocked(now time.Time) error {
	if len(s.stream.runs) == 0 || now.Sub(s.stream.runs[0].since) < coalesceWindow {
		return nil
	}
	return s.flushLocked()
}

// flushLocked queues every open run, oldest first. The caller holds s.mu,
// so no other frame of this sink takes a sequence number in between.
func (s *Sink) flushLocked() error {
	runs := s.stream.runs
	s.stream.runs = nil
	for _, run := range runs {
		obs := run.obs
		if obs.Update == "tool_call_update" {
			if c := s.stream.commands[obs.ToolID]; c != nil {
				obs.Text = string(c.out)
			}
		}
		if err := s.queue(obs); err != nil {
			return err
		}
	}
	return nil
}

// capText returns the longest prefix of s that is at most n bytes and ends
// on a rune boundary.
func capText(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
