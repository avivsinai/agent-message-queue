package acp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// An image the owner attaches in Buzz reaches the session as a local file.
// buzz-acp gives the triggering event as text: its Tags line holds one NIP-92
// imeta tag per attached file. Relay media needs Blossom get auth, so each
// image is fetched as the agent with `buzz media get` and stored under
// <root>/agents/buzz/remote-files/<sha256>.<ext>. The prompt gains one line
// per file, `[image: <path>]` or `[image not attached: <reason>]`, and the turn
// always runs.
const (
	// remoteFileMax caps one file, from the imeta size and from the bytes.
	remoteFileMax = 20 << 20
	// remoteFileLimit caps the files fetched for one message.
	remoteFileLimit = 8
	// remoteFileBudget bounds all downloads of one prompt. They run before
	// the turn's own budget starts, so a prompt with files can wait this
	// long on top of its turn.
	remoteFileBudget = 60 * time.Second
)

var (
	sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// httpsURLRe finds the https URLs of an event's text; blobSHA decides
	// which of them name a blob.
	httpsURLRe = regexp.MustCompile(`https://[^\s"'<>]+`)
	// imageExts are the stored extensions, in the order a stored file is
	// looked up by its sha256.
	imageExts = []string{"png", "jpg", "gif", "webp", "heic"}
)

// eventMedia is one file of a Buzz event: its URL and what its imeta tag
// declares (empty or zero when undeclared).
type eventMedia struct {
	url  string
	mime string
	sha  string
	size int64
}

// attachImages returns prompt with one line per file of its Buzz event, each
// image stored under root. A prompt without files is returned unchanged.
func attachImages(root, prompt string) string {
	media := promptMedia(prompt)
	if len(media) == 0 {
		return prompt
	}
	dir := fsq.AgentRemoteFiles(root, mailboxSender)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	deadline := time.Now().Add(remoteFileBudget)
	lines := make([]string, 0, len(media))
	for i, m := range media {
		if i == remoteFileLimit {
			lines = append(lines, fmt.Sprintf("[image not attached: %d more files; at most %d are attached per message]", len(media)-i, remoteFileLimit))
			break
		}
		stored, err := storeImage(root, dir, m, deadline)
		if err != nil {
			lines = append(lines, "[image not attached: "+oneLine(mediaName(m.url))+": "+oneLine(err.Error())+"]")
			continue
		}
		lines = append(lines, "[image: "+stored+"]")
	}
	return strings.TrimRight(prompt, "\n") + "\n\n" + strings.Join(lines, "\n")
}

// promptMedia lists the files of the prompt's Buzz event: its imeta tags, or,
// when it has none, the Blossom URLs on the agent's relay in the event.
func promptMedia(prompt string) []eventMedia {
	evt := buzzEvent(prompt)
	if evt == "" {
		return nil
	}
	var media []eventMedia
	seen := map[string]bool{}
	for line := range strings.SplitSeq(evt, "\n") {
		rest, ok := strings.CutPrefix(line, "Tags: ")
		if !ok {
			continue
		}
		var tags [][]string
		if json.Unmarshal([]byte(rest), &tags) != nil {
			continue
		}
		for _, t := range tags {
			if len(t) < 2 || t[0] != "imeta" {
				continue
			}
			var m eventMedia
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
			if m.url != "" && !seen[m.url] {
				seen[m.url] = true
				media = append(media, m)
			}
		}
	}
	if len(media) > 0 {
		return media
	}
	for _, raw := range httpsURLRe.FindAllString(evt, -1) {
		if onRelay(raw) && blobSHA(raw) != "" && !seen[raw] {
			seen[raw] = true
			media = append(media, eventMedia{url: raw})
		}
	}
	return media
}

// onRelay reports that raw is an https URL on the host and port of the
// agent's relay.
func onRelay(raw string) bool {
	relay, err := url.Parse(buzzRelayURL)
	if err != nil || relay.Hostname() == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), relay.Hostname()) && urlPort(u) == urlPort(relay)
}

