# AMQ Demo — Split-pane Claude + Codex collaboration with wake notifications
# Render: vhs docs/demo.tape

Output docs/assets/demo.gif

Require amq
Require tmux

Set Shell bash
Set FontSize 15
Set Width 1400
Set Height 700
Set Padding 20
Set TypingSpeed 40ms
Set Theme "Catppuccin Mocha"

# --- Bootstrap: local amq binary, tmux ---

Hide

Type "rm -rf /tmp/amq-demo && mkdir -p /tmp/amq-demo/bin"
Enter
Sleep 200ms
Type "cp /Users/avivsinai/MyProjects/agent-message-queue/amq /tmp/amq-demo/bin/amq"
Enter
Sleep 200ms

# Clean env
Type "export PATH=/tmp/amq-demo/bin:$PATH AMQ_NO_UPDATE_CHECK=1 BASH_SILENCE_DEPRECATION_WARNING=1 PS1='$ '"
Enter
Sleep 200ms
Type "cd /tmp/amq-demo && clear"
Enter
Sleep 300ms

# Start tmux with bash
Type "tmux kill-session -t demo 2>/dev/null; SHELL=/bin/bash tmux new-session -s demo"
Enter
Sleep 1500ms

# Inside tmux — re-set env
Type "export PATH=/tmp/amq-demo/bin:$PATH AMQ_NO_UPDATE_CHECK=1 BASH_SILENCE_DEPRECATION_WARNING=1 PS1='$ '"
Enter
Sleep 200ms
Type "cd /tmp/amq-demo && clear"
Enter
Sleep 500ms

Show

# --- Scene 1: Initialize project ---

Type "amq coop init"
Enter
Sleep 2s

# --- Scene 2: Set up Claude pane (left) ---
# Use explicit AM_ROOT to avoid .amqrc search finding a parent directory's config.

Hide
Type "export AM_ROOT=/tmp/amq-demo/.agent-mail AM_ME=claude PS1='claude $ ' && clear"
Enter
Sleep 500ms
Show

Sleep 500ms

# --- Scene 3: Split pane and set up Codex (right) ---

Hide
Ctrl+b
Sleep 500ms
Type "%"
Sleep 1500ms

# Right pane (pane 1): new bash shell
Type "export PATH=/tmp/amq-demo/bin:$PATH AMQ_NO_UPDATE_CHECK=1 BASH_SILENCE_DEPRECATION_WARNING=1 PS1='$ '"
Enter
Sleep 200ms
Type "cd /tmp/amq-demo && clear"
Enter
Sleep 500ms

# Set codex env with explicit root
Type "export AM_ROOT=/tmp/amq-demo/.agent-mail AM_ME=codex PS1='codex $ ' && clear"
Enter
Sleep 500ms

# Switch back to claude pane (pane 0) for scene 4
Ctrl+b
Sleep 300ms
Left
Sleep 500ms
Show

Sleep 1s

# --- Scene 4: Claude sends a task ---

Type `amq send --to codex --kind todo --subject "Add input validation" --body "Handle the CLI flags, I'll write the tests"`
Enter
Sleep 2s

# --- Scene 5: Simulate wake — inject notification into codex pane via tmux send-keys ---
# This is exactly what amq wake does (injects text into terminal input buffer via TIOCSTI).
# We use tmux send-keys because TIOCSTI isn't available in VHS virtual terminal.

Hide
Type `tmux send-keys -t demo:0.1 -l "AMQ: message from claude — Add input validation. Drain with: amq drain --include-body — then act on it"`
Enter
Sleep 500ms
Type "clear"
Enter
Sleep 300ms

# Switch to codex pane — notification text is on the prompt line
Ctrl+b
Sleep 300ms
Right
Sleep 500ms
Show

# Let viewer see the wake notification for a moment
Sleep 4s

# Clear the notification line and drain the message
Ctrl+u
Sleep 300ms

Type "amq drain --include-body"
Enter
Sleep 3s

# --- Scene 6: Codex replies ---

Type `amq send --to claude --kind status --body "Done -- added flag checks in cmd/validate.go"`
Enter
Sleep 2s

# --- Scene 7: Claude drains the reply ---

Hide
Ctrl+b
Sleep 500ms
Left
Sleep 800ms
Show

Sleep 500ms

Type "amq drain --include-body"
Enter
Sleep 3s

# Hold final frame
Sleep 2s
