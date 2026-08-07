//go:build darwin || linux

package cli

import "testing"

func TestWakeSelfUpgradeVersionStrictlyNewerUsesSemverPrecedence(t *testing.T) {
	tests := []struct {
		incumbent string
		candidate string
		want      bool
	}{
		{incumbent: "0.58.0", candidate: "0.59.0", want: true},
		{incumbent: "v1.0.0-rc.1", candidate: "1.0.0", want: true},
		{incumbent: "1.0.0-alpha", candidate: "1.0.0-beta", want: true},
		{incumbent: "1.0.0-beta.2", candidate: "1.0.0-beta.11", want: true},
		{incumbent: "1.0.0+old", candidate: "1.0.0+new", want: false},
		{incumbent: "1.0.0", candidate: "1.0.0-rc.1", want: false},
		{incumbent: "1.0.0", candidate: "1.0.0", want: false},
		{incumbent: "1.0", candidate: "1.0.1", want: false},
		{incumbent: "1.0.0", candidate: "1.01.0", want: false},
		{incumbent: "1.0.0", candidate: "1.0.1-01", want: false},
	}
	for _, test := range tests {
		if got := wakeSelfUpgradeVersionStrictlyNewer(test.incumbent, test.candidate); got != test.want {
			t.Errorf("strictly newer incumbent=%q candidate=%q = %t, want %t", test.incumbent, test.candidate, got, test.want)
		}
	}
}
