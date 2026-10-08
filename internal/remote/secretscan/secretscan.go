// Package secretscan tells whether text a remote surface would show may
// hold a secret, so the surface can show a notice instead. It is
// deliberately broad: a false positive costs only a preview.
package secretscan

import (
	"regexp"
	"strconv"
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

// curl's -u and -U always take user:password, so a curl command with
// either, alone, in a short-option cluster or attached, may show one,
// whatever its value looks like (Pro review of #988: value shapes did not
// converge). Other tools' -u, as in git push -u, stays visible.
var (
	curlCommand = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])curl(?:$|[^A-Za-z0-9_.-])`)
	curlUserOpt = regexp.MustCompile(`(?:^|[\s'"=;|&(])-[^\s'"-]*[uU]`)
)

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

// A shell rebuilds words before it runs them, so the text is also screened
// with ANSI-C quoting ($'...') decoded, and a command whose command word or
// an option word still holds an expansion ($(...), backquotes, ${...},
// $name) is hidden: what it runs cannot be read from the text (Pro review of
// #988 r6: curl $'\x2du' user:pass, c$'u'rl -u, curl -$(printf u)).
// Screening never runs anything.
var (
	ansiCQuote     = regexp.MustCompile(`\$'((?:[^'\\]|\\.)*)'`)
	shellExpansion = regexp.MustCompile("\\$\\(|\\$\\{|\\$[A-Za-z_]|`")
	commandBreak   = regexp.MustCompile(`[;&|(]$`)
)

// decodeANSIC replaces each $'...' with the text bash makes of it.
func decodeANSIC(s string) string {
	return ansiCQuote.ReplaceAllStringFunc(s, func(m string) string {
		body := m[2 : len(m)-1]
		var b strings.Builder
		for i := 0; i < len(body); i++ {
			if body[i] != '\\' || i+1 == len(body) {
				b.WriteByte(body[i])
				continue
			}
			i++
			switch c := body[i]; c {
			case 'a':
				b.WriteByte('\a')
			case 'b':
				b.WriteByte('\b')
			case 'e', 'E':
				b.WriteByte(0x1b)
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'v':
				b.WriteByte('\v')
			case 'x', 'u', 'U':
				limit := map[byte]int{'x': 2, 'u': 4, 'U': 8}[c]
				j := i + 1
				for j < len(body) && j-i-1 < limit && strings.IndexByte("0123456789abcdefABCDEF", body[j]) >= 0 {
					j++
				}
				if j == i+1 {
					b.WriteByte('\\')
					b.WriteByte(c)
					continue
				}
				n, _ := strconv.ParseUint(body[i+1:j], 16, 32)
				if c == 'x' {
					b.WriteByte(byte(n))
				} else {
					b.WriteRune(rune(n))
				}
				i = j - 1
			case '0', '1', '2', '3', '4', '5', '6', '7':
				j := i
				for j < len(body) && j-i < 3 && body[j] >= '0' && body[j] <= '7' {
					j++
				}
				n, _ := strconv.ParseUint(body[i:j], 8, 16)
				b.WriteByte(byte(n))
				i = j - 1
			default:
				b.WriteByte(c)
			}
		}
		return b.String()
	})
}

// expandsCommandOrOption reports a command word or an option word that holds
// a shell expansion.
func expandsCommandOrOption(t string) bool {
	words := strings.Fields(t)
	for i, w := range words {
		head := i == 0 || commandBreak.MatchString(words[i-1])
		if (head || strings.HasPrefix(w, "-")) && shellExpansion.MatchString(w) {
			return true
		}
	}
	return false
}

// secretName is a name that may hold a secret, matched anywhere in a JSON
// key, an assignment identifier or a flag, in any case. credentialFlag is
// a flag whose value is a credential although its name is common, so it is
// a flag rule only and never matched in a JSON key.
var (
	secretName     = regexp.MustCompile(`(?i)(passw|passphrase|pwd|secret|token|key|auth|credential|bearer|private|cookie|session[_-]?id)`)
	credentialFlag = regexp.MustCompile(`(?i)^(user|proxy-user|passphrase)$`)
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
	spaced := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return ' '
			}
			return r
		}, s)
	}
	// The shell joins a backslash-newline before it splits words, so the
	// text is also read joined: curl -\<newline>u runs as curl -u (Pro
	// review of #988).
	joined := spaced(strings.ReplaceAll(text, "\\\n", ""))
	if expandsCommandOrOption(joined) {
		return true
	}
	decoded := decodeANSIC(joined)
	for _, t := range []string{spaced(text), unquote.Replace(spaced(text)), joined, unquote.Replace(joined), decoded, unquote.Replace(decoded)} {
		if hasSKToken(t) || curlCommand.MatchString(t) && curlUserOpt.MatchString(t) {
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
