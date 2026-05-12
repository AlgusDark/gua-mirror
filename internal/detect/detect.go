// Package detect discovers the current public IPv6 address by querying
// echo endpoints over IPv6. The HTTP client is pinned to tcp6 so we always
// see the address the world sees us as via the VPN's NAT66 exit, regardless
// of any deprecated GUA alias we may have already added.
package detect

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Detector queries a list of echo endpoints to find the current public v6.
type Detector struct {
	Endpoints []string
	Client    *http.Client
	Log       *slog.Logger
}

// New returns a Detector with an IPv6-only HTTP client.
func New(endpoints []string, log *slog.Logger) *Detector {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _ /*network*/, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp6", addr)
		},
		// Don't reuse connections; we want a fresh dial every time so transient
		// VPN reconnects don't leave us stuck on a dead pooled conn.
		DisableKeepAlives: true,
	}
	return &Detector{
		Endpoints: endpoints,
		Client: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		},
		Log: log,
	}
}

// Detect tries each endpoint in order and returns the first valid IPv6 address.
// Returns an error only if every endpoint fails.
func (d *Detector) Detect(ctx context.Context) (net.IP, error) {
	var lastErr error
	for _, ep := range d.Endpoints {
		ip, err := d.query(ctx, ep)
		if err != nil {
			d.Log.Debug("v6 echo failed", "endpoint", ep, "err", err)
			lastErr = err
			continue
		}
		d.Log.Debug("v6 echo succeeded", "endpoint", ep, "ip", ip.String())
		return ip, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no endpoints configured")
	}
	return nil, fmt.Errorf("all v6 echo endpoints failed: %w", lastErr)
}

func (d *Detector) query(ctx context.Context, endpoint string) (net.IP, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	// Some echo services (notably ifconfig.co) return HTML by default and
	// only switch to plain text when they see a curl-ish user agent.
	req.Header.Set("User-Agent", "gua-mirror/curl-compatible")
	req.Header.Set("Accept", "text/plain")

	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return nil, err
	}
	return ParseEchoResponse(string(body))
}

// ParseEchoResponse extracts an IPv6 address from a plain-text echo response,
// rejecting anything that isn't a routable public GUA. This is defense in
// depth against a compromised or hostile echo endpoint: even if an attacker
// controls the response, the worst they can do is fail validation. Adding a
// loopback/ULA/link-local/multicast address as an interface alias would be
// either useless or actively misleading.
//
// Exposed for testing.
func ParseEchoResponse(s string) (net.IP, error) {
	ipStr := strings.TrimSpace(s)
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, fmt.Errorf("not a valid IP: %q", ipStr)
	}
	if ip.To4() != nil {
		return nil, fmt.Errorf("got IPv4 from IPv6-only echo: %s", ipStr)
	}
	// IsGlobalUnicast() returns false for loopback, multicast, unspecified,
	// and link-local. IsPrivate() additionally returns true for ULA (fc00::/7).
	// We want a publicly routable address, so reject both categories.
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return nil, fmt.Errorf("not a public IPv6 address: %s", ipStr)
	}
	return ip, nil
}
