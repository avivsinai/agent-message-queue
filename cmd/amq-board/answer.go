package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/board"
	"github.com/avivsinai/agent-message-queue/internal/board/source"
	"github.com/avivsinai/agent-message-queue/internal/thread"
)

const amqChanged = "installed amq changed or is unavailable; restart the board"
const answerTimeout = 90 * time.Second

func ownerAnswer(ctx context.Context, store board.Store, env board.Env) board.SubmitAnswer {
	installedVersion := func(ctx context.Context) string {
		out, err := env.Run.Run(ctx, env.Dir, []string{"amq", "--version"})
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	startVersion := installedVersion(ctx)
	versionMatches := func(ctx context.Context) bool { return startVersion != "" && installedVersion(ctx) == startVersion }
	return func(ctx context.Context, q board.AnswerRequest) board.AnswerResult {
		ctx, cancel := context.WithTimeout(ctx, answerTimeout)
		defer cancel()
		var result board.AnswerResult
		err := store.WithAnswerSnapshot(ctx, func(state board.Snapshot) {
			result = submitAnswer(ctx, store, env, state, q, versionMatches)
		})
		if err != nil {
			return board.AnswerResult{Reason: "the published board or action lock is unavailable"}
		}
		return result
	}
}

func submitAnswer(ctx context.Context, store board.Store, env board.Env, state board.Snapshot, q board.AnswerRequest, versionMatches func(context.Context) bool) board.AnswerResult {
	refuse := func(reason string) board.AnswerResult { return board.AnswerResult{Reason: reason} }
	if err := env.Run.(execRunner).amqValidate(ctx); err != nil {
		if !versionMatches(ctx) {
			return refuse(amqChanged)
		}
		return refuse("serve-start AMQ route changed")
	}
	healthy := map[string]bool{}
	for _, r := range state.Input.Results {
		healthy[r.Name] = r.Result.Status == board.StatusOK && !r.Result.ReadFailed
	}
	if !healthy["amq"] || !healthy["amq-history"] {
		return refuse("published AMQ sources are unavailable or degraded")
	}
	cfg, warnings := loadConfig(env.Dir)
	if len(warnings) > 0 {
		return refuse("board configuration could not be read")
	}
	if err := board.CheckFreshness(state, cfg.Board, time.Now()); err != nil {
		return refuse("update first: the published view is stale")
	}
	model := state.Project(cfg.Board, time.Now(), version)
	plan, body, err := q.Select(model.Inbox)
	if err != nil {
		return refuse(err.Error())
	}
	o := plan.Obligation
	// An empty session names the authenticated base, even in a serving shell
	// pinned to a child. Select only the root derived from that base.
	root := filepath.Join(store.BaseRoot, o.OwnerSession)
	if root != plan.Root {
		return refuse("owner mailbox route changed")
	}
	selection := boardSelection{session: o.OwnerSession, me: thread.HumanHandle}
	if selection.session == "" {
		selection.root = store.BaseRoot
	}
	target, pinned, err := resolveBoardSelection(ctx, env, selection)
	if err != nil || target.BaseRootID != store.BaseRootID || target.BaseRoot != store.BaseRoot {
		return refuse("owner mailbox identity changed")
	}
	route, err := readBoardContext(ctx, pinned)
	if err != nil || route.Root != root || route.Session != o.OwnerSession || route.Me != thread.HumanHandle {
		return refuse("owner mailbox route changed")
	}
	if err := source.CheckQuestionOpen(ctx, pinned, store.BaseRoot, plan); err != nil {
		return refuse(err.Error())
	}
	runner := pinned.Run.(execRunner)
	if err := runner.amqValidate(ctx); err != nil {
		return refuse("owner mailbox identity changed")
	}
	if !versionMatches(ctx) {
		return refuse(amqChanged)
	}
	if ctx.Err() != nil {
		return refuse("request ended before dispatch")
	}
	// Once dispatched, the reply must finish its delivery and outbox write
	// under the same lock even if the browser disconnects.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), answerTimeout)
	defer cancel()
	raw, err := runner.WithInput(strings.NewReader(body)).Run(sendCtx, env.Dir, plan.AnswerArgv())
	var report struct {
		ID, Thread string
		To         []string
		InReplyTo  string `json:"in_reply_to"`
		Outbox     struct{ Written bool }
	}
	if err != nil || json.Unmarshal(raw, &report) != nil || report.ID == "" || !report.Outbox.Written || report.InReplyTo != o.OpenerID || report.Thread != o.Thread || !slices.Equal(report.To, []string{o.Destination.Handle}) {
		read, _ := board.ShowThread(root, o.Thread)
		return board.AnswerResult{CheckThread: read.Command}
	}
	return board.AnswerResult{ID: report.ID, Recipient: o.Destination.Handle}
}
