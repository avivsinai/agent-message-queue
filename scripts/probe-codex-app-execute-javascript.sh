#!/bin/sh
# Task-0 evidence harness for the native Codex app Apple Events dictionary.
# Run this manually on macOS with exactly one Codex app window and tab open.
# It is not a live-delivery test and is intentionally not run by this change.
# Current live result (Codex app 26.814.41407) is recorded at:
# https://github.com/avivsinai/agent-message-queue/issues/640#issuecomment-5402828566
set -eu

expected='AMQ_CODEX_APP_EXECUTE_JS_TASK_0'

if [ "$(uname -s)" != 'Darwin' ]; then
  printf '%s\n' 'Codex app task-0 probe requires macOS; no live probe was run.' >&2
  exit 2
fi
if ! command -v osascript >/dev/null 2>&1; then
  printf '%s\n' 'Codex app task-0 probe requires osascript.' >&2
  exit 2
fi

apple_script=$(cat <<'APPLESCRIPT'
on run
	tell application "ChatGPT"
		set windowCount to count of windows
		if windowCount is not 1 then error "task-0 requires exactly one Codex app window"
		set targetWindow to item 1 of windows
		set tabCount to count of tabs of targetWindow
		if tabCount is not 1 then error "task-0 requires exactly one Codex app tab"
		set targetTab to item 1 of tabs of targetWindow
		return execute javascript "(() => 'AMQ_CODEX_APP_EXECUTE_JS_TASK_0')()" in targetTab
	end tell
end run
APPLESCRIPT
)

if result=$(osascript -e "$apple_script"); then
  :
else
  status=$?
  printf 'Codex app task-0 probe failed (exit %s).\n' "$status" >&2
  exit "$status"
fi
result=$(printf '%s' "$result" | tr -d '\r\n')
if [ "$result" != "$expected" ]; then
  printf 'Codex app task-0 probe returned %s; want %s.\n' "$result" "$expected" >&2
  exit 1
fi

printf 'Codex app execute-javascript task-0 probe: PASS (%s)\n' "$expected"
