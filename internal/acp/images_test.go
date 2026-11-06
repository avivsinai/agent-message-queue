package acp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// imagePrompt is a buzz-acp prompt whose event carries one imeta tag.
func imagePrompt(url, sha string, size int) string {
	return "<context>\nScope: dm\nChannel: DM (#chan)\n</context>\n<buzz-event type=\"@mention\">\nEvent ID: " + strings.Repeat("a", 64) +
		"\nContent: what is this?\n" + url + "\nTags: [[\"h\",\"chan\"],[\"imeta\",\"url " + url + "\",\"m image/png\",\"x " + sha + "\",\"size " + strconv.Itoa(size) + "\"]]\n</buzz-event>"
}

// An image the owner sends in Buzz reaches the session as a 0600 file the
// prompt names (bead agent-message-queue-ho2).
func TestAttachImagesStoresTheImageAndNamesIt(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	sha := hex.EncodeToString(sum[:])
	url := "https://relay.example/" + sha + ".png"
	saved, savedRelay := buzzCall, buzzRelayURL
	t.Cleanup(func() { buzzCall, buzzRelayURL = saved, savedRelay })
	buzzRelayURL = "wss://relay.example"
	var calls []string
	buzzCall = func(_ time.Duration, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return buf.Bytes(), nil
	}
	root := canonicalTempDir(t)
	prompt := imagePrompt(url, sha, buf.Len())

	got := attachImages(root, prompt)
	path := filepath.Join(root, "agents", mailboxSender, "remote-files", sha+".png")
	if want := prompt + "\n\n[image: " + path + "]"; got != want {
		t.Fatalf("prompt =\n%s\nwant\n%s", got, want)
	}
	if len(calls) != 1 || calls[0] != "media get "+url {
		t.Fatalf("buzz calls = %q", calls)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, buf.Bytes()) {
		t.Fatalf("stored file = %d bytes, %v; want the image", len(data), err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stored file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

// An image over the cap is refused with a line, without a download, and the
// owner's text still arrives (bead agent-message-queue-ho2).
func TestAttachImagesRefusesAnImageOverTheCap(t *testing.T) {
	saved := buzzCall
	t.Cleanup(func() { buzzCall = saved })
	buzzCall = func(time.Duration, ...string) ([]byte, error) {
		t.Fatal("an over-cap image was downloaded")
		return nil, nil
	}
	sha := strings.Repeat("b", 64)
	prompt := imagePrompt("https://relay.example/"+sha+".png", sha, 30<<20)

	got := attachImages(canonicalTempDir(t), prompt)
	if want := prompt + "\n\n[image not attached: " + sha + ".png: 31457280 bytes is over the 20 MiB limit]"; got != want {
		t.Fatalf("prompt =\n%s\nwant\n%s", got, want)
	}
}
