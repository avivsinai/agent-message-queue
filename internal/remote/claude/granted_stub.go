package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

const (
	grantedStubPrefix = "AMQ granted task "

	// GrantedTextLimit is the longest text of a granted request. Claude
	// Code gives a hook's added context to the model whole only below
	// 10,000 characters; the text and the fixed line before it stay below
	// 9,000.
	GrantedTextLimit = 8192
)

// grantedStub is the whole text of the frame a granted request sends.
func grantedStub(msgID string) string { return grantedStubPrefix + msgID }

const grantedContextTail = ": the task for this turn follows. A linked server sent it under the session grant that this session's owner signed. Do it as the user's prompt."

// GrantedContext is what the prompt hook prints as added context for the
// granted request msgID: one line that names the request and the digest of
// the text, an empty line, then the text.
func GrantedContext(msgID, text string) string {
	sum := sha256.Sum256([]byte(text))
	return grantedStubPrefix + msgID + " sha256:" + hex.EncodeToString(sum[:]) + grantedContextTail + "\n\n" + text
}

// grantedContextText returns the text of content when content is the
// context of the granted request msgID and is whole: its first line names
// msgID and a digest, and the text after the empty line has that digest. A
// context that the harness cut or replaced by a preview is not whole.
func grantedContextText(content, msgID string) (string, bool) {
	line, text, ok := strings.Cut(content, "\n\n")
	if !ok {
		return "", false
	}
	digest, ok := strings.CutPrefix(line, grantedStubPrefix+msgID+" sha256:")
	if !ok {
		return "", false
	}
	digest, ok = strings.CutSuffix(digest, grantedContextTail)
	sum := sha256.Sum256([]byte(text))
	return text, ok && digest == hex.EncodeToString(sum[:])
}

var grantedMsgID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// StubMsgID returns the message id of a prompt that is one cross-session
// message whose whole body is a granted stub. Any other prompt, also one
// that holds a stub among other text, is not a stub.
func StubMsgID(prompt string) (string, bool) {
	open, rest, ok := strings.Cut(strings.TrimSpace(prompt), "\n")
	if !ok || !strings.HasPrefix(open, "<cross-session-message ") || !strings.HasSuffix(open, ">") {
		return "", false
	}
	body, ok := strings.CutSuffix(rest, "</cross-session-message>")
	if !ok {
		return "", false
	}
	id, ok := strings.CutPrefix(strings.TrimSpace(body), grantedStubPrefix)
	if !ok || !grantedMsgID.MatchString(id) {
		return "", false
	}
	return id, true
}

// modeAsks reports whether a Claude Code permission mode asks the user, or
// refuses, before an action that is not allowed in advance.
func modeAsks(mode string) bool {
	return mode == "default" || mode == "acceptEdits" || mode == "dontAsk"
}
