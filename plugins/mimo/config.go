// config.go decodes plugin config from the lifecycle request. All
// plugin-level config lives here so the rest of the plugin reads consistent,
// lock-protected snapshots.
package main

import (
	"encoding/json"
	"strings"
	"sync"
)

var (
	// regionMode: auto (default) adopts the desktop session's region when the
	// cookie lane's me-probe reports one, else cn. Explicit values pin it.
	regionMode   = "auto"
	regionModeMu sync.RWMutex

	// xClientVersion is the cookie lane's X-Client-Version header (desktop
	// parity; hl() index.beauty.mjs:4797).
	xClientVersion   = desktopAppVersion
	xClientVersionMu sync.RWMutex

	// platformURL is the OAuth base for NEW sk-lane logins.
	platformURL   = platformBase
	platformURLMu sync.RWMutex

	// cookiePaths are extra Chromium Cookies file paths for adoption.
	cookiePaths   []string
	cookiePathsMu sync.RWMutex

	// exchangeUserAgent overrides the serviceLogin/STS exchange UA. Default ""
	// keeps Go's stdlib UA '' no fabricated identity (the project rejected
	// impersonating mimocode's UA for the same reason); the measured working
	// value on the real machine was "MiClaw/1.0" '' set it here if a passport
	// edge ever starts gating user agents.
	exchangeUserAgent   string
	exchangeUserAgentMu sync.RWMutex
)

func loadedRegionMode() string {
	regionModeMu.RLock()
	defer regionModeMu.RUnlock()
	return regionMode
}

func loadedXClientVersion() string {
	xClientVersionMu.RLock()
	defer xClientVersionMu.RUnlock()
	return xClientVersion
}

func loadedPlatformURL() string {
	platformURLMu.RLock()
	defer platformURLMu.RUnlock()
	if v := strings.TrimRight(platformURL, "/"); v != "" {
		return v
	}
	return platformBase
}

func loadedExchangeUserAgent() string {
	exchangeUserAgentMu.RLock()
	defer exchangeUserAgentMu.RUnlock()
	return exchangeUserAgent
}

func loadedCookiePaths() []string {
	cookiePathsMu.RLock()
	defer cookiePathsMu.RUnlock()
	out := make([]string, len(cookiePaths))
	copy(out, cookiePaths)
	return out
}

// configure decodes plugin config from the lifecycle request. The request is
// the same envelope as zcode's: {"config": <plugin config object>} — the
// host forwards plugins.configs.<pluginID> on register/reconfigure. Tolerate
// both the wrapped and the bare-object form.
func configure(raw []byte) {
	var probe struct {
		Config json.RawMessage `json:"config"`
	}
	cfgRaw := raw
	if err := json.Unmarshal(raw, &probe); err == nil && len(probe.Config) > 0 {
		cfgRaw = probe.Config
	}
	var cfg struct {
		Region            string `json:"region"`
		XClientVersion    string `json:"x_client_version"`
		PlatformURL       string `json:"platform_url"`
		CookiePaths       string `json:"cookie_paths"`
		ExchangeUserAgent string `json:"exchange_user_agent"`
	}
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		return
	}
	if v := strings.TrimSpace(cfg.Region); v != "" {
		switch strings.ToLower(v) {
		case "auto", "cn", "sgp", "ru", "in":
			regionModeMu.Lock()
			regionMode = strings.ToLower(v)
			regionModeMu.Unlock()
		}
	}
	if v := strings.TrimSpace(cfg.XClientVersion); v != "" {
		xClientVersionMu.Lock()
		xClientVersion = v
		xClientVersionMu.Unlock()
	}
	if v := strings.TrimSpace(cfg.PlatformURL); v != "" {
		platformURLMu.Lock()
		platformURL = v
		platformURLMu.Unlock()
	}
	if v := strings.TrimSpace(cfg.ExchangeUserAgent); v != "" {
		exchangeUserAgentMu.Lock()
		exchangeUserAgent = v
		exchangeUserAgentMu.Unlock()
	}
	if v := strings.TrimSpace(cfg.CookiePaths); v != "" {
		var paths []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, p)
			}
		}
		cookiePathsMu.Lock()
		cookiePaths = paths
		cookiePathsMu.Unlock()
	}
}
