// Package loginservice names the amq-remote login service and reads its unit
// file. amq-remote installs the unit; amq reads it to find the binary it runs.
package loginservice

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	Label  = "io.github.avivsinai.amq-remote"
	Unit   = "amq-remote.service"
	Marker = "written by amq-remote service install"
)

// Path is the unit file: ~/Library/LaunchAgents/<label>.plist on
// macOS, $XDG_CONFIG_HOME/systemd/user/amq-remote.service on Linux.
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", Label+".plist"), nil
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "systemd", "user", Unit), nil
}

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
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Installed{}, nil
	}
	if err != nil {
		return Installed{}, err
	}
	if !bytes.Contains(raw, []byte(Marker)) {
		return Installed{}, fmt.Errorf("%s is not an amq-remote login service; amq-remote service install leaves it alone", path)
	}
	st := Installed{Owned: true, Raw: raw}
	var args []string
	if runtime.GOOS == "darwin" {
		args = plistArguments(raw)
	} else {
		args = systemdExecArgs(raw)
	}
	if len(args) == 4 && args[1] == "up" && args[2] == "--root" {
		st.Exe, st.Root = args[0], args[3]
	}
	return st, nil
}

// plistArguments is the ProgramArguments array of a plist this command wrote.
func plistArguments(raw []byte) []string {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	var args []string
	key, inArgs := "", false
	for {
		tok, err := dec.Token()
		if err != nil {
			return args
		}
		switch t := tok.(type) {
		case xml.StartElement:
			var text string
			switch t.Name.Local {
			case "key":
				if dec.DecodeElement(&text, &t) == nil {
					key = text
				}
			case "array":
				inArgs = key == "ProgramArguments"
			case "string":
				if dec.DecodeElement(&text, &t) == nil && inArgs {
					args = append(args, text)
				}
			}
		case xml.EndElement:
			if t.Name.Local == "array" && inArgs {
				return args
			}
		}
	}
}

// systemdExecArgs is the ExecStart command of a unit this command wrote, with
// systemdQuote undone.
func systemdExecArgs(raw []byte) []string {
	for _, line := range strings.Split(string(raw), "\n") {
		cmd, ok := strings.CutPrefix(line, "ExecStart=")
		if !ok {
			continue
		}
		var args []string
		for cmd != "" {
			cmd = strings.TrimLeft(cmd, " ")
			if !strings.HasPrefix(cmd, `"`) {
				word, rest, _ := strings.Cut(cmd, " ")
				args, cmd = append(args, word), rest
				continue
			}
			var w strings.Builder
			i := 1
			for ; i < len(cmd) && cmd[i] != '"'; i++ {
				if cmd[i] == '\\' && i+1 < len(cmd) {
					i++
				}
				w.WriteByte(cmd[i])
			}
			args = append(args, strings.NewReplacer("%%", "%", "$$", "$").Replace(w.String()))
			cmd = cmd[min(i+1, len(cmd)):]
		}
		return args
	}
	return nil
}
