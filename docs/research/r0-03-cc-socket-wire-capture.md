# R0-03: Claude Code cross-session UDS wire capture (bead 611.2)

Authorized probe of Claude Code v2.1.278's cross-session messaging socket, performed
2026-09-22 against a disposable background target session (`claude --bg` in
`/tmp/cc-probe-611.2`, pid 83401, job `55f0c758`). Target metadata came from
`~/.claude/sessions/83401.json` (registry) and `~/.claude/sessions/83401.<sha256>.key`
(peer-token key file). Findings were confirmed end-to-end: a hand-crafted frame was
delivered and rendered in the target session as `› Message from @amq-probe: …`.

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
- `from-mode` is an enum (e.g. `code`, per `hG`).

## 4. Minimal working probe (verified delivered)

Token value redacted — the live token used during the capture belonged to the
disposable probe session (pid 83401), whose socket and `.key` file were removed at
teardown; it is dead and must not be republished. Resolve the target's token at
runtime as shown in §2.

```python
import socket, json, uuid, glob, os, hashlib

SOCK = "/tmp/cc-socks/<pid>.sock"
key = glob.glob(os.path.expanduser(
    f"~/.claude/sessions/<pid>.{hashlib.sha256(os.path.abspath(SOCK).encode()).hexdigest()}.key"))
TOKEN = json.load(open(key[0]))["peerToken"]     # bearer credential — handle as a secret

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
data = s.recv(4096)   # fire-and-forget: recv stays empty until timeout/close
s.close()
# success signal is on the TARGET side: the target session's transcript shows
# `› Message from @amq-probe: CAPTURE-PROBE-611.2 ping`
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

- **Default: `accept`** — with no setting anywhere, inbound peer messages are
  delivered (`f[e ?? "accept"]` resolution). `[live]` confirms: our probe target had
  no such setting and messages were delivered.
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
- **Bypass/mode-parity holds (independent of the setting):** messages are held for
  review when the sender's permission-mode class doesn't match the receiver's
  (`mode-mismatch`), or the sender asserted no mode while the receiver bypasses
  prompts (`no-mode-asserted`), or the receiver bypasses prompts by default
  (`bypass-default`).
- The listener itself must also be up: env override `CLAUDE_CODE_HARBOR_KITE`
  (checked first), else GrowthBook flag `tengu_harbor_kite`, default **on**;
  a second default-on gate `tengu_cuddly_willow`; sockets-dir vetting failures
  refuse to bind (fail-closed). When messaging is off, parked messages drop with
  "parked peer message(s) (cross-session messaging disabled)". `[binary]`
  (Note: this capture ran **before** the version bump to 2.1.273 documented in the
  manifest; all `[binary]` statements here are against the exact 2.1.278 build.)

**Required user-level value for AMQ: none** — defaults deliver. Operators who want
supervision can set `crossSessionInbound: "hold"` (user or managed scope); AMQ's
adapter should surface `hold`/`refuse`/mode-parity holds as non-delivery, not
failure.

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
  observed in the binary: permission-mode parity holds (§5a) and the `hold`
  setting; on-disk mitigations (0700 sockets dir, dir vetting, `sticky` checks)
  gate the listener, not the credential.
- **Recommendation for AMQ:** run the adapter as the same user, treat captured
  tokens as secrets (never log/commit), and prefer `hold`-mode supervision in
  unattended deployments.

## 6. Implications for the AMQ adapter (`internal/remote/claude`)

1. `factory.go` stub can be replaced with a real client: connect → optional auth
   line → one JSON line → close. No reply to parse on the happy path.
2. Sender must read the target's `.key` file to obtain `peerToken`; the key-file
   name is derivable (`<pid>.<sha256(path.resolve(sockPath))>.key`), so no registry
   scraping beyond `~/.claude/sessions/*.json` for `messagingSocketPath`.
3. The XML envelope must be built exactly as §3.1; recommend a single
   `buildCrossSessionEnvelope(from, fromSession, fromName, body, hopChain, mode)`
   helper mirroring `pQe` and a round-trip self-test mirroring `TG`.
4. `peerProtocol: 1` and `peerFeatures` (`notify_idle`, `reply_across_default_dirs`,
   `artifact_yield`) in the registry pid-file can gate capability negotiation.
5. `CLAUDE_CODE_MESSAGING_SOCKET` env override exists; feature flag
   `tengu_session_stable_address` switches addressing to `sid:<sessionId>` form.
6. Probe the `crossSessionInbound: hold` path before shipping unattended sends:
   hold is the fail-closed state for invalid settings and mode mismatches, so an
   AMQ send can silently park rather than deliver or error.
