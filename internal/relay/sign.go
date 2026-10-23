package relay

import (
	"slices"
	"strings"

	"fiatjaf.com/nostr"
)

// Sign signs evt after it replaces each C0 control other than tab, newline
// and carriage return in the content and tag values with U+FFFD. The
// library's id commitment writes those bytes verbatim, while NIP-01 clients
// escape them, so an event that holds one would have a different id for
// them. Without them both serializers commit the same bytes. A changed tag
// is copied, so a slice the caller still holds is never written.
func Sign(evt *nostr.Event, secret [32]byte) error {
	evt.Content = replaceC0(evt.Content)
	for i, tag := range evt.Tags {
		if !slices.ContainsFunc(tag, hasC0) {
			continue
		}
		clean := make(nostr.Tag, len(tag))
		for j, v := range tag {
			clean[j] = replaceC0(v)
		}
		evt.Tags[i] = clean
	}
	return evt.Sign(secret)
}

func hasC0(s string) bool { return strings.IndexFunc(s, isUnescapedC0) >= 0 }

// replaceC0 changes only those bytes: a C0 byte is never part of a multibyte
// UTF-8 sequence, and every other byte is kept as is.
func replaceC0(s string) string {
	if !hasC0(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		if isUnescapedC0(rune(s[i])) {
			b.WriteRune('�')
		} else {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func isUnescapedC0(r rune) bool { return r < 0x20 && r != '\t' && r != '\n' && r != '\r' }
