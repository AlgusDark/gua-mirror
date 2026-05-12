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

// TestRunEmitsOnFileWrite verifies the filesystem-watcher path fires a
// reconcile tick when the watched file is created. The startup tick is
// consumed first so the second tick unambiguously comes from the watcher.
//
// We use a relatively short safety interval as a backstop: even on
// platforms where the watcher behaves quirkily, the poll will deliver a
// tick within that window, so this test does not depend on any specific
// event-coalescing behavior across linux/inotify vs macos/kqueue.
func TestRunEmitsOnFileWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ip")

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan struct{}, 4)
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Order matters: cancel first so Run returns, then Wait collects it.
	done := runAsync(t, ctx, path, 200*time.Millisecond, ticks, log)
	defer done.Wait()
	defer cancel()

	expectTick(t, ticks, "startup")

	if err := os.WriteFile(path, []byte("2606:4700:4700::1111"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	expectTick(t, ticks, "post-write")
}

// TestRunWarnsAfterRepeatedPolls forces the safety-poll path enough times
// to cross the warn threshold and asserts the warning lands in the log.
// It documents the "inotify silently broken" failure mode that motivated
// the threshold in the first place.
func TestRunWarnsAfterRepeatedPolls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ip")

	var buf threadSafeBuffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan struct{}, consecutivePollWarnThreshold+2)

	// A very short poll interval lets us cross the threshold quickly without
	// waiting real wall-clock time for production-sized intervals.
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
// alone never crosses the warn boundary -- so a healthy daemon running for
// several intervals without IP changes stays quiet.
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

// runAsync starts trigger.Run on a goroutine and returns a WaitGroup the
// caller defers Wait() on, so test cleanup always observes a clean shutdown.
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

// expectTick reads one tick from ticks with a generous timeout that's still
// well below "the test is hung" so failures surface as failures, not as the
// Go test runner's two-minute global timeout.
func expectTick(t *testing.T, ticks <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ticks:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for tick: %s", label)
	}
}

// threadSafeBuffer lets slog write to a buffer from one goroutine while
// the test reads from another without racing. bytes.Buffer is not safe for
// concurrent use.
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
