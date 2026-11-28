package loginservice

import (
	"bytes"
	"encoding/xml"
	"strings"
)

// The unit renderers. They reproduce amq-remote's unit bytes exactly — the
// eight fixtures under testdata were rendered by the code they replace — so
// an upgrade never turns an installed service stale.

// plist is the launchd user agent. It starts at load (login) and is
// restarted only when the service exits with a failure. The unit bytes are
// amq-remote's established bytes, moved with optional working-directory and
// log settings for its other caller. A service with a working directory
// selects its project there; a service with a file log uses it for both
// standard streams.
func (u Unit) plist() []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<!-- " + u.Marker + " -->\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	plistString(&b, "\t", "Label", u.Label)
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range append([]string{u.Exe}, u.Args...) {
		b.WriteString("\t\t<string>")
		_ = xml.EscapeText(&b, []byte(arg))
		b.WriteString("</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n")
	if u.Dir != "" {
		plistString(&b, "\t", "WorkingDirectory", u.Dir)
	}
	if u.LogPath != "" {
		plistString(&b, "\t", "StandardOutPath", u.LogPath)
		plistString(&b, "\t", "StandardErrorPath", u.LogPath)
	}
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	plistString(&b, "\t\t", "PATH", PATH(u.Exe))
	b.WriteString("\t</dict>\n</dict>\n</plist>\n")
	return b.Bytes()
}

func plistString(b *bytes.Buffer, indent, key, value string) {
	b.WriteString(indent + "<key>")
	_ = xml.EscapeText(b, []byte(key))
	b.WriteString("</key>\n" + indent + "<string>")
	_ = xml.EscapeText(b, []byte(value))
	b.WriteString("</string>\n")
}

// systemd is the systemd user unit. It starts with the user's session and is
// restarted only when the service exits with a failure; its output goes to
// the journal. The unit bytes are amq-remote's established bytes, moved
// unchanged. The optional WorkingDirectory follows systemd.exec(5), and the
// board uses it to select the project. systemd.exec(5): “Sets the working
// directory for executed processes.” It reads one path, with specifiers expanded.
func (u Unit) systemd() []byte {
	var b strings.Builder
	b.WriteString("# " + u.Marker + "\n")
	b.WriteString("[Unit]\nDescription=" + specifiers(u.Description) + "\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("ExecStart=" + quoteWord(u.Exe))
	for _, arg := range u.Args {
		b.WriteString(" " + quoteWord(arg))
	}
	b.WriteString("\n")
	if u.Dir != "" {
		dir := u.Dir
		// A final space is stripped and a final backslash continues the
		// next line. A trailing slash names the same directory.
		if strings.HasSuffix(dir, " ") || strings.HasSuffix(dir, `\`) {
			dir += "/"
		}
		b.WriteString("WorkingDirectory=" + specifiers(dir) + "\n")
	}
	b.WriteString("Environment=" + quoteValue("PATH="+PATH(u.Exe)) + "\n")
	b.WriteString("Restart=on-failure\nRestartSec=10\n\n")
	b.WriteString("[Install]\nWantedBy=default.target\n")
	return []byte(b.String())
}

// quoteWord writes one ExecStart word. Only ASCII letters, digits and
// hyphens stand bare: systemd needs no escaping for them, and this preserves
// amq-remote's existing unit bytes. Everything else, including an empty word,
// is one quoted word, with specifiers and variable expansion escaped.
func quoteWord(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return r != '-' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}) == -1 {
		return s
	}
	return quoteReplace(strings.ReplaceAll(specifiers(s), "$", "$$"))
}

// quoteValue quotes a value of a quoted assignment such as Environment=,
// where systemd expands specifiers but not variables.
func quoteValue(s string) string { return quoteReplace(specifiers(s)) }

func quoteReplace(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// specifiers doubles %, the specifier character systemd expands in every
// unit setting.
func specifiers(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// plistSettings reads the argument array and the two top-level path settings.
func plistSettings(raw []byte) (args []string, dir, logPath string) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	key := ""
	depth := 0
	for {
		token, err := dec.Token()
		if err != nil {
			return
		}
		switch e := token.(type) {
		case xml.StartElement:
			switch e.Name.Local {
			case "dict":
				depth++
			case "key":
				if depth == 1 {
					_ = dec.DecodeElement(&key, &e)
				}
			case "array":
				if depth == 1 && key == "ProgramArguments" {
					var v struct {
						Words []string `xml:"string"`
					}
					if dec.DecodeElement(&v, &e) == nil {
						args = v.Words
					}
					key = ""
				}
			case "string":
				if depth == 1 {
					switch key {
					case "WorkingDirectory":
						_ = dec.DecodeElement(&dir, &e)
					case "StandardOutPath":
						_ = dec.DecodeElement(&logPath, &e)
					}
					key = ""
				}
			}
		case xml.EndElement:
			if e.Name.Local == "dict" {
				depth--
			}
		}
	}
}

// systemdExecArgs is the ExecStart command of a unit this package wrote,
// with the quoting undone.
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
