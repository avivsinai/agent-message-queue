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
	// Commands is the owner-DM surface's state for a commands share:
	// configured, closed (membership unverified), subscription_active,
	// publish_pending, or refused.
	Commands string `json:"commands,omitempty"`
}

type relayStatusDoc struct {
	URL    string             `json:"url"`
	Shares []relayShareStatus `json:"shares"`
}

// relayConfigFor re-reads the session's enrolled generation on every
// connection attempt: a renewal is picked up on reconnect, and an expired or
// mismatched credential stops authentication instead of riding an old
// socket. serve never mints, renews or enrolls.
func relayConfigFor(root, url string, sh manifest.Share) relay.ConfigFunc {
	return func() (relay.Config, error) {
		creds, err := sharestate.Load(root, sh.Session)
		if err != nil {
			return relay.Config{}, err
		}
		if creds.Owner != sh.OwnerPubKey {
			return relay.Config{}, fmt.Errorf("share %s: enrolled owner %s differs from the manifest owner %s", sh.Session, creds.Owner, sh.OwnerPubKey)
		}
		tag, err := creds.TagFor(authKind, time.Now())
		if err != nil {
			return relay.Config{}, err
		}
		return relay.Config{
			URL:     url,
			Secret:  creds.Body.Secret(),
			AuthTag: []string{"auth", tag.OwnerPubKey, tag.Conditions, tag.SigHex()},
		}, nil
	}
}

// startRelays runs one authenticated relay client per share until ctx ends,
// and keeps relay-status.json current. A relay that is down or refusing
// leaves local IPC and AMQ operation untouched.
func startRelays(ctx context.Context, root, stateDir string, r *manifest.Relay, edges *dmEdges, stderr io.Writer) *sync.WaitGroup {
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
		if edges != nil && sh.Commands {
			if creds, err := sharestate.Load(root, sh.Session); err == nil {
				if ds := edges.forBody(creds.Body.PublicKeyHex()); ds != nil {
					c.OnConnect = func(ctx context.Context, conn *relay.Conn) { ds.runDM(ctx, conn, edges, stderr) }
				}
			}
		}
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
				row := relayShareStatus{
					Session: e.share.Session, Target: e.share.Target, State: string(st.State),
					Error: st.Err, Since: st.Since.UTC().Format(time.RFC3339),
				}
				if edges != nil && e.share.Commands {
					row.Commands = edges.stateOf(e.share.Session)
				}
				doc.Shares = append(doc.Shares, row)
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
