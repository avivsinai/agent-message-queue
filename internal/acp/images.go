package acp

import (
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	"fiatjaf.com/nostr"
)

// imetaURL is the url of one NIP-92 imeta tag, "" for any other tag and for
// one with no url.
func imetaURL(t []string) string {
	if len(t) < 2 || t[0] != "imeta" {
		return ""
	}
	url := ""
	for _, field := range t[1:] {
		if key, value, _ := strings.Cut(field, " "); key == "url" {
			url = strings.TrimSpace(value)
		}
	}
	return url
}

// unfetchedImages returns prompt with one `[image not attached: ...]` line
// per file of the owner's event evt: the machine agent does not download
// media.
func unfetchedImages(evt nostr.Event, prompt string) string {
	var lines []string
	seen := map[string]bool{}
	for _, t := range evt.Tags {
		if u := imetaURL(t); u != "" && !seen[u] {
			seen[u] = true
			lines = append(lines, "[image not attached: "+oneLine(mediaName(u))+": amq-remote serve does not download images yet]")
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
