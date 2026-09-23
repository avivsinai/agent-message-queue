package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/fsq"
	"github.com/avivsinai/agent-message-queue/internal/relay"
	"github.com/avivsinai/agent-message-queue/internal/remote/manifest"
	"github.com/avivsinai/agent-message-queue/internal/remote/sharestate"
)

// relayStatusFile is serve's diagnostic view of each shared session's relay
// connection; doctor reads it. It is a status snapshot, not authority.
const relayStatusFile = "relay-status.json"

// authKind is the enrolled credential kind presented on connection AUTH
// (relay design §3: one tag, chosen deterministically; 1059 is the body's
// enrolled message kind).
const authKind = 1059

type relayShareStatus struct {
	Session string `json:"session"`
	Target  string `json:"target"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
	Since   string `json:"since"`
}

type relayStatusDoc struct {
	URL    string             `json:"url"`
	Shares []relayShareStatus `json:"shares"`
}

// relayConfigFor re-reads the session's enrolled generation on every
// connection attempt: a renewal is picked up on reconnect, and an expired or
// mismatched credential stops authentication instead of riding an old
// socket. serve never mints, renews or enrolls.
//
// The first body this serve loads is pinned for its lifetime
// (codex slice 1 review r2 #1): a renewal for that body is adopted, but a
// different body under the same owner is refused until the operator
// restarts serve.
func relayConfigFor(root, url string, sh manifest.Share) relay.ConfigFunc {
	var mu sync.Mutex
	pinned := ""
	return func() (relay.Config, error) {
		creds, err := sharestate.Load(root, sh.Session)
		if err != nil {
			return relay.Config{}, err
		}
		if creds.Owner != sh.OwnerPubKey {
			return relay.Config{}, fmt.Errorf("share %s: enrolled owner %s differs from the manifest owner %s", sh.Session, creds.Owner, sh.OwnerPubKey)
		}
		body := creds.Body.PublicKeyHex()
		mu.Lock()
		if pinned == "" {
			pinned = body
		}
		same := pinned == body
		mu.Unlock()
		if !same {
			return relay.Config{}, fmt.Errorf("share %s: enrolled body changed from %s to %s; restart serve to adopt it", sh.Session, pinned, body)
		}
		tag, err := creds.TagFor(authKind, time.Now())
		if err != nil {
			return relay.Config{}, err
		}
		return relay.Config{
			URL:      url,
			Secret:   creds.Body.Secret(),
			AuthTag:  []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()},
			NotAfter: creds.NotAfter,
		}, nil
	}
}

// startRelays runs one authenticated relay client per share until ctx ends,
// and keeps relay-status.json current. A relay that is down or refusing
// leaves local IPC and AMQ operation untouched.
func startRelays(ctx context.Context, root, stateDir string, r *manifest.Relay, stderr io.Writer) *sync.WaitGroup {
	var wg sync.WaitGroup
	if r == nil {
		return &wg
	}
	type entry struct {
		share  manifest.Share
		client *relay.Client
	}
	entries := make([]entry, 0, len(r.Shares))
	for _, sh := range r.Shares {
		c := relay.NewClient(relayConfigFor(root, r.URL, sh))
		entries = append(entries, entry{sh, c})
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.Run(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		last := ""
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			doc := relayStatusDoc{URL: r.URL}
			for _, e := range entries {
				st := e.client.Status()
				doc.Shares = append(doc.Shares, relayShareStatus{
					Session: e.share.Session, Target: e.share.Target, State: string(st.State),
					Error: st.Err, Since: st.Since.UTC().Format(time.RFC3339),
				})
			}
			sort.Slice(doc.Shares, func(i, j int) bool { return doc.Shares[i].Session < doc.Shares[j].Session })
			raw, _ := json.MarshalIndent(doc, "", "  ")
			if string(raw) != last {
				if _, err := fsq.WriteFileAtomic(stateDir, relayStatusFile, raw, 0o600); err != nil {
					say(stderr, "relay status: %v", err)
				} else {
					last = string(raw)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return &wg
}

// loadRelayStatus reads serve's relay status for doctor.
func loadRelayStatus(stateDir string) (*relayStatusDoc, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, relayStatusFile))
	if err != nil {
		return nil, err
	}
	var doc relayStatusDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", relayStatusFile, err)
	}
	return &doc, nil
}
