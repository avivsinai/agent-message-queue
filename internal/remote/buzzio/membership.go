package buzzio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"fiatjaf.com/nostr"

	"github.com/avivsinai/agent-message-queue/internal/relay"
)

// NIP-29 group metadata kinds the relay signs with its own key.
const (
	kindGroupMetadata = 39000
	kindGroupMembers  = 39002
)

// membershipTimeout bounds one membership read.
const membershipTimeout = 10 * time.Second

// ErrMembership is a DM channel whose membership is not exactly owner and
// body, is not a private DM, or cannot be read. The DM surface stays closed.
var ErrMembership = errors.New("DM channel membership is not owner and body only")

// VerifyDMMembership reads the relay-signed membership (39002) and metadata
// (39000) snapshots of the bound channel and requires: both signed by the
// relay's own key relaySelf, both for this channel (d tag), members exactly
// {owner, body}, and the channel private and of type dm. Empty, invalid,
// ambiguous or unreadable snapshots refuse (slice 4 contract, pinned Buzz
// source a929532: filters by #d with authors [self]; the authenticated body
// must itself be a member to read them).
//
// Limit, stated rather than hidden: these are stored snapshots behind a
// short relay-side cache, not a linearizable read, so a membership change
// can be seen late; callers re-verify on a timer and close the surface when
// the answer changes.
func VerifyDMMembership(ctx context.Context, conn *relay.Conn, relaySelf string, b Binding) error {
	self, err := nostr.PubKeyFromHex(relaySelf)
	if err != nil {
		return fmt.Errorf("%w: relay self key: %v", ErrMembership, err)
	}
	ctx, cancel := context.WithTimeout(ctx, membershipTimeout)
	defer cancel()
	id := fmt.Sprintf("dm-members-%d", time.Now().UnixNano())
	sub, err := conn.Subscribe(ctx, id,
		nostr.Filter{Kinds: []nostr.Kind{kindGroupMembers}, Authors: []nostr.PubKey{self}, Tags: nostr.TagMap{"d": {b.Channel}}, Limit: 1},
		nostr.Filter{Kinds: []nostr.Kind{kindGroupMetadata}, Authors: []nostr.PubKey{self}, Tags: nostr.TagMap{"d": {b.Channel}}, Limit: 1},
	)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMembership, err)
	}
	defer sub.Close()
	var members, meta *nostr.Event
	newer := func(cur *nostr.Event, evt nostr.Event) *nostr.Event {
		if cur == nil || evt.CreatedAt > cur.CreatedAt {
			return &evt
		}
		return cur
	}
	for done := false; !done; {
		select {
		case evt := <-sub.Events:
			if tagValue(evt, "d") != b.Channel {
				continue
			}
			switch evt.Kind {
			case kindGroupMembers:
				members = newer(members, evt)
			case kindGroupMetadata:
				meta = newer(meta, evt)
			}
		case <-sub.EOSE:
			done = true
		case <-sub.Done():
			return fmt.Errorf("%w: membership read ended: %v", ErrMembership, sub.Err())
		case <-ctx.Done():
			return fmt.Errorf("%w: membership read timed out", ErrMembership)
		}
	}
	if members == nil || meta == nil {
		return fmt.Errorf("%w: no relay-signed membership and metadata for channel %s", ErrMembership, b.Channel)
	}
	got := map[string]bool{}
	for _, t := range members.Tags {
		if len(t) >= 2 && t[0] == "p" {
			got[t[1]] = true
		}
	}
	if len(got) != 2 || !got[b.Owner] || !got[b.Body] {
		return fmt.Errorf("%w: channel %s has %d member(s)", ErrMembership, b.Channel, len(got))
	}
	private, dm := false, false
	for _, t := range meta.Tags {
		switch {
		case len(t) >= 1 && t[0] == "private":
			private = true
		case len(t) >= 2 && t[0] == "t" && t[1] == "dm":
			dm = true
		}
	}
	if !private || !dm {
		return fmt.Errorf("%w: channel %s is not a private DM", ErrMembership, b.Channel)
	}
	return nil
}

// maxNIP11Bytes bounds the relay information document.
const maxNIP11Bytes = 64 << 10

// RelaySelf fetches the relay's NIP-11 document from the configured origin
// and returns its self key (the key that signs NIP-29 group metadata). The
// request goes to the same host over https for wss (http only for a
// loopback ws test relay), and no redirect is followed. A missing or
// malformed self key is an error: the operator must pin one instead.
func RelaySelf(ctx context.Context, wsURL string) (string, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	default:
		return "", fmt.Errorf("relay url scheme %q", u.Scheme)
	}
	ctx, cancel := context.WithTimeout(ctx, membershipTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/nostr+json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("relay information document redirected; refusing")
	}}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("relay information document: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("relay information document: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxNIP11Bytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxNIP11Bytes {
		return "", errors.New("relay information document is too large")
	}
	var doc struct {
		Self string `json:"self"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("relay information document: %w", err)
	}
	self := strings.ToLower(strings.TrimSpace(doc.Self))
	if !validHexID(self) {
		return "", errors.New("relay information document has no valid self key; pin relay_self in the manifest")
	}
	return self, nil
}
