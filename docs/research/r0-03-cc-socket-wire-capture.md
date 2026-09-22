# R0-03: Claude Code cross-session UDS wire capture (bead 611.2)

Authorized probe of Claude Code v2.1.278's cross-session messaging socket, performed
2026-09-22 against a disposable background target session (`claude --bg` in
`/tmp/cc-probe-611.2`, pid 83401, job `55f0c758`). Target metadata came from
`~/.claude/sessions/83401.json` (registry) and `~/.claude/sessions/83401.<sha256>.key`
(peer-token key file). Findings were confirmed end-to-end: a hand-crafted frame was
delivered and rendered in the target session as `› Message from @amq-probe: …`.

## 1. Transport

- **Unix domain socket** at `<messagingSocketPath>` (e.g. `/tmp/cc-socks/<pid>.sock`).
  Registry key: `messagingSocketPath` in `~/.claude/sessions/<pid>.json`.
- **Line-delimited JSON.** One frame per line, terminated by `\n`.
- **Size cap:** 1 MiB per serialized frame (`Kht = 1048576`), measured as
  `authOverhead + jsonLen + 1`; over-cap throws `message_too_large`.
- Socket connect uses a 5s timeout; **no per-frame ack is written back** on the
  happy path (client sends and closes / awaits nothing; receipt tracking is
  internal via `msg_id`).

## 2. Auth handshake (first line, optional on macOS/Linux)

`Gar(token)` = `JSON.stringify({"type":"auth","token":<token>}) + "\n"`.

- The token is the **target's** `peerToken`, not the sender's.
- Sender resolves it from `~/.claude/sessions/<targetPid>.<sha256(canonical socket
  path)>.key`, a JSON file `{"peerToken":"<32 hex>","pidDomain":…,…}`.
  Canonical path = `path.resolve()` (macOS `/private/tmp` vs `/tmp` both work via
  realpath canonicalization; key hash is over the resolved path).
- Without a resolvable key the sender fails closed (`no_live_inbox` /
  "unvouched pipe") — the receiver, not the sender, gates this.
- Token regex: `[0-9a-f]{32}` (exactly 16 random bytes, `$g=16`).

## 3. Peer message frame (the deliverable)

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
  receiver keys rate-limit/dedup state on it. We sent without `from` and delivery
  still succeeded — it is not required by the receiver's parse path, but a real
  client always sets it.
- Control frames use the same `{type:"control", action:…, …J$()}` shape.

### 3.1 XML envelope (content wrapper, `pQe` / parse `TG`)

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
- `from-mode` is an enum (e.g. `code`, per `hG`).

## 4. Minimal working probe (verified delivered)

```python
import socket, json, uuid

SOCK  = "/tmp/cc-socks/83401.sock"
TOKEN = "REDACTED-DEAD-PROBE-TOKEN"   # target's peerToken from its .key file

body = "CAPTURE-PROBE-611.2 ping"
env  = ('<cross-session-message from="amq-probe-6112" from-session="amq-probe-sender"'
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
# expect: no bytes back; message appears in target session as
# `› Message from @amq-probe: CAPTURE-PROBE-611.2 ping`
```

Observed result: socket returns `b''` (recv timeout / clean close), and the target
session's transcript log shows the inbound message banner and the model reasoning
about the ping. Earlier "empty response" probes (bare `sendUserMessage`, bare
`type:"user"` without the XML envelope, Content-Length framing) were correctly
rejected silently — the receiver drops malformed frames without an error frame.

## 5. Ingress guards (receiver side, `Sr.admit` / `peer-guard`) — verified live

Defaults (`tzt`): `bucketCapacity:30, refillPerSecond:0.5, dedupWindowMs:30000,
maxSelfHops:10, maxChainLength:28, maxTrackedSenders:256, maxQueuedPeerMessages:50`.
Drop reasons: `rate-limited`, `duplicate`, `hop-loop`, `hop-runaway`, `queue-full`.
Dedup keys on `(sender, identical body within 30s)`. Batch-drop receipts are
coalesced (500ms trail, 5s max) and reported as
`Dropped a peer message from <addr> (@name): <reason>`.

Live verification against the idle→busy→idle target:

- **Idle target:** frame delivered instantly; message banner appears in the
  target UI (`› Message from @amq-probe: <body> (ctrl+o to expand)`) and a new
  turn starts immediately.
- **Busy target:** frames sent while the target was mid-turn were **parked and
  queued**, not dropped. All queued banners rendered together at the next turn
  boundary and were processed in one batched turn (both replies produced in a
  single 9s turn). Queue cap is 50 (`maxQueuedPeerMessages`); beyond it the
  `queue-full` drop reason applies.
- **Dedup:** an identical body re-sent 1s later was dropped with a visible
  in-session notice: `⏺ Dropped a peer message from @amq-probe (unknown):
  identical to the previous message from this sender.` — i.e. duplicate drops
  are surfaced to the target user/model, not silent.

## 5a. Inbound gate — no `crossSessionInbound` setting exists

There is **no user-level `crossSessionInbound` (or equivalent) setting** in
v2.1.278; no such key appears anywhere in the binary. Inbound acceptance is
gated in code by `As()`:

- env override `CLAUDE_CODE_HARBOR_KITE` (checked first), else GrowthBook
  feature flag `tengu_harbor_kite`, **default: on** (`!0`). Windows additionally
  checks `tengu_harbor_kite_win` (default on).
- A second default-on gate `tengu_cuddly_willow` exists alongside; when either
  gate is off the listener logs `[uds-messaging] Skipped: cross-session
  messaging gate off` and never binds, and parked messages are dropped with
  "parked peer message(s) (cross-session messaging disabled)".
- Sockets dir vetting failures also refuse to bind (fail-closed, message
  "cross-session messaging is OFF for this session").

**Required user-level value: none.** With defaults (gate on), any local process
that can read the target's key file and connect to the socket can deliver
inbound messages; there is no opt-in to flip. AMQ's adapter can rely on the
default and surface the `CLAUDE_CODE_HARBOR_KITE=0` case as "messaging
disabled". (Discovery artifact: `Ubt = "Cross-session messaging is not
available in this session."` shown in sessions where the gate is off.)

## 6. Implications for the AMQ adapter (`internal/remote/claude`)

1. `factory.go` stub can be replaced with a real client: connect → optional auth
   line → one JSON line → close. No reply to parse on the happy path.
2. Sender must read the target's `.key` file to obtain `peerToken`; the key-file
   name is derivable (`<pid>.<sha256(resolve(sockPath))>.key`), so no registry
   scraping beyond `~/.claude/sessions/*.json` for `messagingSocketPath`.
3. The XML envelope must be built exactly as §3.1; recommend a single
   `buildCrossSessionEnvelope(from, fromSession, fromName, body, hopChain, mode)`
   helper mirroring `pQe` and a round-trip self-test mirroring `TG`.
4. `peerProtocol: 1` and `peerFeatures` (`notify_idle`, `reply_across_default_dirs`,
   `artifact_yield`) in the registry pid-file can gate capability negotiation.
5. `CLAUDE_CODE_MESSAGING_SOCKET` env override exists; feature flag
   `tengu_session_stable_address` switches addressing to `sid:<sessionId>` form.
