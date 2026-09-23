package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/activity"
	"github.com/avivsinai/agent-message-queue/internal/remote/codex"
	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/sharestate"
)

// notificationSource is the attachment seam activity export reads: the Codex
// adapter's read-only notification observer.
type notificationSource interface {
	ObserveNotifications(fn func(codex.Notification)) (stop func())
}

// activityShare is one share's activity export (611.17 slice 3): Codex
// notifications for the approved native thread become NIP-44 kind 24200
// frames to the owner. Export runs only while the target's attached native
// session is the pinned one (relay design §2); it carries no control.
type activityShare struct {
	share    manifest.Share
	body     nostr.SecretKey
	owner    nostr.PubKey
	stateDir string

	mu    sync.Mutex
	state string // exporting, or why not
}

// activityDrainTick is how often queued frames are drained, and the native
// fence re-checked.
var activityDrainTick = 250 * time.Millisecond

// activityPublishTimeout bounds one drain pass's sends.
const activityPublishTimeout = 10 * time.Second

func (as *activityShare) set(state string) {
	as.mu.Lock()
	as.state = state
	as.mu.Unlock()
}

func (as *activityShare) view() string {
	as.mu.Lock()
	defer as.mu.Unlock()
	return as.state
}

// newActivityShare loads a share's body for activity export.
func newActivityShare(root, stateDir string, sh manifest.Share) (*activityShare, error) {
	creds, err := sharestate.Load(root, sh.Session)
	if err != nil {
		return nil, err
	}
	owner, err := nostr.PubKeyFromHex(sh.OwnerPubKey)
	if err != nil {
		return nil, err
	}
	return &activityShare{
		share: sh, body: nostr.SecretKey(creds.Body.Secret()), owner: owner,
		stateDir: filepath.Join(stateDir, "activity", sh.Session), state: "configured",
	}, nil
}

// runActivity is one authenticated connection's activity export. It
// observes the target's notifications only while the native fence holds,
// drains the sink on a tick, and stops observing the moment the attached
// session is no longer the approved one. It returns when the connection or
// ctx ends.
func (as *activityShare) runActivity(ctx context.Context, conn *relay.Conn, identity func(string) string, attachment func(string) (core.Attachment, bool), warn io.Writer) {
	target, pin := as.share.Target, as.share.NativeSessionID
	tick := time.NewTicker(activityDrainTick)
	defer tick.Stop()
	for {
		if identity(target) != pin {
			as.set("closed: the attached session is not the one approved for sharing")
		} else if att, ok := attachment(target); !ok {
			as.set("closed: target is not attached")
		} else if src, ok := att.(notificationSource); !ok {
			as.set("refused: this target's adapter has no activity seam")
		} else {
			as.export(ctx, conn, src, identity, warn)
		}
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			return
		case <-tick.C:
		}
	}
}

// export observes and drains until the fence fails or the connection ends.
func (as *activityShare) export(ctx context.Context, conn *relay.Conn, src notificationSource, identity func(string) string, warn io.Writer) {
	target, pin := as.share.Target, as.share.NativeSessionID
	sink := &activity.Sink{ThreadID: pin, Body: as.body, Owner: as.owner, Publish: conn.Publish, StateDir: as.stateDir}
	defer sink.Close()
	stop := src.ObserveNotifications(func(n codex.Notification) {
		if identity(target) != pin {
			return // never export a replacement session's activity
		}
		if err := sink.Enqueue(n); err != nil {
			say(warn, "relay share %s: activity: %v", as.share.Session, err)
		}
	})
	defer stop()
	as.set("exporting")
	tick := time.NewTicker(activityDrainTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.Done():
			return
		case <-tick.C:
		}
		if identity(target) != pin {
			as.set("closed: the attached session is not the one approved for sharing")
			return
		}
		pctx, cancel := context.WithTimeout(ctx, activityPublishTimeout)
		err := sink.Drain(pctx)
		cancel()
		if err != nil {
			as.set(fmt.Sprintf("publish_pending: %s", relay.Category(err)))
		} else {
			as.set("exporting")
		}
	}
}
