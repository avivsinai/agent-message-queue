---
name: amq-remote
version: 0.80.1 # x-release-please-version
description: Let the owner reach this running agent from Buzz, on the phone or in Buzz Desktop. Use when the user types /amq-remote, /amq-remote --native, /amq-remote off, or /amq-remote status, or asks to control this session from Buzz. Not for AMQ messaging between agents (use amq-cli).
metadata:
  short-description: Reach this agent from Buzz
  compatibility: claude-code, codex-cli
---

# AMQ Remote

`/amq-remote` connects the owner's one "AMQ Remote" Buzz agent to this
agent. The owner DMs that agent from the Buzz phone app or Buzz Desktop.

- **Default (AMQ mailbox).** Each DM arrives as an AMQ message from `buzz` in
  this agent's inbox. This agent answers with `amq reply --id <id>`, and that
  reply is shown in the DM. A reply with `--kind status` shows as progress;
  any other reply is the final answer. No Stop hook and no wake are needed.
  Noticing the message is this agent's own business (a wake, a built-in
  consumer, monitor, or the next drain).
- **`--native`.** For a session that is not on AMQ: each DM is typed into
  this exact conversation, and the harness's own final answer goes back.
  On Claude Code it needs the AMQ Stop hook.

Run the steps in order. When a step says to stop, show the user the
command's own message. Do not work around a refusal.

## `/amq-remote` (connect)

1. **Tools.** Run `command -v amq-remote amq-acp`. If either is missing, tell
   the user to run `brew install avivsinai/tap/amq`, then stop.
2. **Bind.**
   - Default: run `amq-remote attach --self`. It uses `AM_ROOT` and `AM_ME`.
     If it says this session is not an AMQ participant, offer `--native`.
   - `--native`: use `$AM_ROOT` when set, else `$HOME/.amq/remote/root`
     (create it with `mkdir -p`). Run
     `amq-remote attach --self --native --root "$ROOT"`.
   If it fails, show its message and stop.
3. **`--native` on Claude Code only: the Stop hook.** Run
   `amq-remote doctor --root "$ROOT" --json` and read its `failing` list; the
   exit code is not the signal. If an entry has `boundary`
   `native_capability`, tell the user this step adds one Stop hook to
   `~/.claude/settings.json` (it always exits 0, and
   `amq-remote claude uninstall-stop-hook` removes it). Ask once, then run
   `amq-remote claude install-stop-hook`. The default mailbox path never
   needs this.
4. **The Buzz agent, once per Mac.** If
   `~/Library/Application Support/xyz.block.buzz.app/custom_harnesses/amq_remote.json`
   does not exist, run `amq-acp setup --out "$HOME/Downloads/AMQ Remote.agent.json"`
   and show its printed steps: in Buzz Desktop, Agents, then + then Import,
   pick that file, then Start. This happens once. If Desktop was open, it must
   be restarted first so it sees the new harness.
5. Tell the user: "Connected. DM **AMQ Remote** from the Buzz app or
   Desktop."
   - Default: "DMs arrive in this agent's AMQ inbox. Stop in Buzz stops
     waiting; a delivered message may still be acted on."
   - `--native`: "A DM sent while this session is busy waits. Buzz Stop cannot
     interrupt a Claude turn."

Codex with `--native`: attach needs `CODEX_THREAD_ID` and a thread loaded in
the Codex app-server daemon. A session started with plain `codex` is not in
the daemon; start it with `codex app-server daemon start`, then
`codex --remote unix://`.

## Answering a Buzz DM (default mailbox)

A message from `buzz` with subject "Buzz DM" is the owner writing from Buzz.
Do the work, then answer with `amq reply --id <that message id>`. Use
`--kind status` for an interim update. Keep the final answer short; it is
read in a chat.

## `/amq-remote off`

Run `amq-remote detach --self`. The session keeps running; the Buzz agent
then answers "Not connected".

## `/amq-remote status`

Show `~/.amq/remote/binding.json` (carrier, root, and handle or target).

## Trust limits

The Buzz agent's identity belongs to Buzz Desktop. Its grant has no kind limit
and no expiry. Archiving the agent does not invalidate a copied key and
grant; only removing the agent's relay access does that. Buzz "owner only"
also admits the owner's other agents. See the `amq-acp` README.
