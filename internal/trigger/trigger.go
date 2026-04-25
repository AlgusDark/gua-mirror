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

// Run blocks until ctx is cancelled, sending a tick on out:
//   - immediately on start (so callers do an initial reconciliation),
//   - whenever the file at path is created/written/renamed,
//   - every safetyInterval if no inotify events have arrived in that window.
//
// Ticks are coalesced: if a receiver is busy, additional ticks are dropped
// rather than queued. The next tick will catch them up to current state.
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
				emit("inotify:" + ev.Op.String())
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Warn("inotify error", "err", err)
		case <-timer.C:
			emit("safety-poll")
		}
	}
}
