#!/bin/bash
# AMQ Co-op Stop Hook. Allows silently on unavailable/invalid context.
set -u

payload="$(cat)"
command -v amq >/dev/null 2>&1 || exit 0
command -v python3 >/dev/null 2>&1 || exit 0
active="$(printf '%s' "$payload" | python3 -c \
  'import json,sys; print("1" if json.load(sys.stdin).get("stop_hook_active") is True else "0")' 2>/dev/null)" ||
  exit 0

ME="${AM_ME:-claude}"
env_args=(env --me "$ME" --json)
if [ -n "${AM_SESSION:-}" ]; then
  env_args+=(--session "$AM_SESSION")
fi
env_json="$(cd "${CLAUDE_PROJECT_DIR:-.}" && amq "${env_args[@]}" 2>/dev/null)" || exit 0
resolved="$(printf '%s' "$env_json" | python3 -c \
  'import json,sys; d=json.load(sys.stdin); print("\x1f".join((d["root"],d.get("session_name",""),d["me"])))' 2>/dev/null)" ||
  exit 0
IFS=$'\x1f' read -r ROOT SESSION ME <<<"$resolved"

list_json="$(cd "${CLAUDE_PROJECT_DIR:-.}" &&
  amq list --root "$ROOT" --me "$ME" --new --json 2>/dev/null)" || exit 0
state="$ROOT/agents/$ME/.stop-hook-state.json"
decision="$(printf '%s' "$list_json" | python3 -c '
import json,os,sys
state,active,root,session=sys.argv[1:]
session=session or "(none)"
ids=sorted({x["id"] for x in json.load(sys.stdin) if x.get("id")})
old={"blocked_ids":[],"chain_blocks":0}
try:
  with open(state,encoding="utf-8") as f: old=json.load(f)
except (OSError,ValueError,TypeError): pass
blocked=sorted(set(old.get("blocked_ids",[])) & set(ids))
blocks=int(old.get("chain_blocks",0)) if active=="1" else 0
fresh=sorted(set(ids)-set(blocked))
out=None
if not ids: blocks=0
# Five local blocks leave a three-block reserve below the harness limit of eight.
if fresh and blocks >= 5:
  blocks=0
  out={"systemMessage":f"AMQ stop-hook block budget exhausted; allowing stop and resetting the guard with {len(fresh)} fresh message(s) still at root={root} session={session}."}
elif fresh:
  blocked=sorted(set(blocked)|set(fresh)); blocks+=1
  out={"decision":"block","reason":f"You have {len(ids)} pending AMQ message(s), including {len(fresh)} not previously reported. Drain before stopping. root={root} session={session}."}
data=(json.dumps({"schema":1,"blocked_ids":blocked,"chain_blocks":blocks},separators=(",",":"))+"\n").encode()
tmp=f"{state}.tmp"
try:
  try: os.unlink(tmp)
  except FileNotFoundError: pass
  fd=os.open(tmp,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
  with os.fdopen(fd,"wb") as f: f.write(data); f.flush(); os.fsync(f.fileno())
  os.replace(tmp,state)
  dfd=os.open(os.path.dirname(state),os.O_RDONLY)
  try: os.fsync(dfd)
  finally: os.close(dfd)
finally:
  try: os.unlink(tmp)
  except FileNotFoundError: pass
if out is not None: print(json.dumps(out,separators=(",",":")))
' "$state" "$active" "$ROOT" "$SESSION" 2>/dev/null)" || exit 0

if [ -n "$decision" ]; then
  printf '%s\n' "$decision"
fi
