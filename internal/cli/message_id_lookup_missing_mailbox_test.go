package cli

import "testing"

// Regression for review-849-r2 P1-a: a handle with no mailbox reaches the
// header-ID scan in reply (reply has no requireMailboxDeliveryRoot guard).
// A missing candidate directory is definitive absence, so the lookup must
// report not-found (exit 3), not a raw open failure (exit 1). read --id is
// covered for the same shape because it shares the scan.
func TestHeaderIDLookupWithMissingMailboxIsNotFound(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(root string) error
	}{
		{
			name: "reply",
			run: func(root string) error {
				_, _, err := captureEnvOutput(t, func() error {
					return runReply([]string{"--root", root, "--me", "nobodyhandle", "--id", "2026-09-22T10-00-00.000Z_pid12345_a1b2c3d4", "--body", "ping", "--json"})
				})
				return err
			},
		},
		{
			name: "read",
			run: func(root string) error {
				_, _, err := captureEnvOutput(t, func() error {
					return runRead([]string{"--root", root, "--me", "nobodyhandle", "--id", "2026-09-22T10-00-00.000Z_pid12345_a1b2c3d4", "--json"})
				})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The root knows the handle (config lists it) but the agent's
			// mailbox directories were never created: the typo'd --me /
			// wrong-root trigger from the live repro.
			root := initializedSendMailboxRoot(t)
			configureSendTestRoot(t, root, "nobodyhandle")
			err := tc.run(root)
			if err == nil {
				t.Fatal("expected not-found for a handle without a mailbox")
			}
			if code := GetExitCode(err); code != ExitNotFound {
				t.Fatalf("missing mailbox must exit 3, got %d: %v", code, err)
			}
		})
	}
}
