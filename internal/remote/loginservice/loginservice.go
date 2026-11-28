// Package loginservice preserves amq-remote's established service identity
// for its readers: the neutral renderer and manager in internal/loginservice
// carry the code, and amq-board is its other caller.
package loginservice

import neutral "github.com/avivsinai/agent-message-queue/internal/loginservice"

const (
	Label  = "io.github.avivsinai.amq-remote"
	Unit   = "amq-remote.service"
	Marker = "written by amq-remote service install"
)

// Definition supplies the identity shared by the remote reader and installer.
func Definition() neutral.Unit {
	return neutral.Unit{Program: "amq-remote", Label: Label, Name: Unit, Marker: Marker}
}

func Path() (string, error) { return Definition().Path() }

// Installed is what a unit file at the service path says.
type Installed struct {
	Owned bool   // the file exists and service install wrote it
	Exe   string // the binary it runs
	Root  string // the root it runs up for
	Raw   []byte
}

// Read reads the unit at path. It refuses a file service install did
// not write, so install and uninstall never touch another program's unit.
func Read(path string) (Installed, error) {
	st, err := Definition().Installed(path)
	if err != nil {
		return Installed{}, err
	}
	out := Installed{Owned: st.Owned, Raw: st.Raw}
	if len(st.Args) == 4 && st.Args[1] == "up" && st.Args[2] == "--root" {
		out.Exe, out.Root = st.Exe, st.Args[3]
	}
	return out, nil
}
