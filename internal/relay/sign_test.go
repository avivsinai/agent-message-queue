package relay_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
)

// nip01Quote writes s as JSON.stringify does, the serializer NIP-01 clients
// such as nostr-tools hash.
func nip01Quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// nip01ID is the event id a NIP-01 client computes.
func nip01ID(evt nostr.Event) string {
	tags := make([]string, len(evt.Tags))
	for i, tag := range evt.Tags {
		vals := make([]string, len(tag))
		for j, v := range tag {
			vals[j] = nip01Quote(v)
		}
		tags[i] = "[" + strings.Join(vals, ",") + "]"
	}
	commitment := "[0," + nip01Quote(evt.PubKey.Hex()) + "," + strconv.FormatInt(int64(evt.CreatedAt), 10) + "," +
		strconv.Itoa(int(evt.Kind)) + ",[" + strings.Join(tags, ",") + "]," + nip01Quote(evt.Content) + "]"
	sum := sha256.Sum256([]byte(commitment))
	return hex.EncodeToString(sum[:])
}

// Regression: #1156, the library's id commitment writes C0 controls other
// than tab, newline and carriage return verbatim, so an event AMQ signed
// with one had another id for NIP-01 clients. An event AMQ signs has the
// id a NIP-01 client computes for it.
func TestSignedEventHasTheNIP01ID(t *testing.T) {
	// The issue's vectors pin the reference serializer.
	pub, err := nostr.PubKeyFromHex("7e7e9c42a91bfef19fa929e5fda1b72e0ebc1a4c1141673e2794234d86addf4e")
	if err != nil {
		t.Fatal(err)
	}
	for content, want := range map[string]string{
		"a\fb":   "c1ce746eee1ef178e988713471e2a1062bed99175770cbb2f29cf7382807278c",
		"a\bb":   "a7d8977f8f6a1240d1bc7542ffc9514b8988d1cb7f8418d091c72f013cd7fe22",
		"a\x01b": "37847e11712fa7d0d1197c382c92b79106f736520ac3ccc9ad367a85105db0dd",
		"a\nb":   "20123c9e3cb553383e7040d2375ce679933681b53c432a3538f837e4609317c7",
	} {
		evt := nostr.Event{PubKey: pub, CreatedAt: 1700000000, Kind: 9, Tags: nostr.Tags{{"h", "s1-vector"}}, Content: content}
		if got := nip01ID(evt); got != want {
			t.Fatalf("reference id of %q = %s, want %s", content, got, want)
		}
	}

	for _, evt := range []nostr.Event{
		{Content: "a\fb", Tags: nostr.Tags{{"h", "s1-vector"}}},
		{Content: "a\bb", Tags: nostr.Tags{{"h", "s1-vector"}}},
		{Content: "a\x01b", Tags: nostr.Tags{{"h", "s1-vector"}}},
		{Content: "a\nb\tc\r", Tags: nostr.Tags{{"h", "s1-vector"}}},
		{Content: "\x1b[31mred\x1b[0m", Tags: nostr.Tags{{"h", "a\fb"}}},
	} {
		evt.CreatedAt, evt.Kind = 1700000000, 9
		if err := relay.Sign(&evt, [32]byte{1}); err != nil {
			t.Fatal(err)
		}
		if evt.ID.Hex() != nip01ID(evt) || !evt.CheckID() || !evt.VerifySignature() {
			t.Errorf("signed %q %q: id %s, NIP-01 id %s", evt.Content, evt.Tags, evt.ID.Hex(), nip01ID(evt))
		}
	}
}
