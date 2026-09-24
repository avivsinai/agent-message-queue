---
name: amq-remote
version: 0.80.1 # x-release-please-version
description: Let the owner drive this running Claude Code or Codex session from Buzz, on the phone or in Buzz Desktop. Use when the user types /amq-remote, /amq-remote off, or /amq-remote status, or asks to control this session from Buzz. Not for AMQ messaging between agents (use amq-cli).
metadata:
  short-description: Control this session from Buzz
  compatibility: claude-code, codex-cli
---

# AMQ Remote

`/amq-remote` connects the owner's one "AMQ Remote" Buzz agent to **this**
session. The owner then DMs that agent from the Buzz phone app or Buzz
Desktop, and each DM runs here as a prompt. The reply goes back to the DM.

Run the steps in order. Stop at the first failure and show the user the
command's own message. Do not work around a refusal.

## `/amq-remote` (connect)

1. **Tools.** Run `command -v amq-remote amq-acp`. If either is missing, tell
   the user to run `brew install avivsinai/tap/amq`, then stop.
2. **Root.** Use `$AM_ROOT` when it is set. Otherwise use
   `$HOME/.amq/remote/root`, and create it with `mkdir -p`.
3. **Claude Code only: the Stop hook.** A Claude turn completes only through
   the AMQ Stop hook. Run `amq-remote doctor --root "$ROOT" --json`. If its
   `failing` list names `native_capability`, tell the user this step adds one
   Stop hook to `~/.claude/settings.json` (it always exits 0, and
   `amq-remote claude uninstall-stop-hook` removes it). Ask once, then run
   `amq-remote claude install-stop-hook`.
4. **The Buzz agent, once per Mac.** If
   `~/Library/Application Support/xyz.block.buzz.app/custom_harnesses/amq_remote.json`
   does not exist, run `amq-acp setup --out "$HOME/Downloads/AMQ Remote.agent.json"`
   and show the user its printed steps: in Buzz Desktop, Agents, then + then
   Import, pick that file, then Start. This is the only Desktop step, and it
   happens once. If Desktop was open, it must be restarted first so it sees the
   new harness.
5. **Bind this session.** Run `amq-remote attach --self --root "$ROOT"`. It
   finds this session exactly, starts the endpoint if needed, and binds it.
6. Tell the user: "Connected. DM **AMQ Remote** from the Buzz app or
   Desktop." Say that a DM sent while this session is mid-turn waits or
   is refused as busy, and that Buzz Stop cannot interrupt a Claude turn (the
   work continues).

Codex: attach needs `CODEX_THREAD_ID` and a thread loaded in the Codex
app-server daemon. A session started with plain `codex` is not in the daemon.
If attach says so, tell the user to start Codex with
`codex app-server daemon start` and then `codex --remote unix://`.

## `/amq-remote off`

Run `amq-remote detach --self --root "$ROOT"`. The session keeps running; the
Buzz agent then answers "Not connected".

## `/amq-remote status`

Show `~/.amq/remote/binding.json` (root, target, session) and the `failing`
list from `amq-remote doctor --root "$ROOT" --json`.

## Trust limits

The Buzz agent's identity belongs to Buzz Desktop. Its grant has no kind limit
and no expiry. Archiving the agent does not invalidate a copied key and
grant; only removing the agent's relay access does that. Buzz "owner only"
also admits the owner's other agents. See the `amq-acp` README.
