package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/board/source"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

const actionHost = "127.0.0.1:4321"

type answerFixture struct {
	handler     http.Handler
	model       board.Model
	log         bytes.Buffer
	key, opener string
	run         func(...string) []byte
}

func newAnswerFixture(t *testing.T, cliEnv ...string) *answerFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("init scratch project: %v: %s", err, out)
	}
	base := filepath.Join(dir, ".agent-mail")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The boundary under test is an installed CLI selected once. Re-enter
	// its real dispatcher in a child with no ambient AMQ session or identity.
	runner := execRunner{amqExe: exe, amqEnv: []string{"HOME=" + home, "USERPROFILE=" + home, "PATH=" + os.Getenv("PATH"), "SystemRoot=" + os.Getenv("SystemRoot"), "AM_ROOT=" + base, "AMQ_BOARD_TEST_CLI=1", "AMQ_NO_UPDATE_CHECK=1", "GORACE=atexit_sleep_ms=0"}}
	runner.amqEnv = append(runner.amqEnv, cliEnv...)
	env := newEnv(dir, runner)
	ctx := t.Context()
	f := &answerFixture{}
	f.run = func(args ...string) []byte {
		t.Helper()
		out, err := runner.Run(ctx, dir, append([]string{"amq"}, args...))
		if err != nil {
			t.Fatalf("amq %s: %v", args[0], err)
		}
		return out
	}
	f.run("init", "--root", base, "--agents", "asker,user,worker")
	f.run("session", "create", "owner")
	f.run("session", "create", "source")
	var sent struct{ ID string }
	raw := f.run("send", "--from-session", "source", "--session", "owner", "--me", "asker", "--to", "user", "--kind", "question", "--thread", "publish", "--body", "When?", "--context", `{"decision":{"options":[{"label":"Today"},{"label":"Later"}]}}`, "--json")
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatal(err)
	}
	f.opener = sent.ID
	f.run("send", "--from-session", "source", "--session", "owner", "--me", "asker", "--to", "worker", "--kind", "question", "--thread", "internal", "--body", "Ready?")
	store, pinned, err := resolveBoardContext(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	state, err := store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store.ProjectID = state.ProjectID
	history, messages, blobs := source.ReadHistory(ctx, pinned, store.BaseRoot, nil, nil)
	if history.Status != board.StatusOK {
		t.Fatalf("history: %s", history.Detail)
	}
	acquisition := board.Acquisition{ProjectID: store.ProjectID, Input: board.Input{Dir: dir, Now: time.Now(), Results: []board.NamedResult{{Name: "amq", Result: board.Result{Status: board.StatusOK}}, {Name: "amq-history", Result: history}}}, Messages: messages, Evidence: blobs}
	if err := store.Commit(ctx, acquisition); err != nil {
		t.Fatal(err)
	}
	state, err = store.ReadContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.model = state.Project(board.DefaultConfig(), time.Now(), "test")
	f.key = store.BaseRoot + "#" + sent.ID
	live := board.Live{WriteToken: "fixture-write-authority", Submit: ownerAnswer(ctx, store, pinned), Now: time.Now,
		Load:  func() (board.FileConfig, []string) { return board.FileConfig{Board: board.DefaultConfig()}, nil },
		Build: func(context.Context, board.FileConfig, bool) board.Model { return f.model },
		Logf:  func(format string, args ...any) { fmt.Fprintf(&f.log, format, args...) }}
	live.Refresh(ctx, false)
	f.handler = live.Handler("read", actionHost)
	return f
}

func (f *answerFixture) request(body string) *http.Request {
	key, _ := json.Marshal(f.key)
	r := httptest.NewRequest("POST", "http://"+actionHost+"/read/actions", strings.NewReader(strings.ReplaceAll(body, `"$ITEM"`, string(key))))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://"+actionHost)
	r.Header.Set("X-AMQ-Write-Token", "fixture-write-authority")
	return r
}

