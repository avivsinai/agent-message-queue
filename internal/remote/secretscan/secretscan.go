// Package secretscan tells whether text a remote surface would show may
// hold a secret, so the surface can show a notice instead. It is
// deliberately broad: a false positive costs only a preview.
package secretscan

import (
	"regexp"
	"strings"
	"unicode"
)

// secretShapes detect secret values by their shape: known token prefixes,
// private keys, bearer or basic credentials, and credentials in a URL.
var secretShapes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)PRIVATE KEY`),
	regexp.MustCompile(`(gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,}|xox[abprs]-[A-Za-z0-9-]{8,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{30,}|glpat-[A-Za-z0-9_-]{16,})`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.`),
	regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`://[^/\s:@]+:[^/\s@]*@`),
}

// sk- tokens are found by context. skAtWordStart is sk- at the start of the
// text or after a character that is no letter or digit. skInOptions is sk-
// inside a word that starts with a single dash, whatever stands between the
// dash and sk- (-usk-…, -vusk-…, -4usk-…, -#usk-…). Both take 16 or more
// token characters. Anywhere else sk- inside a word is part of a name, such
// as task- or risk-, and stays visible: names almost never start with "-".
var (
	skAtWordStart = regexp.MustCompile(`(?:^|[^A-Za-z0-9])sk-[A-Za-z0-9_-]{16,}`)
	skInOptions   = regexp.MustCompile(`(?:^|[\s'"=;|&(])-[^\s'"-][^\s'"]*sk-[A-Za-z0-9_-]{16,}`)
)

// hasSKToken reports an sk- token by its context.
func hasSKToken(t string) bool {
	return skAtWordStart.MatchString(t) || skInOptions.MatchString(t)
}

// secretName is a name that may hold a secret, matched anywhere in a JSON
// key, an assignment identifier or a flag, in any case. credentialFlag is
// a flag whose value is a credential although its name is common, so it is
// a flag rule only and never matched in a JSON key.
var (
	secretName     = regexp.MustCompile(`(?i)(passw|passphrase|pwd|secret|token|key|auth|credential|bearer|private|cookie|session[_-]?id)`)
	credentialFlag = regexp.MustCompile(`(?i)^(user|passphrase)$`)
)

// Names in text: an assignment identifier (NAME=, NAME:) and a flag
// (--name, -name). They are also read with quotes and backslashes removed,
// so --pass"word" and pass\word read as the names they spell.
var (
	assignName = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_.-]*)\s*[=:]`)
	flagName   = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])--?([A-Za-z][A-Za-z0-9_-]*)`)
	unquote    = strings.NewReplacer(`"`, "", `'`, "", `\`, "", "`", "")
)

// MayHold reports whether text may show a secret: a secret shape, or
// a secret-looking assignment or flag name. Every Unicode space is read as
// a space, and the text is checked again with quotes and backslashes
// removed. It is deliberately broad: a false positive costs only the
// preview, and the owner can still block the call.
func MayHold(text string) bool {
	spaced := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, text)
	for _, t := range []string{spaced, unquote.Replace(spaced)} {
		if hasSKToken(t) {
			return true
		}
		for _, re := range secretShapes {
			if re.MatchString(t) {
				return true
			}
		}
		for _, re := range []*regexp.Regexp{assignName, flagName} {
			for _, m := range re.FindAllStringSubmatch(t, -1) {
				if secretName.MatchString(m[1]) || re == flagName && credentialFlag.MatchString(m[1]) {
					return true
				}
			}
		}
	}
	return false
}

// NameMayHold reports whether a key or field name may name a secret, with
// quotes and backslashes removed.
func NameMayHold(name string) bool {
	return secretName.MatchString(unquote.Replace(name))
}

// DisplayForm is text as the DM shows it: control and format characters
// other than newline and tab removed, as the carrier removes them. Screen
// this form too: a removed character can hide a secret from MayHold.
func DisplayForm(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
			return r
		}
		return -1
	}, s)
}
