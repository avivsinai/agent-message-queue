# pi

`amq-bridge.ts` is a [pi](https://github.com/earendil-works/pi) extension. It
makes a live pi session reachable by `amq-remote` target kind `pi`: a remote
request arrives in the session as a follow-up user message, and `amq-remote`
sees its admission, its completion, and session changes. The wire contract is
[the pi bridge protocol](../../docs/pi-bridge-protocol.md).

## Install

Install the package from a release tag:

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
