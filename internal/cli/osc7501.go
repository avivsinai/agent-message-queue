//go:build darwin || linux

package cli

import "strings"

// osc7501MaxBody bounds one buffered OSC 7501 body. A longer sequence passes
// through unchanged and changes no state.
const osc7501MaxBody = 64 << 10

// osc7501Prefix starts the body of agy's status sequence:
// ESC ] 7501 ; state=<idle|working|done|blocked>[:kind=<k>][:app=..][:msg=..]
// terminated by BEL or ESC \ (agy with AGY_CLI_OSC7501=on).
const osc7501Prefix = "7501;"

type osc7501State uint8

const (
	osc7501Text osc7501State = iota
	osc7501Esc
	osc7501Body
	osc7501BodyEsc
)

// osc7501Parser scans a terminal output stream for agy's OSC 7501 status
// sequences. It passes every other byte through unchanged, keeps a sequence
// split across writes, and passes each status sequence on once it is read
// whole. Other OSC sequences pass through as soon as their body stops
// matching the prefix, so they are never held back.
type osc7501Parser struct {
	onState func(state, kind string)
	state   osc7501State
	pending []byte // ESC ] and the body bytes seen so far
}

// feed returns the bytes to show for p.
func (p *osc7501Parser) feed(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for _, b := range in {
		switch p.state {
		case osc7501Text:
			if b == 0x1b {
				p.state = osc7501Esc
				p.pending = append(p.pending[:0], b)
				continue
			}
			out = append(out, b)
		case osc7501Esc:
			if b == ']' {
				p.state = osc7501Body
				p.pending = append(p.pending, b)
				continue
			}
			out = p.flush(out)
			if b == 0x1b {
				p.state = osc7501Esc
				p.pending = append(p.pending[:0], b)
				continue
			}
			out = append(out, b)
		case osc7501Body:
			body := len(p.pending) - 2
			switch {
			case b == 0x07:
				out = p.finish(out, b)
			case b == 0x1b:
				p.state = osc7501BodyEsc
				p.pending = append(p.pending, b)
			case body < len(osc7501Prefix) && b != osc7501Prefix[body], body >= osc7501MaxBody:
				p.pending = append(p.pending, b)
				out = p.flush(out)
			default:
				p.pending = append(p.pending, b)
			}
		case osc7501BodyEsc:
			p.pending = append(p.pending, b)
			if b == '\\' {
				out = p.finish(out, 0)
				continue
			}
			out = p.flush(out)
		}
	}
	return out
}

// flush passes the held bytes through unchanged.
func (p *osc7501Parser) flush(out []byte) []byte {
	out = append(out, p.pending...)
	p.pending = p.pending[:0]
	p.state = osc7501Text
	return out
}

// finish handles one complete sequence; bel is its BEL terminator, or 0 for
// an ESC \ already in pending.
func (p *osc7501Parser) finish(out []byte, bel byte) []byte {
	if bel != 0 {
		p.pending = append(p.pending, bel)
	}
	end := len(p.pending) - 1
	if bel == 0 {
		end--
	}
	body := string(p.pending[2:end])
	rest, ok := strings.CutPrefix(body, osc7501Prefix)
	if !ok || !strings.HasPrefix(rest, "state=") {
		// The query (7501;?) and anything else pass through.
		return p.flush(out)
	}
	var state, kind string
	for i, field := range strings.Split(rest, ":") {
		key, value, _ := strings.Cut(field, "=")
		switch {
		case i == 0:
			state = value
		case key == "kind":
			kind = value
		}
	}
	if p.onState != nil {
		p.onState(state, kind)
	}
	return p.flush(out)
}
