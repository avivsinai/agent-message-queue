#!/bin/sh
# Run this probe ON host G (the Grok Computer).
# Completing or collecting this output does not close bead amq-hws.
# A Mac is not G; never treat this Mac as live G.
# Print host, tool, directory, outbound, and supervision evidence only.
# Do not dump env, tokens, cookies, or secrets. Missing tools still exit 0.
set -eu

printf '%s\n\n' '# Grok Computer host probe'
printf '%s\n' 'This output is evidence from the machine that ran the script.'
printf '%s\n' 'It does **not** prove the host is G and does **not** close bead `amq-hws`.'
printf '%s\n\n' 'A Mac is not G. Live G is still required.'

printf '%s\n\n' '## 1. Host identity'
printf -- '- uname: `%s`\n' "$(uname -a)"
printf -- '- whoami: `%s`\n' "$(whoami)"
printf -- '- id: `%s`\n' "$(id)"
printf -- '- cwd: `%s`\n' "$(pwd)"
printf -- '- date -u: `%s`\n\n' "$(date -u)"

printf '%s\n\n' '## 2. Commands'
printf '%s\n' '| tool | path |'
printf '%s\n' '| --- | --- |'

missing_tools=""
for tool in amq grok claude codex hermes systemctl crontab nohup curl; do
  if command -v "$tool" >/dev/null 2>&1; then
    printf '| `%s` | `%s` |\n' "$tool" "$(command -v "$tool")"
  else
    printf '| `%s` | missing |\n' "$tool"
    if [ -n "$missing_tools" ]; then
      missing_tools="$missing_tools $tool"
    else
      missing_tools=$tool
    fi
  fi
done
printf '\n'
if [ -n "$missing_tools" ]; then
  printf 'Missing tools: %s\n\n' "$missing_tools"
else
  printf '%s\n\n' 'Missing tools: none'
fi

printf '%s\n\n' '## 3. Durable directory candidates'
printf '%s\n' 'Writable means `grok-computer-probe.sentinel` was created and then removed.'
printf '%s\n' '| path | exists | writable |'
printf '%s\n' '| --- | --- | --- |'

if [ -n "${GROK_COMPUTER_PROBE_DIRS:-}" ]; then
  dirs=$GROK_COMPUTER_PROBE_DIRS
elif [ -n "${HOME:-}" ]; then
  dirs="/workspace /home /root $HOME/.grok $HOME/.amq /tmp"
else
  dirs="/workspace /home /root /tmp"
fi

for dir in $dirs; do
  if [ ! -d "$dir" ]; then
    printf '| `%s` | no | n/a |\n' "$dir"
    continue
  fi
  sentinel=$dir/grok-computer-probe.sentinel
  # Subshell so a failed redirect cannot trip `set -e` in the probe.
  if (printf 'grok-computer-probe\n' >"$sentinel") 2>/dev/null; then
    rm -f "$sentinel"
    printf '| `%s` | yes | yes |\n' "$dir"
  else
    printf '| `%s` | yes | no |\n' "$dir"
  fi
done
printf '\n'

printf '%s\n\n' '## 4. Outbound HTTPS'
if command -v curl >/dev/null 2>&1; then
  curl_code=000
  curl_st=0
  curl_code=$(curl -fsS -o /dev/null -w "%{http_code}" https://example.com 2>/dev/null) || curl_st=$?
  if [ "$curl_st" -eq 0 ]; then
    printf -- '- curl https://example.com -> HTTP %s\n\n' "$curl_code"
  else
    printf -- '- curl https://example.com failed (exit %s, http_code %s); not a probe failure\n\n' \
      "$curl_st" "$curl_code"
  fi
else
  printf -- '- no curl\n\n'
fi

printf '%s\n\n' '## 5. Process supervision clues'
printf '%s\n\n' '### crontab -l'
if command -v crontab >/dev/null 2>&1; then
  printf '%s\n' '```'
  crontab -l 2>&1 || true
  printf '%s\n\n' '```'
else
  printf '%s\n\n' 'crontab: missing'
fi

printf '%s\n\n' '### systemd user dir'
if [ -d /etc/systemd/user ]; then
  printf -- '- `/etc/systemd/user`: present\n'
else
  printf -- '- `/etc/systemd/user`: absent\n'
fi

exit 0