func (f *answerFixture) serve(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func (f *answerFixture) answers(t *testing.T) []thread.Entry {
	t.Helper()
	raw := f.run("thread", "--root", filepath.Join(f.model.Dir, ".agent-mail", "source"), "--id", "publish", "--agents", "asker", "--include-body", "--json")
	var entries []thread.Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(entries, func(e thread.Entry) bool { return e.Kind != "answer" })
}

// A browser answer is delivered through the original reply route, including
// a free answer outside the options the asker offered.
func TestOwnerAnswerDeliversInTheOriginalThread(t *testing.T) {
	for _, tc := range []struct{ name, field, body string }{{"option", "option", "Later"}, {"own words", "text", "@neither: choose another time"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAnswerFixture(t)
			w := f.serve(f.request(fmt.Sprintf(`{"action":"answer","item":"$ITEM","%s":%q}`, tc.field, tc.body)))
			var result board.AnswerResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
				t.Fatalf("answer status %d: %+v", w.Code, result)
			}
			entries := f.answers(t)
			if len(entries) != 1 {
				t.Fatalf("delivered %d answers", len(entries))
			}
			e := entries[0]
			if result.Recipient != "asker" || result.ID != e.ID || e.From != "user" || !slices.Equal(e.To, []string{"asker"}) || e.Thread != "publish" || !slices.Equal(e.Refs, []string{f.opener}) || strings.TrimSuffix(e.Body, "\n") != tc.body {
				t.Fatalf("reply %+v, delivered %+v", result, e)
			}
		})
	}
	// Security review of x36b.1 observed a disconnected browser killing
	// the reply process before it could write the owner's outbox.
	t.Run("disconnect after dispatch", func(t *testing.T) {
		ctx, disconnect := context.WithCancel(t.Context())
		defer disconnect()
		started := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { disconnect() }))
		defer started.Close()
		f := newAnswerFixture(t, "AMQ_BOARD_TEST_REPLY_STARTED="+started.URL)
		req := f.request(`{"action":"answer","item":"$ITEM","option":"Later"}`).WithContext(ctx)
		w := f.serve(req)
		answers := f.answers(t)
		var sent []thread.Entry
		raw := f.run("thread", "--root", filepath.Join(f.model.Dir, ".agent-mail", "owner"), "--id", "publish", "--agents", "user", "--json")
		if err := json.Unmarshal(raw, &sent); err != nil {
			t.Fatal(err)
		}
		sent = slices.DeleteFunc(sent, func(e thread.Entry) bool { return e.Kind != "answer" })
		if ctx.Err() == nil || w.Code != 200 || len(answers) != 1 || len(sent) != 1 || sent[0].ID != answers[0].ID {
			t.Fatalf("disconnect at reply start: HTTP %d, asker inbox %d, owner outbox %d", w.Code, len(answers), len(sent))
		}
	})
}

