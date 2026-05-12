//go:build linux

package alias

import (
	"fmt"
	"math"
	"net"

	"github.com/vishvananda/netlink"
)

// validLftLong is the value we set in IFA_CACHEINFO's valid lifetime to
// keep the deprecated alias around indefinitely. Two things to know:
//
//   - The vishvananda/netlink library skips emitting IFA_CACHEINFO when
//     `ValidLft > 0 || PreferedLft > 0` is false, which would let the
//     kernel default both fields to "forever" -- breaking our deprecated
//     semantics (we need `preferred_lft = 0`). So *some* positive
//     ValidLft must be set.
//
//   - The library's Addr.ValidLft is a Go `int`. On 32-bit Linux
//     (GOARCH=arm and similar), the maximum positive `int` is
//     2^31-1, which is well short of the wire-level "forever" sentinel
//     0xFFFFFFFF that iproute2's `valid_lft forever` emits. Rather than
//     pick a value that compiles only on 64-bit, we pick the largest
//     positive int that fits all supported architectures.
//
// math.MaxInt32 is ~68 years. The daemon reconciles continuously and
// would re-set the alias long before then; the lifetime only matters if
// the daemon dies and is never restarted, which is operationally
// equivalent to "forever" for this use case.
const validLftLong = math.MaxInt32

// newKernelOps returns the production netlink-backed implementation.
func newKernelOps() kernelOps {
	return &netlinkOps{}
}

// netlinkOps talks to the kernel over rtnetlink via the
// vishvananda/netlink library. It speaks the same protocol that
// iproute2's `ip` command does, so kernel-visible behavior is
// equivalent, with no fork/exec round trip per call.
type netlinkOps struct{}

func (netlinkOps) listDeprecatedV6(ifname string) ([]net.IP, error) {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return nil, fmt.Errorf("link %q: %w", ifname, err)
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return nil, fmt.Errorf("addr list: %w", err)
	}
	var out []net.IP
	for _, a := range addrs {
		// PreferedLft == 0 is the canonical "deprecated" signal from
		// netlink; checking only IFA_F_DEPRECATED in Flags works too but
		// the field is more straightforward and matches how we set it.
		if a.PreferedLft != 0 {
			continue
		}
		if a.IP == nil || a.IP.To4() != nil {
			continue
		}
		// Only consider /128 host addresses as "ours." This daemon only
		// ever writes /128, and our delete path also assumes /128: if
		// some other agent added a deprecated v6 with a wider prefix
		// (SLAAC autoconf with manual deprecation, an iproute2 call by
		// hand, a different daemon entirely), we leave it alone rather
		// than risk a delete with the wrong mask.
		if ones, bits := a.Mask.Size(); ones != 128 || bits != 128 {
			continue
		}
		// Defensive copy: the netlink library reuses backing storage in
		// some code paths and we hold onto these IPs past the call.
		ip := make(net.IP, len(a.IP))
		copy(ip, a.IP)
		out = append(out, ip)
	}
	return out, nil
}

func (netlinkOps) addDeprecatedV6(ifname string, ip net.IP) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("link %q: %w", ifname, err)
	}
	addr := &netlink.Addr{
		IPNet:       &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)},
		PreferedLft: 0,
		ValidLft:    validLftLong,
	}
	// AddrReplace sends RTM_NEWADDR with NLM_F_CREATE|NLM_F_REPLACE, so
	// it's idempotent at the kernel level even if our pre-check missed
	// the address.
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("addr replace: %w", err)
	}
	return nil
}

func (netlinkOps) deleteV6(ifname string, ip net.IP) error {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("link %q: %w", ifname, err)
	}
	addr := &netlink.Addr{
		IPNet: &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)},
	}
	if err := netlink.AddrDel(link, addr); err != nil {
		return fmt.Errorf("addr del: %w", err)
	}
	return nil
}
