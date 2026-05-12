package trigger

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRunEmitsOnContentChange covers the wiring: a real content change
// arriving via fsnotify must produce a downstream tick. We seed the
// file before starting Run so the startup tick is the only "first
// content observed" event, then write a different value and expect a
// post-write tick.
//
// We intentionally do NOT assert the *negative* (rewrite with the same
// content produces no tick) via fsnotify, because os.WriteFile opens
// with O_TRUNC, which on Linux/inotify fires a Write event with the
// file briefly empty in between truncate and the data write. That
// empty-window read makes a "rewrite-then-quiet" assertion structurally
// racy across fsnotify backends (kqueue often coalesces; inotify
// doesn't). The dedup contract itself is exercised deterministically
// by the readTrimmed-level tests below.
func TestRunEmitsOnContentChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ip")
	if err := os.WriteFile(path, []byte("45.87.213.83\n"), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan struct{}, 4)
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Order matters: cancel runs first, then Wait collects the goroutine.
	done := runAsync(t, ctx, path, 250*time.Millisecond, ticks, log)
	defer done.Wait()
	defer cancel()

	expectTick(t, ticks, "startup")

	if err := os.WriteFile(path, []byte("203.0.113.7\n"), 0o644); err != nil {
		t.Fatalf("change: %v", err)
	}
	expectTick(t, ticks, "after content change")
}

// TestRunEmitsOnFirstObservableContent covers the legitimate first-boot
// race: the directory exists but the file does not yet, then gluetun
// writes the file. We must reconcile when the file first appears.
func TestRunEmitsOnFirstObservableContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ip")

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan struct{}, 4)
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	done := runAsync(t, ctx, path, 250*time.Millisecond, ticks, log)
	defer done.Wait()
	defer cancel()

	expectTick(t, ticks, "startup")

	if err := os.WriteFile(path, []byte("45.87.213.83\n"), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	expectTick(t, ticks, "first observable content")
}

// TestReadTrimmedCapsAtMaxBytes is the bounded-read defense-in-depth
// check. A writer that emits a payload larger than we ever expect for
// an IP file must not cause us to allocate unbounded memory. We assert
// the property directly on readTrimmed rather than through fsnotify,
// because real fsnotify+file-truncate-write sequences can interleave
// events with reads in ways that vary by backend (kqueue coalesces
// rewrites of identical content; inotify does not), and that
// interleaving would make a Run-level assertion flaky cross-platform
// for reasons unrelated to the read cap.
func TestReadTrimmedCapsAtMaxBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ip")

	big := bytes.Repeat([]byte{'X'}, 1<<20) // 1 MiB
	if err := os.WriteFile(path, big, 0o644); err != nil {
		t.Fatalf("write big: %v", err)
	}

	got, err := readTrimmed(path)
	if err != nil {
		t.Fatalf("readTrimmed: %v", err)
	}
	if len(got) != maxPublicIPFileBytes {
		t.Errorf("len(readTrimmed) = %d, want %d (cap)",
			len(got), maxPublicIPFileBytes)
	}
}

// TestReadTrimmedTrimsSurroundingWhitespace pins the trim contract
// independently of fsnotify wiring. Trimming guards against spurious
// reconciles when a writer changes only trailing newlines.
func TestReadTrimmedTrimsSurroundingWhitespace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ip")
	if err := os.WriteFile(path, []byte("  45.87.213.83\n\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readTrimmed(path)
	if err != nil {
		t.Fatalf("readTrimmed: %v", err)
	}
	if string(got) != "45.87.213.83" {
		t.Errorf("readTrimmed = %q, want %q", got, "45.87.213.83")
	}
}

// TestRunWarnsAfterRepeatedPolls forces the safety-poll path enough
// times to cross the warn threshold and asserts the warning lands in
// the log. The threshold semantics are sharper after content-dedup
// landed: suppressed (unchanged-content) inotify events do not reset
// the counter, so this test naturally exercises both a fully-broken
// inotify path (no events at all) and a healthy-but-misconfigured one
// (events arriving but all suppressed).
func TestRunWarnsAfterRepeatedPolls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ip")

	var buf threadSafeBuffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan struct{}, consecutivePollWarnThreshold+2)

	// A very short poll interval lets us cross the threshold quickly
	// without waiting real wall-clock time for production-sized
	// intervals.
	done := runAsync(t, ctx, path, 5*time.Millisecond, ticks, log)
	defer done.Wait()
	defer cancel()

	for i := 0; i < consecutivePollWarnThreshold+1; i++ {
		expectTick(t, ticks, "poll/startup tick")
	}

	if got := buf.String(); !strings.Contains(got, "safety poll has fired repeatedly") {
		t.Errorf("expected repeated-poll warning in log, got: %s", got)
	}
}

// TestRunDoesNotWarnBeforeThreshold confirms a moderate number of polls
// alone never crosses the warn boundary -- so a healthy daemon running
// for several intervals without IP changes stays quiet.
func TestRunDoesNotWarnBeforeThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ip")

	var buf threadSafeBuffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan struct{}, consecutivePollWarnThreshold*2)

	done := runAsync(t, ctx, path, 5*time.Millisecond, ticks, log)
	defer done.Wait()
	defer cancel()

	// One short of the threshold: startup + (threshold - 1) polls.
	for i := 0; i < consecutivePollWarnThreshold; i++ {
		expectTick(t, ticks, "poll/startup tick")
	}

	if got := buf.String(); strings.Contains(got, "safety poll has fired repeatedly") {
		t.Errorf("did not expect warning before threshold, got: %s", got)
	}
}

// runAsync starts trigger.Run on a goroutine and returns a WaitGroup
// the caller defers Wait() on, so test cleanup always observes a clean
// shutdown.
func runAsync(t *testing.T, ctx context.Context, path string, interval time.Duration, ticks chan struct{}, log *slog.Logger) *sync.WaitGroup {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := Run(ctx, path, interval, ticks, log); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	return &wg
}

// expectTick reads one tick from ticks with a timeout that's still
// well below "the test is hung" so failures surface as failures, not
// as the Go test runner's two-minute global timeout.
func expectTick(t *testing.T, ticks <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ticks:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for tick: %s", label)
	}
}

// threadSafeBuffer lets slog write to a buffer from one goroutine while
// the test reads from another without racing. bytes.Buffer is not safe
// for concurrent use.
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
