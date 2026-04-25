package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("TUN_INTERFACE", "")
	t.Setenv("PUBLICIP_FILE", "")
	t.Setenv("IPV6_ECHO_ENDPOINTS", "")
	t.Setenv("SAFETY_POLL_INTERVAL", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("QBITTORRENT_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TunInterface != DefaultTunInterface {
		t.Errorf("TunInterface = %q, want %q", cfg.TunInterface, DefaultTunInterface)
	}
	if cfg.PublicIPFile != DefaultPublicIPFile {
		t.Errorf("PublicIPFile = %q, want %q", cfg.PublicIPFile, DefaultPublicIPFile)
	}
	if cfg.SafetyPollInterval != DefaultSafetyPollInterval {
		t.Errorf("SafetyPollInterval = %s, want %s", cfg.SafetyPollInterval, DefaultSafetyPollInterval)
	}
	if got := len(cfg.IPv6EchoEndpoints); got != 3 {
		t.Errorf("IPv6EchoEndpoints len = %d, want 3", got)
	}
	if cfg.QBittorrent != nil {
		t.Error("QBittorrent should be nil when QBITTORRENT_URL unset")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("TUN_INTERFACE", "wg0")
	t.Setenv("PUBLICIP_FILE", "/var/run/ip")
	t.Setenv("IPV6_ECHO_ENDPOINTS", "https://a.example, https://b.example ,, https://c.example")
	t.Setenv("SAFETY_POLL_INTERVAL", "30s")
	t.Setenv("QBITTORRENT_URL", "http://qbt:8080/")
	t.Setenv("QBITTORRENT_USERNAME", "user")
	t.Setenv("QBITTORRENT_PASSWORD", "pass")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TunInterface != "wg0" {
		t.Errorf("TunInterface = %q", cfg.TunInterface)
	}
	if cfg.SafetyPollInterval != 30*time.Second {
		t.Errorf("SafetyPollInterval = %s", cfg.SafetyPollInterval)
	}
	wantEndpoints := []string{"https://a.example", "https://b.example", "https://c.example"}
	if len(cfg.IPv6EchoEndpoints) != len(wantEndpoints) {
		t.Fatalf("IPv6EchoEndpoints = %v, want %v", cfg.IPv6EchoEndpoints, wantEndpoints)
	}
	for i, ep := range wantEndpoints {
		if cfg.IPv6EchoEndpoints[i] != ep {
			t.Errorf("IPv6EchoEndpoints[%d] = %q, want %q", i, cfg.IPv6EchoEndpoints[i], ep)
		}
	}
	if cfg.QBittorrent == nil {
		t.Fatal("QBittorrent nil")
	}
	if cfg.QBittorrent.URL != "http://qbt:8080" {
		t.Errorf("QBittorrent.URL = %q (trailing slash should be trimmed)", cfg.QBittorrent.URL)
	}
}

func TestLoadInvalidPollInterval(t *testing.T) {
	t.Setenv("SAFETY_POLL_INTERVAL", "not-a-duration")
	if _, err := Load(); err == nil {
		t.Error("expected error for invalid duration")
	}
}

func TestLoadNegativePollInterval(t *testing.T) {
	t.Setenv("SAFETY_POLL_INTERVAL", "-5s")
	if _, err := Load(); err == nil {
		t.Error("expected error for non-positive duration")
	}
}

func TestLoadEmptyEndpoints(t *testing.T) {
	t.Setenv("IPV6_ECHO_ENDPOINTS", " , , ")
	if _, err := Load(); err == nil {
		t.Error("expected error for empty endpoints")
	}
}
