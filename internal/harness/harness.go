// Package harness holds the facts AMQ knows about each agent harness binary:
// its stable name, how coop exec names its session, how resume flags read,
// and how a wake types into it. It is the one table keyed by executable
// basename. Recognizing a harness never implies that managed launch has an
// adapter for it.
package harness

import (
	"path/filepath"
	"strings"
)

// Stable harness names: the discovery identity of a harness, independent of
// the provider names launch.json and launch records store.
const (
	Claude = "claude"
	Codex  = "codex"
	Cursor = "cursor"
	Grok   = "grok"
	Pi     = "pi"
	Agy    = "agy"
	// Other is any program not in the table. It gets generic behavior,
	// never a guess from the handle or from argument text.
	Other = "other"
)

// ResumeSyntax is how a harness's argv asks to resume a conversation.
type ResumeSyntax uint8

const (
	ResumeSyntaxFlags ResumeSyntax = iota
	ResumeSyntaxCodex
	ResumeSyntaxCursor
)

// NamingMode is how coop exec can name the spawned harness session.
type NamingMode int

const (
	NamingOff NamingMode = iota
	// NamingArgv: a --name argument before the harness's own arguments.
	NamingArgv
	// NamingTUI: a rename command typed into the running TUI.
	NamingTUI
	// NamingUnknown: no known way to name the session.
	NamingUnknown
)

// Wake typing modes, the same values amq wake --inject-mode accepts.
const (
	WakeInjectRaw   = "raw"
	WakeInjectPaste = "paste"
)

// Facts is everything AMQ relies on about one harness.
type Facts struct {
	Name   string
	Naming NamingMode
	Resume ResumeSyntax
	// WakeInjectMode is how a wake types into the harness when nobody chose
	// a mode. Claude Code's and Codex's Ink input coalesces the Enter with a
	// bracketed-paste end and drops it, so they get raw.
	WakeInjectMode string
	// AlternateScreen: the harness owns the terminal screen, so a wake keeps
	// plain diagnostics and attention text off it.
	AlternateScreen bool
	// SubmitPrelude is typed between a raw notification and its Enter.
	// codex-tui maps a raw LF to Ctrl-J, which flushes paste-burst state so
	// the later CR submits.
	SubmitPrelude string
}

var known = map[string]Facts{
	Claude: {Name: Claude, Naming: NamingArgv, Resume: ResumeSyntaxFlags, WakeInjectMode: WakeInjectRaw, AlternateScreen: true},
	Codex:  {Name: Codex, Naming: NamingTUI, Resume: ResumeSyntaxCodex, WakeInjectMode: WakeInjectRaw, AlternateScreen: true, SubmitPrelude: "\n"},
	Cursor: {Name: Cursor, Naming: NamingTUI, Resume: ResumeSyntaxCursor, WakeInjectMode: WakeInjectPaste},
	Grok:   {Name: Grok, Naming: NamingUnknown, Resume: ResumeSyntaxFlags, WakeInjectMode: WakeInjectPaste},
	Pi:     {Name: Pi, Naming: NamingArgv, Resume: ResumeSyntaxFlags, WakeInjectMode: WakeInjectPaste},
	Agy:    {Name: Agy, Naming: NamingUnknown, Resume: ResumeSyntaxFlags, WakeInjectMode: WakeInjectPaste},
}

// generic is the behavior for Other: paste typing, plain output, no prelude.
var generic = Facts{Name: Other, Naming: NamingUnknown, Resume: ResumeSyntaxFlags, WakeInjectMode: WakeInjectPaste}

// aliases maps executable basenames that differ from the stable name. Cursor's
// current CLI is "agent"; "cursor-agent" is its legacy executable.
var aliases = map[string]string{"agent": Cursor, "cursor-agent": Cursor}

// ForExecutable returns the facts for an executable path or basename, with
// case and a trailing .exe ignored. ok is false for a program not in the
// table, which gets the generic Other facts.
func ForExecutable(executable string) (Facts, bool) {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(executable)), ".exe")
	if name, ok := aliases[base]; ok {
		base = name
	}
	facts, ok := known[base]
	if !ok {
		return generic, false
	}
	return facts, true
}

// Lookup returns the facts for a stable harness name, as a wake receives it
// with --harness. An unknown name gets the generic Other facts.
func Lookup(name string) Facts {
	if facts, ok := known[name]; ok {
		return facts
	}
	return generic
}
