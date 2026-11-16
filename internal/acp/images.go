package acp

import (
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"fiatjaf.com/nostr"
)

// eventMedia is one file of a Buzz event: its URL and what its imeta tag
// declares (empty or zero when undeclared).
type eventMedia struct {
	url  string
	mime string
	sha  string
	size int64
}

// imetaMedia reads one NIP-92 imeta tag; ok is false for any other tag and
// for one with no url.
func imetaMedia(t []string) (m eventMedia, ok bool) {
	if len(t) < 2 || t[0] != "imeta" {
		return m, false
	}
	for _, field := range t[1:] {
		key, value, _ := strings.Cut(field, " ")
		value = strings.TrimSpace(value)
		switch key {
		case "url":
			m.url = value
		case "m":
			m.mime = strings.ToLower(value)
		case "x":
			m.sha = strings.ToLower(value)
		case "size":
			m.size, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	return m, m.url != ""
}

// unfetchedImages returns prompt with one `[image not attached: ...]` line
// per file of the owner's event evt: a standalone machine agent does not
// download media.
func unfetchedImages(evt nostr.Event, prompt string) string {
	var lines []string
	seen := map[string]bool{}
	for _, t := range evt.Tags {
		if m, ok := imetaMedia(t); ok && !seen[m.url] {
			seen[m.url] = true
			lines = append(lines, "[image not attached: "+oneLine(mediaName(m.url))+": amq-remote serve does not download images yet]")
		}
	}
	if len(lines) == 0 {
		return prompt
	}
	return strings.TrimRight(prompt, "\n") + "\n\n" + strings.Join(lines, "\n")
}

// mediaName is the last path segment of a media URL, for a refusal line.
func mediaName(raw string) string {
	if u, err := url.Parse(raw); err == nil && path.Base(u.Path) != "/" && path.Base(u.Path) != "." {
		return path.Base(u.Path)
	}
	return raw
}

// oneLine keeps refusal text on one short line, cut on a rune boundary.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		cut := 200
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}
