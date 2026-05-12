# gua-mirror

A small daemon that mirrors a VPN tunnel's public IPv6 address (GUA) onto the
tunnel interface as a *deprecated* alias, so applications can listen on it and
announce it to peers without breaking outbound source-address selection.

It exists for one specific scenario: **NAT66 VPNs that hand out a private ULA
on the tunnel and translate to a public GUA at the exit.** The canonical
example is **AirVPN** running inside a container via
[gluetun](https://github.com/qdm12/gluetun), but anything with the same shape
should work.

## Why this exists

Some VPN providers (notably AirVPN) implement IPv6 by:

1. Assigning the client a private **Unique Local Address (ULA)** on the tunnel
  interface (e.g. `fd7d:...`).
2. Performing **NAT66** at the VPN exit, translating the ULA to a public
  **Global Unicast Address (GUA)** that's actually routable on the internet.

The consequence is that your tunnel interface only ever sees the ULA. Apps
that bind to the tunnel address bind to the ULA. Trackers, peers, and any
service that needs your *public* IPv6 (e.g. BitTorrent's `announce_ip`) get
the ULA — which is unroutable and useless.

You'd think the fix is "just add the GUA as an alias on the tunnel." It is —
but if you do it naively, the kernel picks the GUA as the source for new
outbound connections (per [RFC 6724](https://www.rfc-editor.org/rfc/rfc6724)), and the VPN server rejects
packets coming from a GUA it doesn't recognize. Your IPv6 breaks.

The fix is to add the GUA as a **deprecated** alias (`preferred_lft 0`):

- The ULA stays the source for new outbound connections.
- The GUA remains valid for inbound traffic.
- Apps can explicitly bind to the GUA when they need to.

`gua-mirror` automates this: it detects the current public GUA, sets it as a
deprecated alias on your tunnel, and (optionally) tells qBittorrent to
reannounce so trackers learn the new IP.

## How it works

```
gluetun writes its public  ──►  inotify(/gluetun/ip)
IPv4 to PUBLICIP_FILE                  │
                                       ▼
                            content changed since last seen?
                                       │ yes
every SAFETY_POLL_INTERVAL ──►  safety poll tick
                                       │
                                       ▼
                            GET https://api6.ipify.org
                            (with fallbacks, IPv6-only)
                                       │
                                       ▼
                            rtnetlink RTM_GETADDR (read tun0)
                                       │
                                       ▼
                            if observed != desired:
                                rtnetlink RTM_NEWADDR
                                  (replace, preferred_lft 0)
                                rtnetlink RTM_DELADDR (stale)
                                       │
                                       ▼
                            POST qbt /torrents/reannounce
                            (optional)
```

The container shares gluetun's network namespace, so it sees the same `tun0`
and uses the same egress as everything else behind the VPN.

Inotify events are deduplicated by file content: gluetun rewrites
`PUBLICIP_FILE` on every publicip refresh, but most refreshes don't move
the v4 address (and therefore can't have moved the v6 GUA either). The
safety poll is the fallback for the cases where the GUA changes without
a file change (NAT66 pool reassignment on the VPN exit) and as a
liveness check for the watcher itself.

## Quick start (with gluetun)

See `[docker-compose.example.yaml](./docker-compose.example.yaml)` for a full
working setup. The minimal addition to an existing gluetun stack:

```yaml
services:
  gluetun:
    image: qmcgaw/gluetun
    cap_add: [NET_ADMIN]
    devices: ["/dev/net/tun:/dev/net/tun"]
    environment:
      PUBLICIP_FILE: /gluetun/ip          # write to a shared volume
    volumes:
      - gluetun-data:/gluetun
    sysctls:
      net.ipv6.conf.all.disable_ipv6: "0"
    # ...your VPN config...

  gua-mirror:
    image: ghcr.io/algusdark/gua-mirror:latest
    network_mode: "service:gluetun"       # share gluetun's netns
    cap_add: [NET_ADMIN]
    depends_on:
      gluetun:
        condition: service_healthy
    volumes:
      - gluetun-data:/gluetun:ro
    environment:
      TUN_INTERFACE: tun0
      PUBLICIP_FILE: /gluetun/ip
      # Optional qBittorrent integration:
      QBITTORRENT_URL: http://localhost:10095
      QBITTORRENT_USERNAME: admin
      QBITTORRENT_PASSWORD: adminadmin

volumes:
  gluetun-data:
```

## Configuration


| Env var                | Default                                                            | Description                                                             |
| ---------------------- | ------------------------------------------------------------------ | ----------------------------------------------------------------------- |
| `TUN_INTERFACE`        | `tun0`                                                             | Tunnel interface to add the alias on.                                   |
| `PUBLICIP_FILE`        | `/gluetun/ip`                                                      | Path to gluetun's public-IP file. Used as an inotify trigger.           |
| `IPV6_ECHO_ENDPOINTS`  | `https://api6.ipify.org,https://ifconfig.co,https://icanhazip.com` | Comma-separated IPv6 echo endpoints, tried in order until one succeeds. |
| `SAFETY_POLL_INTERVAL` | `1h`                                                               | Fallback re-detect interval when no filesystem events arrive. Accepts Go durations. The daemon is event-driven; this is a slow safety net for cases where the watcher silently breaks. |
| `QBITTORRENT_URL`      | *(unset → disabled)*                                               | qBittorrent WebUI URL. If set, reannounce-all is sent on each change.   |
| `QBITTORRENT_USERNAME` | *(empty)*                                                          | qBittorrent username. Leave empty if "Bypass auth on localhost" is on.  |
| `QBITTORRENT_PASSWORD` | *(empty)*                                                          | qBittorrent password.                                                   |
| `LOG_LEVEL`            | `info`                                                             | One of `debug`, `info`, `warn`, `error`.                                |


## Design notes

- **Why `preferred_lft 0`?** A deprecated address is not used as a source for
new connections (RFC 6724) but remains valid for inbound traffic. This lets
the ULA stay the source AirVPN expects while the GUA becomes a usable
bind/announce target.
- **Why share gluetun's netns?** So we operate on the same `tun0` gluetun
manages, with the same egress. No separate networking required.
- **Why an HTTP echo for v6 detection?** gluetun's `PUBLICIP_FILE` only
holds the v4 exit address. gluetun's control API (`/v1/publicip/ip`) does
expose v6 as of v3.35, but it runs exactly one publicip fetch per VPN
reconnect on whatever stack its HTTP client happens to dial — there's no
separate v4+v6 split, so on most stacks the control API also returns
only v4. A direct echo over IPv6 is the cheapest reliable way to
discover what the world sees, and as a bonus it confirms outbound v6
actually works.
- **Why is reannounce in scope?** Without it, qBittorrent keeps announcing
the old GUA until its next scheduled reannounce (typically 30+ minutes),
leaving your client off-grid on the new IP.
- **Why netlink, not `ip` exec?** The kernel is the source of truth for
the alias state. Reading it via rtnetlink before each write lets the
daemon adopt an existing matching alias on restart (a true no-op
instead of an unconditional re-set) and detect external drift. The
`vishvananda/netlink` library speaks the same rtnetlink protocol that
`ip` does, so kernel-visible behavior is equivalent; talking to it
directly removes a fork/exec round trip per reconcile and trims the
runtime image (no `iproute2` userland).

## Security

The threat model is narrow: this daemon listens on no ports, and its
only privileged operation is mutating a single deprecated v6 alias on
one interface via rtnetlink. The
[example compose](./docker-compose.example.yaml) drops all capabilities and
adds back only `CAP_NET_ADMIN`. Echo-endpoint responses are validated to be
publicly-routable global unicast addresses; ULA, link-local, loopback,
multicast, and unspecified responses are rejected as a defense against a
hostile or compromised endpoint.

See [SECURITY.md](./SECURITY.md) for the full threat model and reporting
instructions.

## Limitations

- IPv6 only. IPv4 changes are not handled (your VPN client handles those).
- One alias per interface.
- qBittorrent is the only built-in app integration.
- Validated with **AirVPN + gluetun (WireGuard)**. Other NAT66 VPNs should
work but are not tested.

## Building from source

```bash
go build ./cmd/gua-mirror
```

## License

MIT — see [LICENSE](./LICENSE).