func TestOwnerAnswerRefusalsLeaveMailAlone(t *testing.T) {
	f := newAnswerFixture(t)
	const good = `{"action":"answer","item":"$ITEM","option":"Later"}`
	for _, tc := range []struct {
		name, body string
		change     func(*http.Request)
		status     int
	}{
		{"host", good, func(r *http.Request) { r.Host = "elsewhere" }, 403},
		{"path", good, func(r *http.Request) { r.URL.Path = "/other/actions" }, 404},
		{"preflight", good, func(r *http.Request) { r.Method = "OPTIONS" }, 405},
		{"foreign origin", good, func(r *http.Request) { r.Header.Set("Origin", "https://elsewhere.invalid") }, 403},
		{"missing origin", good, func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"missing token", good, func(r *http.Request) { r.Header.Del("X-AMQ-Write-Token") }, 403},
		{"old token", good, func(r *http.Request) { r.Header.Set("X-AMQ-Write-Token", "previous") }, 403},
		{"content type", good, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"unknown action", strings.Replace(good, "answer", "overrule", 1), nil, 400},
		{"malformed", "{", nil, 400},
		{"additional field", strings.TrimSuffix(good, "}") + `,"argv":[]}`, nil, 400},
		{"duplicate", strings.TrimSuffix(good, "}") + `,"option":"Later"}`, nil, 400},
		{"option and text", strings.TrimSuffix(good, "}") + `,"text":"tomorrow"}`, nil, 400},
		{"trailing", good + " {}", nil, 400},
		{"too large", strings.Repeat(" ", 16<<10), func(r *http.Request) { r.ContentLength = -1 }, 413},
		{"blank", `{"action":"answer","item":"$ITEM","text":" "}`, nil, 400},
		{"unknown item", strings.Replace(good, "$ITEM", "missing", 1), nil, 409},
		{"foreign item", strings.Replace(good, "$ITEM", foreignQuestion(t, f), 1), nil, 409},
		{"invalid option", strings.Replace(good, "Later", "later", 1), nil, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.request(tc.body)
			if tc.change != nil {
				tc.change(r)
			}
			w := f.serve(r)
			if w.Code != tc.status || w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("HTTP %d, want %d", w.Code, tc.status)
			}
		})
	}
	// The review found 2,100 two-byte letters passed the textarea limit
	// and received a generic refusal, leaving the owner no way to correct it.
	t.Run("UTF-8 byte limit", func(t *testing.T) {
		w := f.serve(f.request(`{"action":"answer","item":"$ITEM","text":"` + strings.Repeat("é", 2100) + `"}`))
		var result board.AnswerResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 400 || result.Reason != "the answer is over 4 KiB" {
			t.Fatalf("overlong answer: HTTP %d, reason %q", w.Code, result.Reason)
		}
	})
	t.Run("stale publication", func(t *testing.T) {
		dir := filepath.Join(f.model.Dir, ".amq")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, board.ConfigFile)
		if err := os.WriteFile(path, []byte(`{"liveness_window":"1ns"}`), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
		})
		w := f.serve(f.request(good))
		if w.Code != 409 || !strings.Contains(w.Body.String(), "stale") {
			t.Fatalf("stale publication: %d %s", w.Code, w.Body.String())
		}
	})
	if got := len(f.answers(t)); got != 0 {
		t.Fatalf("refusals delivered %d answers", got)
	}
	t.Run("static export and logs", func(t *testing.T) {
		var page bytes.Buffer
		if err := board.WriteHTML(&page, f.model, ""); err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{page.String(), f.log.String(), f.serve(httptest.NewRequest("GET", "http://"+actionHost+"/read/model.json", nil)).Body.String()} {
			if strings.Contains(s, "fixture-write-authority") || strings.Contains(s, `data-option="Later"`) {
				t.Fatal("live authority or a submit control escaped the live page")
			}
		}
		if !strings.Contains(page.String(), "Copy answer: Later") {
			t.Fatal("static answer copy missing")
		}
	})
	t.Run("concurrent duplicate", func(t *testing.T) {
		results := make(chan int, 2)
		for range 2 {
			go func() { results <- f.serve(f.request(good)).Code }()
		}
		codes := []int{<-results, <-results}
		slices.Sort(codes)
		if !slices.Equal(codes, []int{200, 409}) || len(f.answers(t)) != 1 {
			t.Fatalf("simultaneous POSTs returned %v", codes)
		}
	})
}

func foreignQuestion(t *testing.T, f *answerFixture) string {
	t.Helper()
	var entries []thread.Entry
	raw := f.run("thread", "--root", filepath.Join(f.model.Dir, ".agent-mail", "owner"), "--id", "internal", "--json")
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) != 1 {
		t.Fatal("foreign question missing")
	}
	value, _ := json.Marshal(filepath.Join(f.model.Dir, ".agent-mail") + "#" + entries[0].ID)
	return string(value[1 : len(value)-1])
}
