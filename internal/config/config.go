// Package config loads gua-mirror's runtime configuration from environment
// variables. All fields have safe defaults except QBittorrent, which is
// nil unless QBITTORRENT_URL is set.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Default values, exposed so callers (and tests) can reference them.
const (
	DefaultTunInterface       = "tun0"
	DefaultPublicIPFile       = "/gluetun/ip"
	DefaultIPv6EchoEndpoints  = "https://api6.ipify.org,https://ifconfig.co,https://icanhazip.com"
	DefaultSafetyPollInterval = 15 * time.Minute
	DefaultLogLevel           = "info"
)

// Config holds the parsed environment for one process lifetime.
type Config struct {
	TunInterface       string
	PublicIPFile       string
	IPv6EchoEndpoints  []string
	SafetyPollInterval time.Duration
	LogLevel           string

	// QBittorrent is nil if QBITTORRENT_URL is not set; reannounce is then a no-op.
	QBittorrent *QBittorrentConfig
}

// QBittorrentConfig holds optional qBittorrent WebUI credentials.
type QBittorrentConfig struct {
	URL      string
	Username string
	Password string
}

// Load reads the environment and returns a validated Config.
func Load() (*Config, error) {
	cfg := &Config{
		TunInterface:      getenv("TUN_INTERFACE", DefaultTunInterface),
		PublicIPFile:      getenv("PUBLICIP_FILE", DefaultPublicIPFile),
		IPv6EchoEndpoints: splitCSV(getenv("IPV6_ECHO_ENDPOINTS", DefaultIPv6EchoEndpoints)),
		LogLevel:          getenv("LOG_LEVEL", DefaultLogLevel),
	}

	pollStr := getenv("SAFETY_POLL_INTERVAL", DefaultSafetyPollInterval.String())
	poll, err := time.ParseDuration(pollStr)
	if err != nil {
		return nil, fmt.Errorf("invalid SAFETY_POLL_INTERVAL %q: %w", pollStr, err)
	}
	if poll <= 0 {
		return nil, fmt.Errorf("SAFETY_POLL_INTERVAL must be positive, got %s", poll)
	}
	cfg.SafetyPollInterval = poll

	if url := os.Getenv("QBITTORRENT_URL"); url != "" {
		cfg.QBittorrent = &QBittorrentConfig{
			URL:      strings.TrimRight(url, "/"),
			Username: os.Getenv("QBITTORRENT_USERNAME"),
			Password: os.Getenv("QBITTORRENT_PASSWORD"),
		}
	}

	if len(cfg.IPv6EchoEndpoints) == 0 {
		return nil, errors.New("IPV6_ECHO_ENDPOINTS must contain at least one URL")
	}
	if cfg.TunInterface == "" {
		return nil, errors.New("TUN_INTERFACE must not be empty")
	}
	if cfg.PublicIPFile == "" {
		return nil, errors.New("PUBLICIP_FILE must not be empty")
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
