# Security

## Reporting a vulnerability

Please report security issues privately by opening a draft security advisory
on GitHub: <https://github.com/AlgusDark/gua-mirror/security/advisories/new>.

Do **not** open a public issue for vulnerabilities. We aim to respond within
a few days.

## Threat model

`gua-mirror` is a small daemon that runs inside a VPN-isolated container,
listens on no ports, and has one privileged operation: adding a single
deprecated `/128` IPv6 alias to a tunnel interface. The threat model is
correspondingly narrow.

### What we protect against

- **Compromised echo endpoint.** If a configured `IPV6_ECHO_ENDPOINTS` URL is
  compromised (TLS attack, hijacked domain) and returns a hostile IP, we
  reject anything that isn't a publicly-routable global unicast address
  (no loopback, link-local, ULA, multicast, IPv4-mapped, unspecified). The
  worst remaining case — a valid GUA controlled by the attacker — would only
  cause us to add an unused deprecated alias that the kernel won't use as
  an outbound source. No traffic is redirected or intercepted.
- **Resource exhaustion.** All HTTP responses are read with a 256-byte
  limit. Connections have a 5-second dial timeout and a 10-second overall
  timeout. Keep-alives are disabled so transient VPN reconnects don't leave
  pooled dead connections.
- **Privilege escalation.** The example compose file uses `cap_drop: ALL`
  plus `cap_add: NET_ADMIN` and `no-new-privileges:true`. The container
  has exactly one privileged capability — the minimum required to mutate
  IPv6 addresses on the tunnel interface via rtnetlink.

### What we do not protect against

- **A malicious operator.** `gua-mirror` is configured via environment
  variables. Anyone who can set `IPV6_ECHO_ENDPOINTS` or `QBITTORRENT_URL`
  can point us at arbitrary HTTP servers. This is by design.
- **A compromised host.** The container shares the gluetun network
  namespace. A host kernel compromise or a compromise of gluetun itself is
  out of scope.
- **Tracker/peer privacy.** The mirrored GUA is by definition the same
  public address every internet service already sees you as. We do not
  attempt to hide it or rotate it.

### What we log

`gua-mirror` writes structured JSON to stderr at the configured
`LOG_LEVEL` (default `info`).

At **info** the log includes:

- Startup configuration: the interface, file path, and echo endpoints.
- Alias state changes: `alias added`, `alias replaced`, `alias present;
  pruning extra stale aliases`, and the corresponding "shutting down" line.
- Failures: detection errors, alias-write errors, qBittorrent reannounce
  errors. None include credentials or request bodies.

At **debug** the log additionally includes:

- Per-tick trigger reasons (startup, inotify event, safety poll).
- Per-reconcile detection details, including which echo endpoint
  succeeded and the detected public IPv6 address (this is your VPN's
  public exit IP, which is already visible to every service you connect
  to).
- "alias unchanged" no-op lines on reconciles where the kernel already
  matches the desired state.

The result at default verbosity: a healthy daemon logs at startup and on
state transitions, then is quiet until something changes or fails.

The `QBITTORRENT_USERNAME` and `QBITTORRENT_PASSWORD` environment variables
are never logged, never written to disk, and never echoed back from the
qBittorrent API.

## Hardening checklist for operators

When deploying `gua-mirror`, you should:

- [ ] Use the example compose's `cap_drop: ALL` + `cap_add: NET_ADMIN` +
      `no-new-privileges:true` pattern.
- [ ] Mount gluetun's data volume **read-only** (`/gluetun:ro`) — the daemon
      only needs to read the public-IP file.
- [ ] If using qBittorrent integration over a non-localhost URL, use HTTPS.
- [ ] Keep credentials out of `compose.yaml`; use a `.env` file or a Docker
      secret.
- [ ] Review your gluetun stack's network isolation. `gua-mirror` adds no
      new network exposure but inherits everything gluetun's netns has.

## Privileged operations performed

The complete list of privileged operations `gua-mirror` performs:

1. `RTM_NEWADDR` on the tunnel interface, adding `<gua>/128` with
   `preferred_lft = 0` and a large `valid_lft` (math.MaxInt32 ~= 68
   years; see `kernel_linux.go` for why this is "forever-ish" rather
   than the wire-level `0xFFFFFFFF`). Equivalent to
   `ip -6 addr replace <gua>/128 dev <iface> valid_lft forever preferred_lft 0`.
2. `RTM_DELADDR` on the tunnel interface for **every** deprecated /128
   v6 alias that is not the current desired GUA. Equivalent to
   `ip -6 addr del <stale-gua>/128 dev <iface>` per stale address.
   Reconcile converges state in one pass, even if a previous incarnation
   of the daemon left multiple stale aliases behind.
3. `RTM_GETADDR` (read-only) on the tunnel interface, to observe the
   current set of deprecated v6 aliases before deciding whether to write.

That's it. No firewall rules, no routing changes, no sysctl
modifications. The daemon previously shelled out to the `ip` command
for (1) and (2); it now talks to rtnetlink directly via the
`vishvananda/netlink` library, with no fork/exec round trip. (3) is new
in this design: the kernel is the source of truth for the alias state,
so we always read before we write rather than trusting an in-memory
cache.

## Build provenance

Releases are built and pushed by GitHub Actions from tagged commits in this
repository. The workflow that does so is at
[`.github/workflows/release.yaml`](./.github/workflows/release.yaml).
Verify the digest of any image you pull against the digests recorded on the
matching GitHub release.
