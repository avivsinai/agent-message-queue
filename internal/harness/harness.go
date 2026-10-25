// Package harness holds the facts AMQ knows about each agent harness binary:
// its stable name, how coop exec names its session, how resume flags read,
// and how a wake types into it. It is the one table keyed by executable
// basename. Recognizing a harness never implies that managed launch has an
// adapter for it.
package harness

import (
	"path/filepath"
	"strings"
	"time"
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
	// PasteSubmitDelay is the least quiet time between a pasted notification
	// and its submit Enter; zero keeps the generic spacing. Cursor's composer
	// types an Enter that follows a paste too closely as a newline, and when
	// text and Enter arrive as one burst it runs the turn without clearing
	// the composer.
	PasteSubmitDelay time.Duration
}

var known = map[string]Facts{
	Claude: {Name: Claude, Naming: NamingArgv, Resume: ResumeSyntaxFlags, WakeInjectMode: WakeInjectRaw, AlternateScreen: true},
	Codex:  {Name: Codex, Naming: NamingTUI, Resume: ResumeSyntaxCodex, WakeInjectMode: WakeInjectRaw, AlternateScreen: true, SubmitPrelude: "\n"},
	Cursor: {Name: Cursor, Naming: NamingTUI, Resume: ResumeSyntaxCursor, WakeInjectMode: WakeInjectPaste, PasteSubmitDelay: 500 * time.Millisecond},
	Grok:   {Name: Grok, Naming: NamingUnknown, Resume: ResumeSyntaxFlags, WakeInjectMode: WakeInjectPaste},
	Pi:     {Name: Pi, Naming: NamingArgv, Resume: ResumeSyntaxFlags, WakeInjectMode: WakeInjectPaste},
	Agy:    {Name: Agy, Naming: NamingUnknown, Resume: ResumeSyntaxFlags, WakeInjectMode: WakeInjectPaste},
}

// generic is the behavior for Other: paste typing, plain output, no prelude.
var generic = Facts{Name: Other, Naming: NamingUnknown, Resume: ResumeSyntaxFlags, WakeInjectMode: WakeInjectPaste}

// executables maps each recognized executable basename to its harness name.
// A harness name is not an executable unless listed: Cursor's CLI is "agent",
// with "cursor-agent" as its legacy executable, and "cursor" is its desktop
// app, not a harness binary.
var executables = map[string]string{
	"claude":       Claude,
	"codex":        Codex,
	"agent":        Cursor,
	"cursor-agent": Cursor,
	"grok":         Grok,
	"pi":           Pi,
	"agy":          Agy,
}

// ForExecutable returns the facts for an executable path or basename, with
// case and a trailing .exe ignored. ok is false for a program not in the
// table, which gets the generic Other facts.
func ForExecutable(executable string) (Facts, bool) {
	base := executableName(executable)
	name, ok := executables[base]
	if !ok {
		return generic, false
	}
	return known[name], true
}

// Lookup returns the facts for a stable harness name, as a wake receives it
// with --harness. An unknown name gets the generic Other facts.
func Lookup(name string) Facts {
	if facts, ok := known[name]; ok {
		return facts
	}
	return generic
}

// acpHost is the amq-acp companion, whose host subcommand serves another
// harness over ACP: amq-acp host [--cwd dir] [--check] [--protocol p] [--] agent ...
const acpHost = "amq-acp"

// Invocation is what one argv runs: the harness that does the work and the
// wrapper executable in front of it, if any.
type Invocation struct {
	Harness string
	Wrapper string
}

// ResolveInvocation names the harness behind executable and its arguments.
// It understands only the supported wrapper grammars (amq-acp host); any
// other wrapper form names the harness Other rather than searching argument
// text for a recognizable name. The terminal behavior of a wrapped
// invocation is the wrapper's, which ForExecutable reports.
func ResolveInvocation(executable string, args []string) Invocation {
	if executableName(executable) != acpHost {
		facts, _ := ForExecutable(executable)
		return Invocation{Harness: facts.Name}
	}
	inv := Invocation{Harness: Other, Wrapper: acpHost}
	if len(args) == 0 || args[0] != "host" {
		return inv
	}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			if i+1 < len(args) {
				facts, _ := ForExecutable(args[i+1])
				inv.Harness = facts.Name
			}
			return inv
		case arg == "--cwd" || arg == "-cwd" || arg == "--protocol" || arg == "-protocol":
			i++
		case strings.HasPrefix(arg, "-"):
		default:
			facts, _ := ForExecutable(arg)
			inv.Harness = facts.Name
			return inv
		}
	}
	return inv
}

// WrapperName is the recorded name of a wrapper executable: its basename
// with case and .exe ignored, or Other when that is not a plain name.
func WrapperName(executable string) string {
	name := executableName(executable)
	if name == "" || strings.HasPrefix(name, "-") || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
		return Other
	}
	return name
}

func executableName(executable string) string {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(executable)), ".exe")
}
