# pi

`amq-bridge.ts` is a [pi](https://github.com/earendil-works/pi) extension. It
makes a live pi session reachable by `amq-remote` target kind `pi`: a remote
request arrives in the session as a follow-up user message, and `amq-remote`
sees its admission, its completion, and session changes. The wire contract is
[the pi bridge protocol](../../docs/pi-bridge-protocol.md).

## Install

Load the extension that matches the installed `amq-remote`:

```bash
pi -e "$(amq-remote pi bridge-path)"
```

`amq-remote pi bridge-path` writes the extension built into that binary to
`~/.amq/pi/amq-bridge.ts` and prints the path. After an AMQ upgrade, the next
pi start loads the new bridge.

Or install the package from a release tag:

```bash
pi install git:github.com/avivsinai/agent-message-queue@<tag>
```

Or load it for one invocation from a checkout:

```bash
pi -e ./integrations/pi/amq-bridge.ts
```

## Run pi under AMQ

The extension takes its identity from `AM_ROOT` and `AM_ME`. Start pi through
`amq coop exec`, which sets both:

```bash
amq coop exec --me pi-main pi
```

Without those variables the extension stays inactive and says so once. When
it is active it creates
`<AM_ROOT>/agents/<handle>/extensions/pi-bridge/` and keeps
`bridge.liveness` fresh while the session runs.

## Embed in a host application

A host application that runs pi with its own identity or prompt queue loads
the file that `amq-remote pi bridge-path` prints, unmodified, and calls `registerAmqBridge(pi, options)` instead of
the default export:

- `identity(ctx)` returns `{root, handle}` instead of `AM_ROOT` and `AM_ME`.
- `deliver` gives `busy(ctx)` and `send(ctx, request, report)` instead of
  `sendUserMessage`. The host reports `started` and then exactly one terminal
  outcome for each request it is handed.
- `upgrade` is the remedy `amq-remote` shows when the bridge is too old.

The contract is [the protocol's Embedding section](../../docs/pi-bridge-protocol.md#embedding).

## Answer an extension prompt from Buzz

`registerAmqBridge(pi, options)` returns a handle. An extension that shows
its own approval prompt offers the same choice in Buzz with `openDialog`:

```ts
openDialog(d: {
  title: string;
  body?: string;
  options: { label: string; deny?: boolean }[];
  signal?: AbortSignal;
}): Promise<{ by: "buzz"; index: number } | undefined>
```

The extension owns the terminal prompt and races it with `openDialog`. It
aborts `signal` when the terminal answers first, and it closes its terminal
prompt when `openDialog` resolves an answer. Buzz shows option `index` as
number `index + 1`; ✅ picks the first option and ❌ the one marked `deny`.
The promise resolves the owner's choice only after the Buzz agent verified
the owner's answer and this dialog was resolved for Buzz. It resolves
`undefined`, and never rejects, when no `/amq-remote` binding serves this pi,
when `signal` aborted, when the dialog expired, or when Buzz failed: the
terminal then keeps the decision. While an `openDialog` is open, the bridge
posts no option-less copy of a pi prompt beside it.

## Doorbell guard

While pi asks "Trust project folder?" at startup, and while an extension
prompt (`confirm`, `select`, `input`, `editor`, or a custom prompt) is open,
the extension writes `<AM_ROOT>/agents/<handle>/.wake.dialog`, and the AMQ
wake holds its doorbell instead of typing into pi. It removes the marker
when the session starts after the trust answer, when pi stops waiting on a
prompt, and at every session shutdown. The guard runs only when this
extension is loaded.

## Declare the target

Add a kind `pi` entry to the `amq-remote` manifest
(`<root>/extensions/remote/manifest.json`) with the handle pi runs as. The
root is the `AM_ROOT` of the pi process. Start pi once before `amq-remote
serve`, so the bridge directory exists when the adapter attaches.

```json
{
  "schema_version": 1,
  "layer": "remote",
  "adapters": [
    {"kind": "pi", "target": "pi-main", "config": {"handle": "pi-main"}}
  ]
}
```

Submits deliver as follow-ups only. Steering and cancellation are not
available through this bridge.

## Test

```bash
npm test
```
