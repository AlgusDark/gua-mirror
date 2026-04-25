// Package alias manages a single deprecated /128 IPv6 alias on a network
// interface. We shell out to iproute2 because it's the exact same command
// an operator would run by hand, which keeps behavior obvious and avoids a
// netlink library dependency that quirks differently between versions.
package alias

import (
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"strings"
)

// Manager remembers the last alias it set so it can replace cleanly on change.
type Manager struct {
	Interface string
	Log       *slog.Logger

	current net.IP
}

// NewManager returns a Manager. It does not touch the interface until Set.
func NewManager(ifname string, log *slog.Logger) *Manager {
	return &Manager{Interface: ifname, Log: log}
}

// Set ensures `ip/128` is present on the interface as a deprecated address
// (preferred_lft=0, valid_lft=forever). If a different alias was previously
// set by this Manager, it is removed.
//
// Idempotent: calling Set with the unchanged current address is a no-op.
func (m *Manager) Set(ip net.IP) error {
	if ip == nil {
		return fmt.Errorf("nil IP")
	}
	if ip.To4() != nil || ip.To16() == nil {
		return fmt.Errorf("not an IPv6 address: %v", ip)
	}
	if m.current != nil && m.current.Equal(ip) {
		return nil
	}

	cidr := ip.String() + "/128"
	if err := run("ip", "-6", "addr", "replace", cidr,
		"dev", m.Interface,
		"valid_lft", "forever",
		"preferred_lft", "0",
	); err != nil {
		return fmt.Errorf("set alias %s on %s: %w", cidr, m.Interface, err)
	}
	m.Log.Info("alias set", "interface", m.Interface, "address", ip.String())

	if m.current != nil && !m.current.Equal(ip) {
		oldCIDR := m.current.String() + "/128"
		if err := run("ip", "-6", "addr", "del", oldCIDR, "dev", m.Interface); err != nil {
			// Stale alias removal is best-effort: the interface may have been
			// torn down by gluetun underneath us, taking the address with it.
			m.Log.Warn("remove stale alias failed",
				"address", m.current.String(), "err", err)
		} else {
			m.Log.Info("alias removed", "interface", m.Interface, "address", m.current.String())
		}
	}
	m.current = ip
	return nil
}

// Current returns the last alias successfully set, or nil if none.
func (m *Manager) Current() net.IP {
	return m.current
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)",
			name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
