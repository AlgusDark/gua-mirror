<!--
This file is the domain glossary for gua-mirror. Read it before changing
code: the daemon is small, but every term here has been chosen carefully
because the underlying networking concepts have multiple plausible names
in different communities (Linux kernel, RFC 4861/6724, BitTorrent, VPN
vendor docs). Aligning on the names below prevents drift and re-litigation.
-->

# Context

`gua-mirror` exists to make a NAT66 VPN's public IPv6 *usable* by
applications running inside the VPN's network namespace. Its single
responsibility is to mirror the VPN exit's current public IPv6 GUA onto
the tunnel interface as a deprecated alias, and to notify integrated
applications (currently only qBittorrent) when that alias changes.

## Language

**GUA** (Global Unicast Address):
A publicly routable IPv6 address (RFC 4291), as opposed to a ULA, link-local,
or other reserved range. The "G" in `gua-mirror`.
_Avoid_: "public address" (ambiguous with NAT v4), "external IPv6".

**ULA** (Unique Local Address):
A private IPv6 address in `fc00::/7` (RFC 4193). On a NAT66 VPN like AirVPN,
the tunnel interface receives a ULA on the client side; the VPN exit
translates packets to a **GUA** for transit on the public internet.
_Avoid_: "private IPv6", "internal IPv6".

**NAT66**:
Network Address Translation between two IPv6 prefixes (typically ULA → GUA).
The defining problem this daemon addresses: applications inside the
tunnel see only the **ULA**, but trackers and peers need the **GUA**.
_Avoid_: "IPv6 NAT" (overloaded with NAT64).

**Deprecated alias**:
An IPv6 address present on an interface but marked with `preferred_lft = 0`,
which per RFC 6724 prevents the kernel from selecting it as the source
address for new outbound connections, while keeping it valid for inbound
traffic. The mechanism that lets us announce the **GUA** without breaking
the VPN's strict-RPF source-address expectation.
_Avoid_: "secondary address", "passive alias", "/128 alias" (the /128 is
mechanical, not the defining property).

**Desired alias**:
The GUA that `gua-mirror` has decided should currently be the deprecated
alias on the tunnel interface. The output of detection.
_Avoid_: "target alias", "intended address".

**Observed alias**:
What the kernel reports is *actually* on the tunnel interface right now.
The input to reconciliation.
_Avoid_: "current alias" (ambiguous with the in-memory cache that
preceded netlink-as-source-of-truth).

**Reconcile (verb)**:
Make the **observed** state equal the **desired** state with the minimum
necessary kernel mutations. Used at two scopes in this codebase:
- `alias.Manager.Reconcile` reconciles a single deprecated alias on the
  tunnel interface.
- The orchestration loop in `cmd/gua-mirror` reconciles the *whole*
  pipeline per trigger: detect → alias reconcile → reannounce.
_Avoid_: "sync", "apply", "update" — those don't carry the "diff first,
mutate minimally" semantics.

**Trigger**:
A signal that says "the **desired alias** may have changed; rerun
detection." Two sources: a content change on `PUBLICIP_FILE`, or a
periodic safety poll.
_Avoid_: "event", "tick" (used elsewhere for raw channel sends).

**Safety poll**:
The periodic trigger that fires when no content change has arrived in
`SAFETY_POLL_INTERVAL`. Distinct from the inotify trigger because the
VPN exit can reassign the **GUA** without rewriting `PUBLICIP_FILE`
(which holds only IPv4), and because the watcher itself could fail
silently. The poll exists for those two cases, not as a primary detection
mechanism.
_Avoid_: "watchdog" (suggests recovery actions; this is just a trigger).

## Relationships

- A **VPN exit** publishes a **GUA** that applications need to announce.
- The tunnel interface receives only a **ULA** because of **NAT66**.
- `gua-mirror` detects the current **GUA** and writes it onto the tunnel
  interface as a **deprecated alias**.
- The **deprecated alias** is the **observed alias** when read back from
  the kernel and the **desired alias** when returned by the detector
  and passed into `alias.Manager.Reconcile`. The daemon holds no
  durable cache of either; both are computed fresh on each reconcile.
- The orchestration loop fires once per **trigger**, reconciling
  observed to desired in two stages (alias, then qBittorrent reannounce).

## Example dialogue

> **Dev:** "When the VPN reconnects, gluetun rewrites `PUBLICIP_FILE`.
> Should we always reconcile on that event?"
>
> **Maintainer:** "Only if the file content changed -- gluetun rewrites it
> on every publicip refresh, even when the **GUA** didn't move. We dedup
> by content. The **safety poll** handles the case where the **GUA**
> changes without a file change, which happens on NAT66 pool
> reassignment."

> **Dev:** "Why deprecated and not just a regular alias?"
>
> **Maintainer:** "If the **GUA** is a regular alias on the tunnel, the
> kernel picks it as the source for new outbound connections (RFC 6724),
> and the VPN exit rejects packets from a GUA it doesn't recognize.
> Deprecated means the kernel keeps it valid for inbound but won't pick
> it as a source. The **ULA** remains the source as expected."

## Flagged ambiguities

- "Public IP" alone could mean v4 or v6. We use **GUA** for the v6 sense
  and qualify "v4 public IP" explicitly when needed.
- "Current" was historically used in `alias.Manager.Current()` to mean
  the in-memory cache of the last-set alias. We removed that method when
  the kernel became the source of truth; if a future reviewer reaches
  for "current" again, prefer **observed** or **desired**.
