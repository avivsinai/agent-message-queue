# ADR: Two-Host Fleets

## Status

Accepted.

## Date

2026-08-20

## Context

AMQ is a daemon-free, file-based message bus. Each queue root is local.
Maildir-style `tmp -> new -> cur` delivery is crash-safe without a database or
server. Agents drain their own mailboxes. Receipts are consumer-local.

Two machines run agents that need to converse:

- Host M (Mac) runs AMQ with its own agents and its own roots.
- Host G (Grok Bot computer) runs AMQ with its own agents and its own roots.

Grok Bot is the operator on host G. It is not a remote Mac root and not an
AMQ access-control principal. A handle on G is not the same agent as the
same handle string on M. The Mac `Grok Bot.app` is that operator's local UI;
it is not host G and not an AMQ host principal.

The product boundary stays the same as the rest of AMQ: the message is
canonical. [Adapters](adapter-contract.md) emit ordinary messages; they are
not a remote workflow engine. Cross-host mail is a companion, not Core, and
not a drain of a foreign mailbox.

This ADR freezes identity, routing authority, receipt typing, and the v1
kill-list. It does not specify wake adapters or the `amq-bridge` wire protocol.

## Decision

### Two local fleets

Host M and host G each run AMQ locally. Each host owns its agents, mailbox
layout, receipts, and queue roots.

AMQ Core remains local and daemon-free on each host. The `amq` binary does
not listen on sockets and does not grow AMQ Core listeners.

### Cross-host mail is `amq-bridge`

Cross-host mail is a companion binary, `amq-bridge`, in both directions.
Callers address receiver-owned aliases. The destination host applies the
message into its own Maildir.

`amq-bridge` is not Maildir sync, not a remote `drain` of a foreign mailbox,
and not a socket inside `amq`.

Bidirectional v1 means all of:

1. **Alias send** — the sender addresses a receiver-owned alias.
2. **Crash-idempotent local apply** — the destination host commits the
   message into its own Maildir; retries do not duplicate after a crash.
3. **Reply on the same opaque thread ID** — the reply uses that thread ID
   as an opaque correlation key, not as routing authority.

Both bridge nodes dial out. Host G accepts no inbound SSH. Reachability is
operator-provided. The signed envelope and local apply path are frozen in
[the companion bridge protocol ADR](adr-bridge-protocol.md). Proven
bidirectional hops use `amq-bridge apply-file` on the destination host.
HTTPS store-and-forward remains the courier class when an operator
provisions a rendezvous; AMQ does not ship a hosted relay. Git is not the
default cross-host transport. Core has no sockets.

### Routing aliases

v1 routing aliases are receiver-owned `<host>/<agent>`. The receiving host
publishes which aliases it accepts. The sender uses those aliases.

Project, job, and session values are untrusted context. They may travel on
the message. They are never routing authority and must not select a root,
mailbox, executable, argv, or environment.

### Authorization and identity

Authorization is receiver-owned. The destination host decides what it
accepts into its Maildir.

These are never authority:

- a claimed agent name
- labels
- remote paths
- prompt text
- Grok Bot product identity

Grok Bot identity is not an AMQ ACL. Handles on G are attribution, not extra
principals.

Prompt text cannot select `--root`, argv, env, or executable.

The Grok VM is one trust domain. Durable workspace state is distinct from
replaceable packages and processes. Package installs and live process
identities are not a second trust boundary.

### Receipts stay typed

v1 receipt states stay distinct. They are not collapsed:

| State | Meaning |
| --- | --- |
| `transport_accepted` | The bridge accepted the send for transport. |
| `destination_maildir_committed` | The destination host applied the message into its Maildir. |
| consumer-local `drain` / `start` / `complete` | A consumer on that host ingested or progressed the work. |

`transport_accepted` is not `destination_maildir_committed`. Destination
commit is not consumer-local evidence. Consumer evidence may stay local in
v1; a remote party must not treat a wake, a transport ACK, or a missing
remote drain receipt as proof of consumption.

### Wake (out of scope except the kill-list)

Wake implementation is [the capability-vector ADR](adr-wake-capability-vector.md).
The kill-list still forbids dishonest wake substitutions and prompt-driven GUI
control.

### Kill-list

v1 does not:

- sync Maildirs across hosts
- remotely drain a foreign mailbox
- put sockets or listeners in the `amq` binary
- put Mac mailbox files on the Bot VM
- use git as the relay by fiat (git is not the default cross-host transport)
- collapse receipt states
- treat Grok Bot identity as an AMQ ACL
- treat claimed agent names, labels, or remote paths as authority
- let prompt text select `--root`, argv, env, or executable
- put OAuth MCP inside `amq`
- claim ACP v2
- put `--always-approve` in committed launch plans
- hold a Buzz nsec inside AMQ
  - The `amq-acp` companion instead gates inbound `BUZZ_ACP_AGENTS`/`RESPOND_TO`
    on the harness holding a Buzz NIP-PL kind:30350 lease with a quota-1
    deployment-identity profile (block/buzz#5667); it does not invent one.
- silently downgrade inject→notify or submit→prefill
- scrape ChatGPT through Accessibility
- run generic `osascript` from prompt text

## Consequences

- Each host remains a complete local AMQ installation. Cross-host mail is
  optional and alias-only.
- Skills and launch plans teach receiver-owned `<host>/<agent>` aliases, not
  foreign `--root` values and not remote drain.
- Companion `amq-bridge` exists beside `amq`, as `amq-keepalive` does.
  Core stays a local CLI.
- Receipt APIs and docs name transport, destination commit, and consumer
  evidence separately. Callers that need destination commit wait for
  `destination_maildir_committed`, not for `transport_accepted`.
- Adapters continue to emit local messages. They do not become a remote
  workflow engine or a foreign-host drain.
- Inbound SSH to G, git-by-default, and sockets in Core stay out.
- Wake adapters, when specified elsewhere, advertise real capability. They
  do not substitute a weaker path and do not drive GUI automation from prompt
  text.
