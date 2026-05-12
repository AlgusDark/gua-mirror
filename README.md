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
every SAFETY_POLL_INTERVAL ──►  safety poll tick
                                       │
                                       ▼
                            GET https://api6.ipify.org
                            (with fallbacks, IPv6-only)
                                       │
                                       ▼
                            ip -6 addr replace <gua>/128 \
                                dev tun0                 \
                                preferred_lft 0
                                       │
                                       ▼
                            POST qbt /torrents/reannounce
                            (optional)
```

The container shares gluetun's network namespace, so it sees the same `tun0`
and uses the same egress as everything else behind the VPN.

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
- **Why an HTTP echo for v6 detection?** gluetun's `PUBLICIP_FILE` is
IPv4-only, and its control API doesn't expose v6. A direct echo over IPv6
is the cheapest reliable way to discover what the world sees, and as a
bonus it confirms outbound v6 actually works.
- **Why is reannounce in scope?** Without it, qBittorrent keeps announcing
the old GUA until its next scheduled reannounce (typically 30+ minutes),
leaving your client off-grid on the new IP.

## Security

The threat model is narrow: this daemon listens on no ports, and its only
privileged operation is `ip -6 addr replace` on a single interface. The
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