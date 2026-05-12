package alias

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
)

// TestReconcileAddsWhenNoDeprecatedAliasPresent exercises the empty-slate
// path: the manager is constructed, the kernel has nothing deprecated on
// the target interface, and Reconcile must result in exactly one add and
// zero deletes.
func TestReconcileAddsWhenNoDeprecatedAliasPresent(t *testing.T) {
	desired := net.ParseIP("2606:4700:4700::1111")
	ops := &fakeOps{}
	m := newManagerWithOps("tun0", ops)

	changed, err := m.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !changed {
		t.Errorf("expected changed=true after an add, got false")
	}

	if ops.adds != 1 || ops.dels != 0 {
		t.Errorf("expected adds=1 dels=0, got adds=%d dels=%d", ops.adds, ops.dels)
	}
	if got := ops.list("tun0"); len(got) != 1 || !got[0].Equal(desired) {
		t.Errorf("kernel state = %v, want exactly [%s]", got, desired)
	}
}

// TestReconcileAdoptsExistingMatchingAlias is the load-bearing
// adopt-on-restart property: the kernel already has the desired alias
// deprecated (because a previous gua-mirror set it and we just
// restarted), and Reconcile must observe it as already-satisfied with no
// kernel mutations.
func TestReconcileAdoptsExistingMatchingAlias(t *testing.T) {
	desired := net.ParseIP("2606:4700:4700::1111")
	ops := &fakeOps{addrs: map[string][]net.IP{"tun0": {desired}}}
	m := newManagerWithOps("tun0", ops)

	changed, err := m.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Adopt-on-restart: kernel state did not move. This is the property
	// the reannounce gate depends on -- if changed were true here the
	// daemon would hammer trackers on every restart.
	if changed {
		t.Errorf("expected changed=false on adopt-on-restart, got true")
	}

	if ops.adds != 0 || ops.dels != 0 {
		t.Errorf("expected zero kernel mutations, got adds=%d dels=%d",
			ops.adds, ops.dels)
	}
}

// TestReconcileReplacesStaleAlias is the in-flight-drift case: the
// kernel has a deprecated alias that is no longer desired (the GUA
// changed). Reconcile must add the new one, then delete the old one --
// in that order, so the bind/announce target is continuously available.
func TestReconcileReplacesStaleAlias(t *testing.T) {
	stale := net.ParseIP("2001:db8::1")
	desired := net.ParseIP("2001:db8::2")
	ops := &fakeOps{addrs: map[string][]net.IP{"tun0": {stale}}}
	m := newManagerWithOps("tun0", ops)

	changed, err := m.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !changed {
		t.Errorf("expected changed=true after replace, got false")
	}

	if ops.adds != 1 || ops.dels != 1 {
		t.Errorf("expected adds=1 dels=1, got adds=%d dels=%d", ops.adds, ops.dels)
	}
	// Ordering: the add must precede the delete so callers binding to
	// the new GUA never observe a window with no deprecated alias.
	if ops.opLog[0] != "add 2001:db8::2" || ops.opLog[1] != "del 2001:db8::1" {
		t.Errorf("op order = %v, want add before del", ops.opLog)
	}
	if got := ops.list("tun0"); len(got) != 1 || !got[0].Equal(desired) {
		t.Errorf("kernel state = %v, want exactly [%s]", got, desired)
	}
}

// TestReconcileRemovesAllStaleAliases guards a property earlier
// versions of this code got wrong: the cleanup path must remove every
// deprecated alias that isn't the desired one, not just the first one
// encountered. A previous incarnation of the daemon may have left
// multiple aliases behind (interrupted reconcile, container restart
// mid-update); each subsequent Reconcile must converge state in one
// pass, not N.
func TestReconcileRemovesAllStaleAliases(t *testing.T) {
	stale1 := net.ParseIP("2001:db8::a")
	stale2 := net.ParseIP("2001:db8::b")
	stale3 := net.ParseIP("2001:db8::c")
	desired := net.ParseIP("2001:db8::d")
	ops := &fakeOps{addrs: map[string][]net.IP{"tun0": {stale1, stale2, stale3}}}
	m := newManagerWithOps("tun0", ops)

	changed, err := m.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !changed {
		t.Errorf("expected changed=true after multi-stale prune, got false")
	}

	if ops.adds != 1 || ops.dels != 3 {
		t.Errorf("expected adds=1 dels=3, got adds=%d dels=%d", ops.adds, ops.dels)
	}
	if got := ops.list("tun0"); len(got) != 1 || !got[0].Equal(desired) {
		t.Errorf("kernel state = %v, want exactly [%s]", got, desired)
	}
}

// TestReconcilePrunesExtraStaleWhenDesiredAlreadyPresent is the
// rare-but-real case where the kernel already has the desired alias
// *and* one or more stale extras. Reconcile must not add (the alias is
// already there) but must still remove the extras.
func TestReconcilePrunesExtraStaleWhenDesiredAlreadyPresent(t *testing.T) {
	desired := net.ParseIP("2001:db8::d")
	stale := net.ParseIP("2001:db8::e")
	ops := &fakeOps{addrs: map[string][]net.IP{"tun0": {desired, stale}}}
	m := newManagerWithOps("tun0", ops)

	changed, err := m.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// Stale was pruned -- kernel state moved, even though "desired" was
	// already present. Callers should still see changed=true here so
	// they can reannounce (the cleanup itself shouldn't normally happen,
	// but if it did, qBT was likely also out of sync).
	if !changed {
		t.Errorf("expected changed=true after pruning extras, got false")
	}

	if ops.adds != 0 || ops.dels != 1 {
		t.Errorf("expected adds=0 dels=1, got adds=%d dels=%d", ops.adds, ops.dels)
	}
	if got := ops.list("tun0"); len(got) != 1 || !got[0].Equal(desired) {
		t.Errorf("kernel state = %v, want exactly [%s]", got, desired)
	}
}

