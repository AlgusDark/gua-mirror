// Command gua-mirror keeps the public IPv6 GUA of a NAT66 VPN mirrored as a
// deprecated alias on the tunnel interface. See README for the why.
package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/AlgusDark/gua-mirror/internal/alias"
	"github.com/AlgusDark/gua-mirror/internal/config"
	"github.com/AlgusDark/gua-mirror/internal/detect"
	"github.com/AlgusDark/gua-mirror/internal/qbt"
	"github.com/AlgusDark/gua-mirror/internal/trigger"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	}))
	slog.SetDefault(log)

	log.Info("gua-mirror starting",
		"version", version,
		"interface", cfg.TunInterface,
		"publicip_file", cfg.PublicIPFile,
		"v6_endpoints", cfg.IPv6EchoEndpoints,
		"safety_poll", cfg.SafetyPollInterval,
		"qbittorrent_enabled", cfg.QBittorrent != nil,
	)

	warnIfPublicIPPathMissing(cfg.PublicIPFile, log)

	detector := detect.New(cfg.IPv6EchoEndpoints, log)
	aliasMgr := alias.NewManager(cfg.TunInterface, log)

	var qbtClient *qbt.Client
	if cfg.QBittorrent != nil {
		qbtClient, err = qbt.New(
			cfg.QBittorrent.URL,
			cfg.QBittorrent.Username,
			cfg.QBittorrent.Password,
			log,
		)
		if err != nil {
			return err
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	triggers := make(chan struct{}, 1)
	go func() {
		if err := trigger.Run(ctx, cfg.PublicIPFile, cfg.SafetyPollInterval, triggers, log); err != nil {
			log.Error("trigger loop exited", "err", err)
			cancel()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return nil
		case <-triggers:
			reconcile(ctx, detector, aliasMgr, qbtClient, log)
		}
	}
}

// reconcile detects the current public v6 and updates the alias and (if
// configured) qBittorrent. Errors are logged and swallowed; the next tick
// will retry. We never want to crash the daemon over a transient HTTP blip.
func reconcile(ctx context.Context, det *detect.Detector, mgr *alias.Manager, q *qbt.Client, log *slog.Logger) {
	detectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ip, err := det.Detect(detectCtx)
	if err != nil {
		log.Warn("v6 detection failed; keeping current alias",
			"current", ipString(mgr.Current()), "err", err)
		return
	}
	if cur := mgr.Current(); cur != nil && cur.Equal(ip) {
		log.Debug("v6 unchanged", "address", ip.String())
		return
	}
	log.Info("v6 changed", "from", ipString(mgr.Current()), "to", ip.String())

	if err := mgr.Set(ip); err != nil {
		log.Error("alias set failed", "err", err)
		return
	}
	if q == nil {
		return
	}
	qctx, qcancel := context.WithTimeout(ctx, 15*time.Second)
	defer qcancel()
	if err := q.ReannounceAll(qctx); err != nil {
		log.Warn("qbittorrent reannounce failed", "err", err)
	}
}

func ipString(ip net.IP) string {
	if ip == nil {
		return ""
	}
	return ip.String()
}

// warnIfPublicIPPathMissing reports the common misconfiguration where
// PUBLICIP_FILE points at a path no writer ever produces.
//
// Two distinct states are checked:
//
//   - Parent directory missing. The filesystem watcher will fail to attach
//     and the daemon will exit shortly after this warning -- but the
//     warning makes it obvious *why*. The fix is to mount the VPN
//     container's shared volume into this container at the right path.
//
//   - Parent present but the file itself not yet written. This is the
//     legitimate first-boot state; the watcher attaches to the directory
//     and waits for the first write. Logged at info as reassurance, not a
//     problem.
//
// We don't try to recover from the first case (poll-only mode is a strictly
// worse degraded state that no one asked for); we just make the failure
// mode loud enough that an operator finds the misconfig within seconds.
func warnIfPublicIPPathMissing(path string, log *slog.Logger) {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Warn("PUBLICIP_FILE parent directory does not exist; "+
				"mount the VPN container's shared volume here or fix PUBLICIP_FILE. "+
				"The filesystem watcher will fail to attach and the daemon will exit.",
				"path", path, "dir", dir)
		} else {
			log.Warn("PUBLICIP_FILE parent directory stat failed",
				"path", path, "dir", dir, "err", err)
		}
		return
	}
	if _, err := os.Stat(path); err != nil && errors.Is(err, fs.ErrNotExist) {
		log.Info("PUBLICIP_FILE not present yet; "+
			"waiting for first write from the VPN container",
			"path", path)
	}
}

func parseLogLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
