# amq-remote relay connection

`amq-remote serve` connects to a Buzz relay for each share in the manifest's
`relay` block. This page holds the rules of that connection. Configuring a
share, the owner step, and the owner-facing surfaces are in
[the amq-remote reference](../cmd/amq-remote/README.md#relay).

## Authentication

`serve` keeps one connection per share. It answers the relay's NIP-42
challenge with a kind 22242 event signed by the body key, carrying exactly
one enrolled NIP-OA tag: the owner's kind 1059 grant. The connection counts
as authenticated only after the relay's positive OK for that event. On each
connect, and every minute while connected, `serve` re-reads the enrolled
generation, so a renewal is picked up. The connection closes at the grant's
signed expiry. An enrolled owner that differs from `owner_pubkey` stops
authentication. A lost connection reconnects with backoff of 1 to 30
seconds.

At NIP-42 AUTH, a Buzz relay verifies the owner signature on the NIP-OA tag
and refuses self-attestation. A relay may also refuse a grant whose
`created_at` bounds have passed; a relay that does not check them accepts a
signed grant until the owner or the body key is removed from the relay. No
relay checks `kind=` conditions at connection admission, so `amq-remote`
enforces the per-kind limit itself.

## Owner DM surface

The DM surface opens only when the relay's own key signs the channel's
NIP-29 membership (kind 39002) as exactly the owner and the body, and its
metadata (kind 39000) as private and of type `dm`. `serve` reads the
membership and the native session again every minute and closes the surface
on the first read that no longer verifies. These are stored relay snapshots,
not a read of current membership, so a change can show late; no bound on the
delay is claimed.

The body signs a kind 9 or 40003 event only when the enrolled generation has
the owner's grant for that kind. It attaches that grant as the event's
NIP-OA tag. The relay does not enforce these grants; `serve` does, and it
checks them again before it sends owed output. Each owner event is claimed
once and its outcome recorded, so a redelivered DM never runs again, even
after a busy rejection. Output owed from an earlier channel or body is kept,
never redirected.

## Owed output

A relay refusal of a DM output describes one attempt, so the output stays
owed and is retried with a backoff of 30 seconds, doubling to 10 minutes.
Order is kept per request: while a request's earlier output is owed, its
later outputs wait; other requests' outputs still go out. `doctor` reports
each refused output under `publication_refused`.

## Activity frames

Codex deltas for one item are merged into one kind 24200 frame per 100 ms,
so a fast stream stays well under the 100 frames per second cap.
