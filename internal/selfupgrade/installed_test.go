package selfupgrade

import "testing"

// A program that runs from its version-store copy names the path it was
// started as, so a caller finds the programs installed beside it.
func TestInstalledImageOfAStoreRunProgramIsThePathItWasStartedAs(t *testing.T) {
	got, err := installedImage("/home/u/.amq/state/versions/ab/amq-acp", true, "/opt/homebrew/bin/amq-acp")
	if err != nil || got != "/opt/homebrew/bin/amq-acp" {
		t.Fatalf("installedImage = %q, %v; want /opt/homebrew/bin/amq-acp", got, err)
	}
}
