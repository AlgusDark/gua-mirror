// Package trigger emits reconciliation ticks driven by file-content changes
// and a periodic safety poll.
//
// Why we watch the *parent directory*, not the file: gluetun (and most
// well-behaved writers) replace the file atomically via rename, which
// changes the inode and silently breaks any watcher attached directly to
// the file.
//
// Why we dedup by content: gluetun rewrites PUBLICIP_FILE on every
// publicip refresh, including refreshes where the contents did not change
// (same VPN exit re-fetched). Treating every filesystem event as a
// reconcile trigger would pay an HTTPS round-trip to a third-party echo
// for changes that cannot move the v6 GUA. We read the file on each
// filesystem event and suppress events that did not change the trimmed
// content. The safety poll remains unconditional: it exists for cases
// where the v6 GUA changes without a corresponding v4 file change (NAT66
// pool reassignment on the VPN exit), and for cases where the watcher
// itself has gone silent.
package trigger

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// consecutivePollWarnThreshold is the number of back-to-back safety polls
// (with zero intervening content-change inotify events) we tolerate
// before warning. In a healthy stack, the IP file is updated each time
// the VPN reconnects -- roughly daily at most -- so several days of
// polls in a row is suspicious. With the default 1h poll interval,
// this is ~24 hours of silence.
//
// Combined with content-dedup, this counter has a sharper meaning than
// the file-event-only counter would: a string of inotify events that
// all carry unchanged content does NOT reset the counter, because
// suppressed events are operationally equivalent to "watcher told us
// nothing useful." A misconfigured stack where inotify watches the
// wrong directory and a healthy stack where gluetun rewrites the file
// with unchanged content for days are both caught.
const consecutivePollWarnThreshold = 24

// maxPublicIPFileBytes is the most we will read from PUBLICIP_FILE on any
// single inotify event. Real writers (gluetun) emit a single IP address
// per line, well under 64 bytes; 1 KiB is generous and bounds resource
// use if the path is ever replaced with something unexpectedly large
// (compromised writer, mis-pointed path, symlink target rotation). The
// daemon still trusts whoever can write to the path -- this is
// defense-in-depth, not an isolation boundary.
const maxPublicIPFileBytes = 1 << 10

// Run blocks until ctx is cancelled, sending a tick on out:
//   - immediately on start (so callers do an initial reconciliation),
//   - whenever the trimmed content of the file at path changes,
//   - every safetyInterval if no content events have arrived in that window.
//
// Ticks are coalesced: if a receiver is busy, additional ticks are dropped
// rather than queued. The next tick will catch them up to current state.
//
// The safety poll is a fallback for cases where inotify is unreliable
// (silent watcher death, unusual filesystems, or the VPN updating v6
// state without rewriting the IPv4 publicip file). If poll ticks fire
// without any intervening content-change events, we surface that at
// WARN so a misconfigured stack does not silently rely on the slow
// path forever.
func Run(ctx context.Context, path string, safetyInterval time.Duration, out chan<- struct{}, log *slog.Logger) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if err := watcher.Add(dir); err != nil {
		return err
	}

	timer := time.NewTimer(safetyInterval)
	defer timer.Stop()

	// lastSeen is the trimmed content from the most recent file read, or
	// nil before any successful read.
	//
	// Note: a nil lastSeen and a successfully-read trimmed-empty file
	// are indistinguishable to bytes.Equal. In practice gluetun (the
	// only documented writer) always writes a single IP address line,
	// so the trimmed content cannot legitimately be empty; the
	// degenerate case ("writer suddenly produces an empty file") would
	// be picked up on the next safety-poll tick. If a future
	// integration needs to distinguish those states, replace the
	// sentinel with an explicit haveLastSeen bool.
	var lastSeen []byte
	// Seed lastSeen if the file already exists at startup. Otherwise the
	// first inotify event will populate it. Read errors are tolerated:
	// fail-safely means "reconcile when in doubt", and the worst case is
	// one extra HTTPS request.
	if seed, err := readTrimmed(path); err == nil {
		lastSeen = seed
		log.Debug("seeded last-seen content", "bytes", len(seed))
	}

	// Track consecutive safety-poll triggers since the last
	// content-change event. A non-zero count when the daemon is healthy
	// means inotify is not observing useful events -- usually a missing
	// volume mount or a wrong path.
	consecutivePolls := 0

	emit := func(reason string) {
		log.Debug("trigger fired", "reason", reason)
		select {
		case out <- struct{}{}:
		default:
			// Receiver is still working on a previous tick; coalesce.
		}
		// Reset the safety-poll countdown so it only fires when truly idle.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(safetyInterval)
	}

	emit("startup")

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 {
				continue
			}
			// Read the file and dedup by trimmed content. A read error
			// (file disappeared between events, transient I/O, mid-rename
			// race) is treated as "content might have changed" and we
			// emit -- the reconciler can decide what to do with no signal.
			content, err := readTrimmed(path)
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					log.Debug("file read failed; emitting anyway",
						"path", path, "err", err)
				}
				consecutivePolls = 0
				emit("inotify:" + ev.Op.String() + " (unreadable)")
				continue
			}
			if bytes.Equal(content, lastSeen) {
				log.Debug("file event suppressed; content unchanged",
					"op", ev.Op.String(), "bytes", len(content))
				continue
			}
			lastSeen = content
			consecutivePolls = 0
			emit("inotify:" + ev.Op.String())
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Warn("inotify error", "err", err)
		case <-timer.C:
			consecutivePolls++
			if consecutivePolls == consecutivePollWarnThreshold {
				log.Warn("safety poll has fired repeatedly with no content events; "+
					"check that PUBLICIP_FILE is shared from the VPN container",
					"path", path, "consecutive", consecutivePolls,
					"interval", safetyInterval)
			}
			emit("safety-poll")
		}
	}
}

// readTrimmed reads the file and returns its content with leading and
// trailing whitespace removed. Trimming guards against spurious reconciles
// when a writer changes only trailing newlines or surrounding whitespace.
// Reads are capped at maxPublicIPFileBytes; see that constant for why.
func readTrimmed(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxPublicIPFileBytes))
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(b), nil
}