// urlPort is the port u connects to: its explicit port, else its scheme's.
func urlPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "wss":
		return "443"
	case "http", "ws":
		return "80"
	}
	return ""
}

// blobSHA is the sha256, in lower case, that a Blossom URL names its blob
// by in its last path segment, `<sha256>[.<ext>]`, or "".
func blobSHA(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	sha, _, _ := strings.Cut(path.Base(u.Path), ".")
	sha = strings.ToLower(sha)
	if !sha256HexRe.MatchString(sha) {
		return ""
	}
	return sha
}

// mediaGet downloads one relay file as the agent and fails with
// errBuzzOutputLimit past limit bytes. A variable so tests can serve files
// without a relay.
var mediaGet = func(budget time.Duration, limit int, rawURL string) ([]byte, error) {
	return runBuzzCLIBounded(context.Background(), budget, nil, limit, "media", "get", rawURL)
}

// storeImage returns the stored file of m: the file already stored under its
// sha256, else the image fetched and written to dir (mode 0600) by its
// content's sha256 and type.
func storeImage(root, dir string, m eventMedia, deadline time.Time) (string, error) {
	if m.size > remoteFileMax {
		return "", fmt.Errorf("%d bytes is over the 20 MiB limit", m.size)
	}
	if m.mime != "" && !strings.HasPrefix(m.mime, "image/") {
		return "", fmt.Errorf("%s is not an image", m.mime)
	}
	// The download is signed with the agent's key, so it goes only to the
	// agent's own relay.
	if !onRelay(m.url) {
		return "", errors.New("it is not on this agent's relay")
	}
	// The relay serves a Blossom URL's blob by the sha256 the URL names, so
	// that name is the only key of a stored file, and a declared sha256 must
	// be it.
	key := blobSHA(m.url)
	if m.sha != "" && m.sha != key {
		return "", errors.New("its sha256 does not match its URL")
	}
	if key != "" {
		if stored, ok := storedImage(dir, key); ok {
			return stored, nil
		}
	}
	budget := min(time.Until(deadline), defaultPostTimeout)
	if budget <= 0 {
		return "", errors.New("no time left to download it")
	}
	data, err := mediaGet(budget, remoteFileMax, m.url)
	if errors.Is(err, errBuzzOutputLimit) {
		return "", errors.New("it is over the 20 MiB limit")
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	if key != "" && key != sha {
		return "", errors.New("the downloaded bytes do not match its sha256")
	}
	ext := imageExt(data)
	if ext == "" {
		return "", fmt.Errorf("%s is not a png, jpeg, gif, webp or heic image", http.DetectContentType(data))
	}
	// The carrier's own mailbox holds the files, complete so that no half
	// mailbox appears in the root.
	if err := fsq.EnsureAgentDirs(root, mailboxSender); err != nil {
		return "", err
	}
	return fsq.WriteFileAtomic(dir, sha+"."+ext, data, 0o600)
}

// storedImage finds a stored file by its sha256 and marks it used now, so
// history cleanup keeps a file a new message names.
func storedImage(dir, sha string) (string, bool) {
	for _, ext := range imageExts {
		p := filepath.Join(dir, sha+"."+ext)
		if info, err := os.Lstat(p); err == nil && info.Mode().IsRegular() {
			now := time.Now()
			_ = os.Chtimes(p, now, now)
			return p, true
		}
	}
	return "", false
}

// imageExt is the stored extension of an allowed image type, decided by the
// content alone, or "".
func imageExt(data []byte) string {
	switch http.DetectContentType(data) {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	}
	// HEIC is an ISO base media file whose ftyp box names a HEVC brand.
	if len(data) >= 12 && string(data[4:8]) == "ftyp" && slices.Contains([]string{"heic", "heix", "hevc", "hevx", "mif1", "msf1"}, string(data[8:12])) {
		return "heic"
	}
	return ""
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
