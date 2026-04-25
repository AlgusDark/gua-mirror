// Command gua-mirror keeps the public IPv6 GUA of a NAT66 VPN mirrored as a
// deprecated alias on the tunnel interface. See README for the why.
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/algus/gua-mirror/internal/alias"
	"github.com/algus/gua-mirror/internal/config"
	"github.com/algus/gua-mirror/internal/detect"
	"github.com/algus/gua-mirror/internal/qbt"
	"github.com/algus/gua-mirror/internal/trigger"
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
