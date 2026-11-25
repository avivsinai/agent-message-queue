package linkio

import (
	"strings"
	"testing"
	"time"

	"github.com/avivsinai/agent-message-queue/internal/remote/core"
	"github.com/avivsinai/agent-message-queue/internal/remote/protocol"
)

// The live run of 2026-11-24 retained a stored grant after pi changed its
// binding's epoch. Only a task that meets that changed binding ends it;
// absence is deliberately not the same evidence.
func TestGrantSessionChangeEndsStoredGrantButAbsenceDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		changed     func(*Binding)
		wantCode    string
		wantCleared bool
	}{
		{name: "epoch", changed: func(b *Binding) { b.Epoch = "new-epoch" }, wantCode: string(protocol.CodeStaleEpoch), wantCleared: true},
		{name: "session", changed: func(b *Binding) { b.NativeSessionID = "new-session" }, wantCode: CodeSessionChanged, wantCleared: true},
		{name: "absent", wantCode: string(protocol.CodeUnshared)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, key := goldenView(t)
			b := view.Bindings["pi-demo"]
			gen := "g_3jx7k2m4qv5w6y7zab2cd3ef4g"
			digest := goldenGrantDigest(t)
			store := &memGrants{g: Grants{b.Binding: {Generation: gen, Grant: goldenGrant(t, b, key, gen, "2026-10-09T15:10:00Z", key)}}}
			c := *newCarrierForTest(t, cfgWithGrant(t, store))
			notices := make(chan string, 1)
			c.cfg.Notify = func(_, body string) { notices <- body }
			current := b
			if tc.changed != nil {
				tc.changed(&current)
			}
			c.cfg.Bindings = func() []Binding {
				if tc.name == "absent" {
					return nil
				}
				return []Binding{current}
			}
			c.bindingsNow() // the absent view is still not a real end

			if _, err := c.admitGranted(digest, grantSubmit(t, b, "a granted task")); refusalCode(err) != tc.wantCode {
				t.Fatalf("refusal = %v; want %s", err, tc.wantCode)
			}
			store.mu.Lock()
			rec := store.g[b.Binding]
			store.mu.Unlock()
			if (rec.Grant == nil) != tc.wantCleared {
				t.Fatalf("stored grant = %+v; want cleared = %t", rec.Grant, tc.wantCleared)
			}
			if tc.wantCleared {
				if end, ok := c.endedGrant(digest); !ok || end.why != "the session behind binding "+b.Binding+" changed" {
					t.Fatalf("end = %+v", end)
				}
				c.RefreshGrants()
				select {
				case notice := <-notices:
					if !strings.Contains(notice, "ended (session changed)") {
						t.Fatalf("notice %q; want the session changed reason", notice)
					}
				case <-time.After(time.Second):
					t.Fatal("no session-changed end notice")
				}
			}
		})
	}
}

func newCarrierForTest(t *testing.T, c Config) *Carrier {
	t.Helper()
	carrier, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return carrier
}

func cfgWithGrant(t *testing.T, store *memGrants) Config {
	t.Helper()
	view, key := goldenView(t)
	devKey, err := MintDeviceKey(t.TempDir(), "example")
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Name: "example", Pin: Pin{URL: "wss://link.example.invalid/v1", ServerID: view.ServerID},
		StoreID: view.StoreID, Key: devKey, Now: grantClock().now,
		Grants: store, ConsentKeys: func() ([]ConsentKey, error) { return []ConsentKey{key}, nil },
		Handle: func(*protocol.Command, core.Source) (any, error) {
			return protocol.Reply{Outcome: protocol.Outcome{Op: protocol.OpRequestSubmit}}, nil
		},
		GrantInfoFor: func(Binding) (bool, string, int) { return true, "", protocol.MaxInputBytes },
	}
}

func grantSubmit(t *testing.T, b Binding, text string) *protocol.Command {
	t.Helper()
	return &protocol.Command{Op: protocol.OpRequestSubmit, RequestID: "25000000-0000-4000-8000-000000000000", TargetID: b.TargetID, Epoch: b.Epoch,
		NotAfter: time.Date(2026, 10, 9, 14, 11, 0, 0, time.UTC).Format(time.RFC3339),
		Input:    &protocol.SubmitInput{Text: text, Busy: protocol.BusyReject, Deliver: protocol.DeliverTurn, MinEvidence: protocol.EvidenceSubmitted}}
}
