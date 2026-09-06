package launch

import "testing"

func TestArgsHaveResumeUsesProviderSyntax(t *testing.T) {
	for _, test := range []struct {
		name   string
		syntax ResumeSyntax
		args   []string
		want   bool
	}{
		{name: "claude continue", syntax: ResumeSyntaxFlags, args: []string{"--continue"}, want: true},
		{name: "claude resume", syntax: ResumeSyntaxFlags, args: []string{"--resume=thread"}, want: true},
		{name: "cursor resume", syntax: ResumeSyntaxCursor, args: []string{"--resume", "chat"}, want: true},
		{name: "cursor continue is not resume", syntax: ResumeSyntaxCursor, args: []string{"--continue"}, want: false},
		{name: "codex resume", syntax: ResumeSyntaxCodex, args: []string{"resume", "thread"}, want: true},
		{name: "codex exec resume", syntax: ResumeSyntaxCodex, args: []string{"exec", "resume", "thread"}, want: true},
		{name: "codex plain exec", syntax: ResumeSyntaxCodex, args: []string{"exec"}, want: false},
		{name: "resume after separator", syntax: ResumeSyntaxFlags, args: []string{"--", "--resume", "thread"}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ArgsHaveResume(test.args, test.syntax); got != test.want {
				t.Fatalf("ArgsHaveResume(%#v, %d) = %v, want %v", test.args, test.syntax, got, test.want)
			}
		})
	}
}
