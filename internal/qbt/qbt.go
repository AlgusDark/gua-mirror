// Package qbt is a tiny qBittorrent WebUI client that knows how to log in
// and trigger a reannounce-all. It is intentionally minimal: gua-mirror's
// only need is "tell qBittorrent to retry tracker announcements now."
package qbt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// Client wraps an authenticated qBittorrent WebUI session.
type Client struct {
	BaseURL  string
	Username string
	Password string
	Log      *slog.Logger

	http     *http.Client
	loggedIn bool
}

// New returns a Client. Username/password may be empty if "Bypass auth on
// localhost" is enabled in qBittorrent's WebUI settings.
func New(baseURL, user, pass string, log *slog.Logger) (*Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Username: user,
		Password: pass,
		Log:      log,
		http: &http.Client{
			Jar:     jar,
			Timeout: 10 * time.Second,
		},
	}, nil
}

// ReannounceAll tells qBittorrent to immediately reannounce every torrent.
// Logs in transparently if needed and retries once on auth failure.
func (c *Client) ReannounceAll(ctx context.Context) error {
	if err := c.ensureLoggedIn(ctx); err != nil {
		return err
	}
	body := url.Values{"hashes": {"all"}}
	err := c.post(ctx, "/api/v2/torrents/reannounce", body)
	if errors.Is(err, errAuth) {
		// Cookie may have expired; re-login and retry once.
		c.loggedIn = false
		if err := c.ensureLoggedIn(ctx); err != nil {
			return err
		}
		err = c.post(ctx, "/api/v2/torrents/reannounce", body)
	}
	if err != nil {
		return fmt.Errorf("reannounce: %w", err)
	}
	c.Log.Info("qbittorrent reannounce-all sent")
	return nil
}

func (c *Client) ensureLoggedIn(ctx context.Context) error {
	if c.loggedIn {
		return nil
	}
	if c.Username == "" && c.Password == "" {
		// "Bypass auth on localhost" is presumed; defer real verification to
		// the first API call, which will return errAuth if it isn't.
		c.loggedIn = true
		return nil
	}
	body := url.Values{
		"username": {c.Username},
		"password": {c.Password},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/v2/auth/login", strings.NewReader(body.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// qBittorrent rejects login if the Referer doesn't match the host.
	req.Header.Set("Referer", c.BaseURL)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(out)) != "Ok." {
		return fmt.Errorf("login failed: HTTP %d body=%q",
			resp.StatusCode, strings.TrimSpace(string(out)))
	}
	c.loggedIn = true
	return nil
}

// errAuth is returned when an API call comes back 403; ReannounceAll uses it
// to decide whether to re-login and retry.
var errAuth = errors.New("forbidden")

func (c *Client) post(ctx context.Context, path string, body url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path,
		strings.NewReader(body.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", c.BaseURL)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusForbidden:
		return errAuth
	default:
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("HTTP %d body=%q",
			resp.StatusCode, strings.TrimSpace(string(out)))
	}
}
