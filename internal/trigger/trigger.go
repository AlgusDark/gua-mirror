// Package trigger emits reconciliation ticks driven by both filesystem
// events and a periodic safety poll.
//
// We watch the parent directory of the public-IP file rather than the file
// itself because gluetun (and most well-behaved writers) replace the file
// atomically via rename, which changes the inode and silently breaks any
// watcher attached directly to it.
package trigger

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// consecutivePollWarnThreshold is the number of back-to-back safety polls
// (with zero intervening inotify events) we tolerate before warning. In a
// healthy stack, the IP file is updated each time the VPN reconnects --
// roughly daily at most -- so several days of polls in a row is suspicious.
// With the default 1h poll interval, this is ~24 hours of silence.
const consecutivePollWarnThreshold = 24

// Run blocks until ctx is cancelled, sending a tick on out:
//   - immediately on start (so callers do an initial reconciliation),
//   - whenever the file at path is created/written/renamed,
//   - every safetyInterval if no inotify events have arrived in that window.
//
// Ticks are coalesced: if a receiver is busy, additional ticks are dropped
// rather than queued. The next tick will catch them up to current state.
//
// The safety poll is a fallback for cases where inotify is unreliable
// (silent watcher death, unusual filesystems, or the VPN updating v6 state
// without rewriting the IPv4 publicip file). If poll ticks fire without any
// intervening inotify events, we surface that at WARN so a misconfigured
// stack does not silently rely on the slow path forever.
func Run(ctx context.Context, path string, safetyInterval time.Duration, out chan<- struct{}, log *slog.Logger) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if err := watcher.Add(dir); err != nil {
		return err
	}

	timer := time.NewTimer(safetyInterval)
	defer timer.Stop()

	// Track consecutive safety-poll triggers since the last inotify event.
	// A non-zero count when the daemon is healthy means inotify is not
	// observing writes -- usually a missing volume mount or a wrong path.
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
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) != 0 {
				consecutivePolls = 0
				emit("inotify:" + ev.Op.String())
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Warn("inotify error", "err", err)
		case <-timer.C:
			consecutivePolls++
			if consecutivePolls == consecutivePollWarnThreshold {
				log.Warn("safety poll has fired repeatedly with no inotify events; "+
					"check that PUBLICIP_FILE is shared from the VPN container",
					"path", path, "consecutive", consecutivePolls,
					"interval", safetyInterval)
			}
			emit("safety-poll")
		}
	}
}
