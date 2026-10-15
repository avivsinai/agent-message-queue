package xferv3

import (
	"fmt"
	"strings"
)

const (
	MaxNameBytes      = 63
	MaxDestBytes      = 127
	MaxMessageIDBytes = 200
	MaxThreadIDBytes  = 200
	MaxPayloadBytes   = 8 * 1024 * 1024
	MaxRecordBytes    = 1024
	MaxOfferRecords   = 32
	MaxOfferPageBytes = 48 * 1024
	IDTextLen         = 52
)

var (
	ErrName      = fmt.Errorf("xferv3: name")
	ErrLimit     = fmt.Errorf("xferv3: limit")
	ErrPayload   = fmt.Errorf("xferv3: payload does not match the record")
	ErrSignature = fmt.Errorf("xferv3: signature")
	ErrBadID     = fmt.Errorf("xferv3: id does not match the record")
)

type Host struct{ s string }

type Handle struct{ s string }

type MessageID struct{ s string }

type ThreadID struct{ s string }

type Dest struct {
	host  Host
	agent Handle
}

func ParseHost(s string) (Host, error) {
	if err := parseName(s); err != nil {
		return Host{}, fmt.Errorf("%w: host %q", err, s)
	}
	return Host{s: s}, nil
}

func ParseHandle(s string) (Handle, error) {
	if err := parseName(s); err != nil {
		return Handle{}, fmt.Errorf("%w: handle %q", err, s)
	}
	return Handle{s: s}, nil
}

func ParseMessageID(s string) (MessageID, error) {
	if err := parseMessage(s, MaxMessageIDBytes); err != nil {
		return MessageID{}, fmt.Errorf("%w: message id", err)
	}
	return MessageID{s: s}, nil
}

func ParseThreadID(s string) (ThreadID, error) {
	if err := parseMessage(s, MaxThreadIDBytes); err != nil {
		return ThreadID{}, fmt.Errorf("%w: thread id", err)
	}
	return ThreadID{s: s}, nil
}

func ParseDest(alias string) (Dest, error) {
	if len(alias) == 0 || len(alias) > MaxDestBytes || strings.Count(alias, "/") != 1 {
		return Dest{}, fmt.Errorf("%w: dest %q", ErrName, alias)
	}
	host, agent, _ := strings.Cut(alias, "/")
	h, err := ParseHost(host)
	if err != nil {
		return Dest{}, fmt.Errorf("%w: dest host", ErrName)
	}
	a, err := ParseHandle(agent)
	if err != nil {
		return Dest{}, fmt.Errorf("%w: dest handle", ErrName)
	}
	return Dest{host: h, agent: a}, nil
}

func (d Dest) Host() Host          { return d.host }
func (d Dest) Agent() Handle       { return d.agent }
func (d Dest) Alias() string       { return d.host.s + "/" + d.agent.s }
func (h Host) String() string      { return h.s }
func (h Handle) String() string    { return h.s }
func (m MessageID) String() string { return m.s }
func (t ThreadID) String() string  { return t.s }

func parseName(s string) error {
	if len(s) == 0 || len(s) > MaxNameBytes || s == "." || s == ".." {
		return ErrName
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '.' || b == '_' || b == '-' {
			continue
		}
		return ErrName
	}
	return nil
}

func parseMessage(s string, max int) error {
	if len(s) == 0 || len(s) > max {
		return ErrName
	}
	if s[0] == ' ' || s[len(s)-1] == ' ' {
		return ErrName
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b < 0x20 || b > 0x7e || b == '\\' {
			return ErrName
		}
	}
	return nil
}
