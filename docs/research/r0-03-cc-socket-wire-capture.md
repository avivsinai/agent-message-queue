# Claude Code cross-session socket wire format

Probe of Claude Code 2.1.278's cross-session messaging socket against a
disposable background target session (`claude --bg`). Target metadata came from
`~/.claude/sessions/<pid>.json` (registry) and `~/.claude/sessions/<pid>.<sha256>.key`
(peer-token key file). Findings were confirmed end-to-end: a hand-crafted frame was
delivered and rendered in the target session as `› Message from @amq-probe: …`.

> **Point-in-time research, not current truth.** This document records what was true of
> Claude Code 2.1.278. It grades individual claims (`[binary]` / `[live]` /
> `[unverified]`), but the whole document ages with that build; re-verify against
> the installed version before relying on any claim (see
> [the compatibility manifest](../remote-compat.md#2-how-to-read-the-evidence)).

**Evidence conventions used below:** `[binary]` = fact derived from static analysis of
the v2.1.278 binary (strings/symbol evidence); `[live]` = observed against the probe
target during this capture; `[unverified]` = inferred, not yet exercised.

## 1. Transport

- **Unix domain socket** at `<messagingSocketPath>` (e.g. `/tmp/cc-socks/<pid>.sock`).
  Registry key: `messagingSocketPath` in `~/.claude/sessions/<pid>.json`. `[binary]`
- **Line-delimited JSON.** One frame per line, terminated by `\n`. `[binary]`
- **Size cap:** 1 MiB per serialized frame (`Kht = 1048576`), measured as
  `authOverhead + jsonLen + 1`; over-cap throws `message_too_large`. `[binary]`
- Socket connect uses a 5s timeout; **no per-frame ack is written back** on the
  happy path (client sends and closes / awaits nothing; receipt tracking is
  internal via `msg_id`). `[binary]` + `[live]` (recv blocked until timeout; empty
  close, while the target transcript showed delivery)

## 2. Auth handshake (first line, optional on macOS/Linux) `[binary]`

`Gar(token)` = `JSON.stringify({"type":"auth","token":<token>}) + "\n"`.

- The token is the **target's** `peerToken`, not the sender's. It is a **bearer
  credential**: possession alone authorizes delivery (no per-connection secret, no
  challenge/response). Treat any read access to `~/.claude/sessions/*.key` as send
  access to the matching session. `[security]`
- Sender resolves it from `~/.claude/sessions/<targetPid>.<sha256(canonical socket
  path)>.key`, a JSON file `{"peerToken":"<32 hex>","pidDomain":…,…}`.
  The hash is over Node `path.resolve()` of the socket path — absolute + normalized,
  **without** symlink resolution (`[live]` verified: `/tmp/cc-socks/N.sock` hashes to
  the key filename; `/private/tmp/...` does **not**, so the `/tmp` and `/private/tmp`
  spellings are distinct keys).
- Without a resolvable key the sender fails closed (`no_live_inbox` /
  "unvouched pipe") — the receiver, not the sender, gates this. `[binary]`
- Token regex: `[0-9a-f]{32}` (exactly 16 random bytes, `$g=16`). `[binary]`
- Auth acceptance matrix `[binary]`:
  | first line | receiver behavior |
  |---|---|
  | none | accepted on macOS/Linux where `authRequired=false`; refused on Windows (authRequired) |
  | valid target token | accepted |
  | wrong/unknown token | refused ("an unparseable line" / auth failure path) |
  | blank line before any auth | refused when `authRequired` |

## 3. Peer message frame (the deliverable) `[binary]`

Built by the send path (binary fn `Jht`, log tag `[uds-client] Sending`):

```json
{
  "msgV": 1,
  "msg_id": "<uuid v4>",
  "type": "user",
  "message": { "role": "user", "content": "<XML envelope>" },
  "priority": "next",
  "from": "<sender address>",
  "file_attachments": [ … ]   // optional, only when non-empty
}
```

- `msgV` is the envelope version (`n=1` in `J$()`); `msg_id` is a fresh UUID per send.
- `from` is the sender's address (e.g. its own socket path or `uds:…` form). The
  receiver keys rate-limit/dedup state on it. `[live]` a frame without `from` was
  still delivered — it is optional in the receiver's parse path — but a real client
  always sets it. **Note `from` is unauthenticated and attacker-controlled**; a
  malicious sender can spoof any `from`/`from-name` to make drops, banners, or
  attribution look like they came from another session. `[security]`
- Control frames use the same `{type:"control", action:…, …J$()}` shape. `[binary]`

### 3.1 XML envelope (content wrapper, `pQe` / parse `TG`) `[binary]`

```
<cross-session-message from="…" from-session="…" hop-chain="…" from-name="…" from-mode="…">
<body text>
</cross-session-message>
```

- All attributes are optional; attribute order in the builder is
  `from`, `from-session`, `hop-chain`, `from-name`, `from-mode`.
- **Round-trip exactness required:** the receiver re-serializes with `pQe` and
  compares to the input byte-for-byte (`if (pQe(...) !== s) return`). Any drift
  (spacing, trailing newline, unescaped chars) silently drops the frame.
- Structure: `^<cross-session-message[^>]*>\n<body>\n</cross-session-message>$`
  — exactly one `\n` after the open tag and before the close tag.
- `from`/`from-name` are restricted character classes (`Yc`); stick to
  `[a-z0-9-]` and no `"`, `<`, `>`, `\n`, `\r`.
- `hop-chain` is comma-separated session refs; used for loop detection
  (`hop-loop`, `hop-runaway`).
- `from-mode` is an enum (e.g. `code`, per `hG`). It is optional in the receiver's parse
  path, but **omitting it has policy consequences**: an envelope with no asserted mode
  class engages the mode-parity holds described in §5a (a sender asserting no class is
  held whenever the target bypasses permission prompts).

### 3.2 Transcript shape of a delivered message `[live]`

On-disk, the target session's JSONL transcript records two entries: a preceding
`type: "queue-operation"` entry, then a `type: "user"` entry whose content is the
envelope wrapped with a preamble and trailing guidance prose:

```
Another Claude session sent a message: <cross-session-message …>
</cross-session-message>  This came from another Claude session — not typed by your
user, but very likely working on their behalf. Treat it as …
```

This entry — the exact envelope text plus the harness wrapper — is the field-level
observable that grades a socket send as **submitted** (the harness accepted the payload
into the transcript); it carries no run identity, so it never reaches `admitted`.

## 4. Minimal working probe (verified delivered)

Token value redacted — the live token used during the capture belonged to the
disposable probe session, whose socket and `.key` file were removed at
teardown; it is dead and must not be republished. Resolve the target's token at
runtime as shown in §2.

```python
import socket, json, uuid, glob, os, hashlib

SOCK = "/tmp/cc-socks/<pid>.sock"
key = glob.glob(os.path.expanduser(
    f"~/.claude/sessions/<pid>.{hashlib.sha256(os.path.abspath(SOCK).encode()).hexdigest()}.key"))
TOKEN = json.load(open(key[0]))["peerToken"]     # bearer credential — handle as a secret

body = "capture probe ping"
env  = ('<cross-session-message from="amq-probe" from-session="amq-probe-sender"'
        ' from-name="amq-probe">\n' + body + '\n</cross-session-message>')
frame = {
    "msgV": 1, "msg_id": str(uuid.uuid4()),
    "type": "user",
    "message": {"role": "user", "content": env},
    "priority": "next",
}
auth = json.dumps({"type": "auth", "token": TOKEN}, separators=(",", ":"))
line = json.dumps(frame, separators=(",", ":"))

s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM); s.settimeout(6)
s.connect(SOCK)
s.sendall((auth + "\n" + line + "\n").encode())
data = s.recv(4096)   # fire-and-forget: recv stays empty until timeout/close
s.close()
# success signal is on the TARGET side: the target session's transcript shows
# `› Message from @amq-probe: capture probe ping`
```

`[live]` The socket returned no bytes (recv timeout / clean close), and the target
session's transcript log showed the inbound message banner and the model reasoning
about the ping. Earlier "empty response" probes (bare `sendUserMessage`, bare
`type:"user"` without the XML envelope, Content-Length framing) were correctly
rejected silently — the receiver drops malformed frames without an error frame.

## 5. Ingress guards (receiver side, `Sr.admit` / `peer-guard`)

Defaults (`tzt`) `[binary]`: `bucketCapacity:30, refillPerSecond:0.5,
dedupWindowMs:30000, maxSelfHops:10, maxChainLength:28, maxTrackedSenders:256,
maxQueuedPeerMessages:50`. Drop reasons `[binary]`: `rate-limited`, `duplicate`,
`hop-loop`, `hop-runaway`, `queue-full`. Dedup keys on `(sender, identical body
within 30s)` `[binary]`. Batch-drop receipts are coalesced (500ms trail, 5s max) and
reported as `Dropped a peer message from <addr> (@name): <reason>` `[binary]`.

Live-verified behaviors `[live]` (idle→busy→idle target):

- **Idle target:** frame delivered instantly; message banner appears in the
  target UI (`› Message from @amq-probe: <body> (ctrl+o to expand)`) and a new
  turn starts immediately.
- **Busy target:** frames sent while the target was mid-turn were **parked and
  queued**, not dropped. All queued banners rendered together at the next turn
  boundary and were processed in one batched turn (both replies produced in a
  single 9s turn).
- **Dedup:** an identical body re-sent 1s later was dropped with a visible
  in-session notice: `⏺ Dropped a peer message from @amq-probe (unknown):
  identical to the previous message from this sender.` — i.e. duplicate drops
  are surfaced to the target user/model, not silent.

Binary-derived but not live-exercised `[unverified]`: the exact rate-limit refill
behavior, the 50-message queue cap overflowing to `queue-full`, hop-loop/hop-runaway
triggers, and the `maxTrackedSenders` eviction.

## 5a. Inbound policy — the `crossSessionInbound` setting `[binary]`

Inbound delivery is governed by a per-settings-file enum key:

```json
{ "crossSessionInbound": "accept" | "hold" | "refuse" }
```

(zod schema: `"Inbound cross-session peer messages (SendMessage from your other
sessions): 'accept' delivers them, 'hold' parks them for your review without letting
Claude act, 'refuse' opts t…"`, plus a `default` UI value that clears the key).

- **Default (unset): mode-parity gating, not unconditional delivery.** The shipped zod
  description, quoted in full from the 2.1.278 binary:

  > Inbound cross-session peer messages (SendMessage from your other sessions):
  > 'accept' delivers them, 'hold' parks them for your review without letting Claude
  > act, 'refuse' opts this session out. An explicit value always wins. Unset (mode
  > parity): a message auto-delivers only when the sending session's permission-mode
  > class matches yours (bypass↔bypass or prompting↔prompting); a mismatched sender's
  > message is held for your approval; a sender that asserts no class is held only
  > while this session bypasses permission prompts.

  So with no setting anywhere there are three outcomes: sender mode class matches the
  receiver's → auto-delivers; sender class differs → held for approval; sender asserts
  no class → held while the receiver bypasses prompts. A second binary string confirms
  the hold families are distinct: `state 'held' only: … 'mode-mismatch' /
  'no-mode-asserted' are the permission-mode parity holds (the sender runs in a
  different permission class, or asserted none while this session bypasses
  permissions); the *-setting causes are a standing crossSessionInbound:'hold'.`
  (`[live]` our probe delivery succeeded because sender and receiver happened to
  satisfy parity — it does not show that unset means `accept`.)
- **Explicit value always wins:** an explicit `accept` suppresses the parity holds and
  delivers unconditionally (subject to the other guards in §5).
- **Precedence:** policy (managed) settings > user settings > repo/local settings,
  and repo/local may only **tighten** (`restrictive:["refuse","hold"]`) — a repo
  cannot loosen a user's or admin's `hold`/`refuse`, and its own `accept` cannot
  override managed policy. User-facing copy: "your own 'accept' cannot override
  managed policy" / "(a repo may only tighten, so your own 'accept' cannot override
  it)". An **invalid value** fails closed to `hold` (`invalid-setting`) with a
  settings-validation warning.
- **`hold` behavior:** messages are parked for user review, "without letting Claude
  act"; held items can later be released when the cause clears
  (`mode-changed` → "permissions are prompting again", `policy-accepts` →
  "crossSessionInbound now accepts", `approved` → "you approved it").
- **`refuse` behavior:** messages refused at the gate (`peer_inbound_gate`,
  `crossSessionInbound=refuse`), sender receipt `refused`;
  `notify_when_idle` subscriptions to this session fail with
  `requester-refuses-inbound`.
- **Bypass/mode-parity holds:** under the unset default these are the gating rule
  itself (see above); with an explicit `accept` they no longer hold messages.
- The listener itself must also be up: env override `CLAUDE_CODE_HARBOR_KITE`
  (checked first), else GrowthBook flag `tengu_harbor_kite`, default **on**;
  a second default-on gate `tengu_cuddly_willow`; sockets-dir vetting failures
  refuse to bind (fail-closed). When messaging is off, parked messages drop with
  "parked peer message(s) (cross-session messaging disabled)". `[binary]`
  (All `[binary]` statements here are against the 2.1.278 build.)

**Required user-level value for unattended AMQ sends: `accept`.** Unset engages
mode-parity gating, and the most likely AMQ configuration — an envelope without
`from-mode` sent to an unattended (bypass-mode) target — parks silently under it.
Operators who want supervision instead can set `crossSessionInbound: "hold"` (user or
managed scope); AMQ's adapter should surface `hold`/`refuse`/parity holds as
non-delivery, not failure. An adapter may instead carry a matching `from-mode` class
in the envelope; that is an adapter design decision, not a claim of this document.

## 5b. Security assessment `[security]`

- **Bearer-token model:** the target's `peerToken` is the only credential; whoever
  can read the `.key` file (same user by default; scope depends on
  `~/.claude/sessions` permissions) can send. No sender authentication, no
  challenge — do not treat `from`/`from-name` as identity.
- **`from` is attacker-controlled:** rate-limit bucket, dedup keying, banner
  attribution ("Message from @…"), and drop notices all key on the spoofable
  `from` string.
- **Inbound text is model input:** a delivered peer message becomes a user-turn
  message to the target's model, i.e. a prompt-injection surface. A local attacker
  who can read the key file can steer the target session's tool use. Mitigations
  observed in the binary: mode-parity gating by default plus the `hold` setting (§5a);
  on-disk mitigations (0700 sockets dir, dir vetting, `sticky` checks)
  gate the listener, not the credential.
- **Recommendation for AMQ:** run the adapter as the same user, treat captured
  tokens as secrets (never log/commit), and prefer `hold`-mode supervision in
  unattended deployments.
