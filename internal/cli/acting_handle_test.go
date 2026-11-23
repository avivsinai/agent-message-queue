package cli

import "testing"

func TestActingHandleNote(t *testing.T) {
	pinnedRoot := t.TempDir()
	otherRoot := t.TempDir()
	note := "this shell is codex-lead (AM_ME), and this command acts as codex (--me); it does not read or send mail for codex-lead"

	tests := []struct {
		name    string
		envMe   string
		hasMe   bool
		envRoot string
		hasRoot bool
		root    string
		me      string
		want    string
	}{
		{name: "no pin", root: pinnedRoot, me: "codex"},
		{name: "the pinned handle", envMe: "codex-lead", hasMe: true, envRoot: pinnedRoot, hasRoot: true, root: pinnedRoot, me: "codex-lead"},
		{name: "another handle in the pinned root", envMe: "codex-lead", hasMe: true, envRoot: pinnedRoot, hasRoot: true, root: pinnedRoot, me: "codex", want: note},
		{name: "another handle in another root", envMe: "codex-lead", hasMe: true, envRoot: pinnedRoot, hasRoot: true, root: otherRoot, me: "codex"},
		{name: "AM_ME without AM_ROOT", envMe: "codex-lead", hasMe: true, root: pinnedRoot, me: "codex"},
		{name: "the pinned handle with spaces", envMe: " codex-lead ", hasMe: true, envRoot: pinnedRoot, hasRoot: true, root: pinnedRoot, me: "codex-lead "},
		{name: "the pinned handle in another case", envMe: "codex-lead", hasMe: true, envRoot: pinnedRoot, hasRoot: true, root: pinnedRoot, me: "Codex-Lead"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setOptionalEnv(t, envMe, tt.envMe, tt.hasMe)
			setOptionalEnv(t, envRoot, tt.envRoot, tt.hasRoot)
			if got := actingHandleNote(tt.root, tt.me); got != tt.want {
				t.Fatalf("actingHandleNote(%q, %q) = %q, want %q", tt.root, tt.me, got, tt.want)
			}
		})
	}
}
