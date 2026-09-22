# Documentation

Start with the [README walkthrough](../README.md#getting-started) to connect
two agents. This index points to the guide for each task; command and flag
syntax comes from the [generated CLI reference](cli.md).

## Use AMQ

- [Install and update](../INSTALL.md) — binary and skill installation,
  platform support, and companion binaries.
- [Co-op operations](../COOP.md) — launch agents, use sessions, exchange
  messages, and run supervised notifications.
- [Session routing](session-routing.md) — identities, roots, cross-session
  access, and worktree isolation.
- [Wake operations](wake-operations.md) — inspect, repair, recover, and
  retire notifications safely.
- [Trace a message](trace.md) — delivery evidence and its limits.
- [Keepalive](amq-keepalive.md) — attach notifications to terminal targets
  and manage reattachment.
- [Security](../SECURITY.md) — the local trust model and vulnerability reporting.

## Connect tools and hosts

- [Bridge companion](../cmd/amq-bridge/README.md) — exchange messages between hosts.
- [ACP companion](../cmd/amq-acp/README.md) — deliver ACP prompts to an AMQ inbox.
- [Adapter contract](adapter-contract.md) — convert external events into messages.
- [Launch API](launch-api.md) — version negotiation and prepare/apply integration.
- [Extension metadata](adr-layer-extensions.md) — store layer-owned data without
  changing AMQ's mailbox format.

## Understand the design

- **Routing:** [session-guard decisions](w2a-session-guard-table.md).
- **Launch:** [recovery and ownership](launch-recovery.md).
- **Wake:** [state invariants](wake-state-invariants.md),
  [lifecycle protocol](wake-lifecycle.md),
  [doorbell acknowledgement](wake-doorbell-acknowledgement.md), and
  [adapter capabilities](adr-wake-capability-vector.md).
- **Cross-host delivery:** [two-host architecture](adr-two-host-fleets.md) and
  [bridge protocol](adr-bridge-protocol.md).
- **Remote sessions:** [remote-control design](adr-remote-control.md) and
  [compatibility evidence](remote-compat.md). A design describes the contract;
  a capability requires the evidence stated in the compatibility reference.
  Point-in-time research records live under [research/](research/); they grade
  the evidence behind a row and are not current-truth statements.

## Contribute or configure an agent

[Contributing](../CONTRIBUTING.md) covers development. [CLAUDE.md](../CLAUDE.md)
contains repository-specific instructions for coding agents, not the public
CLI manual. The published [AMQ skill](../skills/amq-cli/SKILL.md) teaches agents
how to use AMQ; the [spec skill](../skills/amq-spec/SKILL.md) adds a collaborative
design workflow. [Release notes](../CHANGELOG.md) record version history.
