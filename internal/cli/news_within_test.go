package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
)

// A request sent with --news-within carries news_by to its recipient, arms a
// news timer beside the completion timer's place, and thread open shows
// whether the owner has replied. Observed: a builder told only its reviewer,
// and the requester's request stayed quiet; that message is no reply.
func TestSendNewsWithinShowsInThreadOpen(t *testing.T) {
	clearSendMailboxTestEnv(t)
	root := filepath.Join(t.TempDir(), "queue")
	if err := runInit([]string{"--root", root, "--agents", "alice,bob,carol"}); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "alice", "--to", "bob", "--kind", "todo", "--news-within", "20m", "--body", "build the parser", "--json"})
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	var sent struct {
		ID   string         `json:"id"`
		News followUpResult `json:"news"`
	}
	if err := json.Unmarshal([]byte(out), &sent); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if sent.News.Due == "" || sent.News.After != "20m0s" || sent.News.Error != "" {
		t.Fatalf("news = %+v, want a 20m news clock", sent.News)
	}
	if _, err := os.Stat(filepath.Join(fsq.AgentFollowUps(root, "alice"), sent.ID+".news")); err != nil {
		t.Fatalf("news timer file: %v", err)
	}
	drained := runDrainJSON(t, root, "bob", 0, false)
	if drained.Count != 1 || drained.Drained[0].NewsBy != sent.News.Due {
		t.Fatalf("bob's drain = %+v, want news_by %s", drained.Drained, sent.News.Due)
	}
	if text, err := captureStdout(t, func() error { return outputDrainItems(false, "bob", false, drained.Drained) }); err != nil ||
		!strings.Contains(text, "  News by: "+sent.News.Due+" (") {
		t.Fatalf("drain text = %q (err %v), want a News by line", text, err)
	}

	// Read the wire value so an omitted key, null and false stay distinct.
	type obligation struct {
		Opener       threadOpenOpener `json:"opener"`
		NewsBy       string           `json:"news_by"`
		State        string           `json:"state"`
		OwnerReplied json.RawMessage  `json:"owner_replied"`
	}
	open := func(id string) obligation {
		t.Helper()
		stdout, stderr := captureOutput(t, func() error { return runThread([]string{"open", "--json", "--root", root}) })
		var result struct {
			Obligations []obligation `json:"obligations"`
		}
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatalf("thread open = %s (err %v), stderr %s", stdout, err, stderr)
		}
		for _, ob := range result.Obligations {
			if ob.Opener.ID == id {
				return ob
			}
		}
		t.Fatalf("thread open = %s, want the obligation of %s", stdout, id)
		return obligation{}
	}
	// A status with a closing label can have its own news clock.
	status := runSendJSONForTest(t, "--root", root, "--me", "alice", "--to", "carol", "--kind", "status", "--labels", "done", "--news-within", "5m", "--body", "review complete", "--json")
	if status["news"] == nil {
		t.Fatal("status has no news timer")
	}
	unwatched := runSendJSONForTest(t, "--root", root, "--me", "alice", "--to", "carol", "--kind", "todo", "--body", "also review", "--json")
	if ob := open(unwatched["id"].(string)); ob.OwnerReplied != nil {
		t.Fatal("unwatched request contains owner_replied")
	}
	if ob := open(sent.ID); ob.NewsBy != sent.News.Due || string(ob.OwnerReplied) != "false" {
		t.Fatalf("before any reply: news_by %q owner_replied %v, want %s and false", ob.NewsBy, ob.OwnerReplied, sent.News.Due)
	}

	if _, err := captureStdout(t, func() error {
		return runSend([]string{"--root", root, "--me", "bob", "--to", "carol", "--kind", "review_request", "--refs", sent.ID, "--body", "please review"})
	}); err != nil {
		t.Fatal(err)
	}
	if ob := open(sent.ID); ob.State != "acknowledged" || string(ob.OwnerReplied) != "false" {
		t.Fatalf("after bob wrote only to carol: state %s owner_replied %v, want acknowledged and false", ob.State, ob.OwnerReplied)
	}

	if _, err := captureStdout(t, func() error {
		return runReply([]string{"--root", root, "--me", "bob", "--id", sent.ID, "--news-within", "5m", "--body", "on it"})
	}); err != nil {
		t.Fatal(err)
	}
	if ob := open(sent.ID); string(ob.OwnerReplied) != "true" {
		t.Fatal("after bob replied to alice: owner_replied false, want true")
	}
}
