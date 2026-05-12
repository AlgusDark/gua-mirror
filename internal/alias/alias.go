// Package alias manages a single deprecated /128 IPv6 alias on a network
// interface.
//
// The kernel is the source of truth. The manager reads the interface's
// current deprecated v6 aliases on every reconcile, compares them to the
// caller's desired address, and applies the minimum change needed. This
// gives three properties the previous exec-based design did not:
//
//   - Self-heal after restart. If a previous daemon instance set the same
//     address and the process restarted, the kernel still has the alias and
//     Reconcile is a true no-op. Operator-visible side effect: restart
//     stops being a noisy "alias set" log line followed by an immediate
//     "alias unchanged".
//
//   - Drift detection. If something external removed or changed the alias
//     between reconciles, the manager observes that on the next call and
//     restores intent.
//
//   - Testability. The kernel interaction is a small unexported interface
//     with two adapters (netlink in production, in-memory in tests), so
//     idempotency and ordering rules are exercised by unit tests without
//     root or a real network namespace.
//
// This file is the pure logic and is build-tag-free. The production
// netlink adapter lives in kernel_linux.go; a non-functional stub lives
// in kernel_other.go so contributors on macOS/Windows can still
// `go vet` and `go build` the repo.
package alias

import (
	"context"
	"fmt"
	"log/slog"
	"net"
)

// Manager keeps the kernel's deprecated /128 v6 alias on Interface in sync
// with the desired address most recently passed to Reconcile.
type Manager struct {
	Interface string
	Log       *slog.Logger

	ops kernelOps
}

// NewManager returns a Manager bound to ifname using the platform's
// default kernel adapter (netlink on Linux, a non-functional stub
// elsewhere). It does not touch the kernel until Reconcile is called.
func NewManager(ifname string, log *slog.Logger) *Manager {
	return &Manager{
		Interface: ifname,
		Log:       log,
		ops:       newKernelOps(),
	}
}

// Reconcile makes the deprecated /128 v6 alias on Interface equal to
// desired. It is idempotent: if the kernel already has exactly the
// desired alias deprecated, no kernel mutation occurs and the call is a
// no-op (logged at debug).
//
// If the kernel has a different deprecated /128 v6 alias, Reconcile
// replaces it: the new address is added first, then the old one is
// removed, so callers never observe a window with no deprecated alias.
// Removing the old address is best-effort -- if the interface was torn
// down between the two operations the deletion will fail harmlessly and
// the warning is informational.
//
// Reconcile validates desired is a non-nil v6 address. The detect
// package is responsible for narrower public-GUA validation; this is a
// defense-in-depth check that we never call netlink with garbage.
func (m *Manager) Reconcile(ctx context.Context, desired net.IP) error {
	if desired == nil {
		return fmt.Errorf("nil IP")
	}
	if desired.To4() != nil || desired.To16() == nil {
		return fmt.Errorf("not an IPv6 address: %v", desired)
	}

	observed, err := m.ops.listDeprecatedV6(m.Interface)
	if err != nil {
		return fmt.Errorf("list deprecated v6 on %s: %w", m.Interface, err)
	}

	// Partition observed addresses into "this is what we want" and "this
	// has to go." In a healthy stack there's at most one of each; we
	// handle the multi-stale case defensively because incomplete
	// cleanup from a previous incarnation or external interference can
	// leave several deprecated v6 aliases on the interface, and the
	// daemon's job is to converge state on each reconcile.
	var matching net.IP
	var stale []net.IP
	for _, ip := range observed {
		if ip.Equal(desired) {
			matching = ip
			continue
		}
		stale = append(stale, ip)
	}

	if matching != nil && len(stale) == 0 {
		m.Log.Debug("alias unchanged",
			"interface", m.Interface, "address", desired.String())
		return nil
	}

	// Add desired first (idempotent at the netlink layer via
	// NLM_F_REPLACE); only then delete any stale aliases, so callers
	// binding to the new GUA never observe a window with no deprecated
	// alias.
	if matching == nil {
		if err := m.ops.addDeprecatedV6(m.Interface, desired); err != nil {
			return fmt.Errorf("add %s/128 on %s: %w",
				desired, m.Interface, err)
		}
		switch len(stale) {
		case 0:
			m.Log.Info("alias added",
				"interface", m.Interface, "address", desired.String())
		case 1:
			m.Log.Info("alias replaced",
				"interface", m.Interface,
				"from", stale[0].String(), "to", desired.String())
		default:
			m.Log.Info("alias replaced; pruning extra stale aliases",
				"interface", m.Interface,
				"to", desired.String(), "stale_count", len(stale))
		}
	} else {
		// matching is present but stale exists -- nothing to add, but
		// extras must be pruned. Log explicitly so this state is visible
		// at info level rather than only appearing as warn-on-failure.
		m.Log.Info("alias present; pruning extra stale aliases",
			"interface", m.Interface,
			"address", desired.String(), "stale_count", len(stale))
	}
	for _, ip := range stale {
		if err := m.ops.deleteV6(m.Interface, ip); err != nil {
			// Best-effort: the interface may have been torn down by the
			// VPN container between the add and the delete, taking the
			// stale address with it. Other stales (if any) are still
			// attempted; one failure doesn't stop the loop.
			m.Log.Warn("remove stale alias failed",
				"interface", m.Interface, "address", ip.String(), "err", err)
		}
	}
	_ = ctx // reserved for future netlink contexts; current API is synchronous
	return nil
}

// kernelOps is the internal seam used to swap the production netlink
// adapter for an in-memory fake in tests. The interface is intentionally
// small: the manager only needs three operations and they map 1:1 to
// netlink primitives.
type kernelOps interface {
	listDeprecatedV6(ifname string) ([]net.IP, error)
	addDeprecatedV6(ifname string, ip net.IP) error
	deleteV6(ifname string, ip net.IP) error
}