// TestReconcileToleratesBestEffortDeleteFailure documents the policy
// that a failed delete of the stale alias does not fail the whole
// reconcile. The interface may have been torn down between the add and
// the delete (common during VPN reconnects), in which case the kernel
// already cleared the stale address.
func TestReconcileToleratesBestEffortDeleteFailure(t *testing.T) {
	stale := net.ParseIP("2001:db8::1")
	desired := net.ParseIP("2001:db8::2")
	ops := &fakeOps{
		addrs:     map[string][]net.IP{"tun0": {stale}},
		delErrors: errors.New("kernel: interface torn down"),
	}
	var buf threadSafeBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	m := &Manager{Interface: "tun0", Log: log, ops: ops}

	changed, err := m.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The add succeeded even though the best-effort delete failed --
	// kernel state still moved (new alias is present), so changed must
	// be true.
	if !changed {
		t.Errorf("expected changed=true despite best-effort delete failure, got false")
	}
	if !strings.Contains(buf.String(), "remove stale alias failed") {
		t.Errorf("expected warning in log, got: %s", buf.String())
	}
}

// TestReconcileRejectsNonIPv6 is the defense-in-depth check. The detect
// package guarantees we only see public GUAs, but Reconcile itself
// rejects non-v6 inputs so a future regression in detection cannot
// produce a v4 alias attempt that would have unclear netlink semantics.
func TestReconcileRejectsNonIPv6(t *testing.T) {
	ops := &fakeOps{}
	m := newManagerWithOps("tun0", ops)

	cases := []net.IP{
		nil,
		net.ParseIP("1.2.3.4"),
		net.ParseIP("::ffff:1.2.3.4"), // v4-mapped v6
	}
	for _, ip := range cases {
		changed, err := m.Reconcile(context.Background(), ip)
		if err == nil {
			t.Errorf("expected error for %v, got nil", ip)
		}
		if changed {
			t.Errorf("rejected input must not report changed=true, ip=%v", ip)
		}
	}
	if ops.adds != 0 || ops.dels != 0 {
		t.Errorf("rejected inputs should not touch kernel, got adds=%d dels=%d",
			ops.adds, ops.dels)
	}
}

// newManagerWithOps is a test helper that constructs a Manager bypassing
// the platform-default kernel adapter. Tests should not depend on which
// platform they run on.
func newManagerWithOps(ifname string, ops kernelOps) *Manager {
	return &Manager{
		Interface: ifname,
		Log:       slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		ops:       ops,
	}
}

// fakeOps is an in-memory kernelOps that records mutations and exposes
// the current state so tests can assert on it. Concurrency-safe so the
// race detector is happy if any future test exercises Reconcile
// concurrently.
type fakeOps struct {
	mu        sync.Mutex
	addrs     map[string][]net.IP
	adds      int
	dels      int
	opLog     []string
	delErrors error
}

func (f *fakeOps) listDeprecatedV6(ifname string) ([]net.IP, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addrs == nil {
		return nil, nil
	}
	src := f.addrs[ifname]
	out := make([]net.IP, len(src))
	copy(out, src)
	return out, nil
}

func (f *fakeOps) addDeprecatedV6(ifname string, ip net.IP) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addrs == nil {
		f.addrs = map[string][]net.IP{}
	}
	// AddrReplace semantics: when the address is already present the
	// kernel updates it in place. We model that as a true no-op so test
	// assertions on adds/dels accurately reflect kernel-side
	// idempotency. (Earlier versions of this fake counted these as
	// "adds" even though no slot was created, which would make a future
	// "idempotent replace" test pass while measuring the wrong thing.)
	for _, existing := range f.addrs[ifname] {
		if existing.Equal(ip) {
			return nil
		}
	}
	f.addrs[ifname] = append(f.addrs[ifname], ip)
	f.adds++
	f.opLog = append(f.opLog, "add "+ip.String())
	return nil
}

func (f *fakeOps) deleteV6(ifname string, ip net.IP) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErrors != nil {
		f.dels++
		f.opLog = append(f.opLog, "del "+ip.String()+" (err)")
		return f.delErrors
	}
	if f.addrs == nil {
		return nil
	}
	keep := f.addrs[ifname][:0]
	for _, existing := range f.addrs[ifname] {
		if !existing.Equal(ip) {
			keep = append(keep, existing)
		}
	}
	f.addrs[ifname] = keep
	f.dels++
	f.opLog = append(f.opLog, "del "+ip.String())
	return nil
}

func (f *fakeOps) list(ifname string) []net.IP {
	out, _ := f.listDeprecatedV6(ifname)
	return out
}

// threadSafeBuffer is the same helper used in trigger tests; duplicated
// here to keep packages independent and avoid a test-only shared dep.
type threadSafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *threadSafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *threadSafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
