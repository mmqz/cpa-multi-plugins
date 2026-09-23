// adopt.go adopts the Xiaomi MiMo desktop app's SSO session as a cookie-lane
// credential: locate the Chromium Cookies store of the persist:xiaomi-account
// partition, decrypt the *.xiaomimimo.com rows, and persist one
// mimo-cookie-<uid>.json per discovered account. Read-only on the desktop's
// files (temp copy), credentials live only in the host auth store.
//
// Discovery covers both productName layouts seen in the wild ("Xiaomi MiMo
// AI" overseas / "Xiaomi MiMo" domestic) on the three platforms, plus any
// user-supplied extra paths (cookie_paths config).
package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	adoptMu      sync.Mutex
	lastAdoptRun time.Time
)

// startAdoption kicks the background adoption. Safe to call on every
// register/reconfigure — the guard collapses repeated calls.
func startAdoption() {
	go func() {
		adoptMu.Lock()
		if !lastAdoptRun.IsZero() && time.Since(lastAdoptRun) < time.Minute {
			adoptMu.Unlock()
			return
		}
		lastAdoptRun = time.Now()
		adoptMu.Unlock()
		adoptDesktopCookies()
	}()
}

// desktopCookieCandidates returns every plausible Cookies store path for the
// current user, most likely first.
func desktopCookieCandidates() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	products := []string{"Xiaomi MiMo AI", "Xiaomi MiMo"}
	var bases []string
	var goos = desktopOS()
	switch goos {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		for _, p := range products {
			bases = append(bases, filepath.Join(appData, p))
		}
	case "darwin":
		for _, p := range products {
			bases = append(bases, filepath.Join(home, "Library", "Application Support", p))
		}
	default: // linux and other unix
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		for _, p := range products {
			bases = append(bases, filepath.Join(cfg, p))
		}
	}
	var out []string
	for _, base := range bases {
		// Chromium 96+ / Electron 15+ moved the cookie store under a
		// Network subdir — the shipped desktop builds on Electron 41
		// (Chromium 146), so try that layout first, then the legacy
		// pre-96 location.
		out = append(out,
			filepath.Join(base, "Partitions", "xiaomi-account", "Network", "Cookies"),
			filepath.Join(base, "Partitions", "xiaomi-account", "Cookies"))
	}
	out = append(out, loadedCookiePaths()...)
	return out
}

func desktopOS() string {
	return runtime.GOOS
}

// adoptDesktopCookies scans candidates and persists one credential per new
// account. Existing cookie files are refreshed in place (re-adoption
// overwrites the stored jar so expiry tracking stays honest).
func adoptDesktopCookies() {
	candidates := desktopCookieCandidates()
	if len(candidates) == 0 {
		return
	}
	existing := map[string]string{} // uid -> auth file name
	if files, err := hostAuthList(); err == nil {
		for _, f := range files {
			sa, err := hostAuthGet(f.AuthIndex)
			if err != nil || sa == nil || authLaneFor(sa) != laneCookie {
				continue
			}
			if uid := sanitizeUIDForFileName(sa.Account.UID); uid != "" {
				existing[uid] = authFileNameFor(sa)
			}
		}
	}
	adopted := 0
	for _, dbPath := range candidates {
		if !fileExists(dbPath) {
			continue
		}
		cookies, _, err := readCookiesFromDB(dbPath)
		if err != nil {
			// Expected on macOS (keychain) or when the desktop never logged
			// in; stay quiet unless it's an unexpected shape.
			if !strings.Contains(err.Error(), "keychain") {
				log.Printf("adopt: %v", err)
			}
			continue
		}
		uid := userIdFromCookies(cookies)
		if uid == "" {
			uid = "adopted-" + sha256Sum(cookieFingerprint(cookies))
		}
		sa := &storedAuth{
			Auth: mimoTokens{
				Lane:      laneCookie,
				UID:       uid,
				Cookies:   cookies,
				Source:    "desktop-adopt",
				AdoptedAt: time.Now().Unix(),
			},
			Account: mimoAccount{UID: uid},
		}
		name, _ := resolveAuthFileTarget(sa, nil)
		if prev, ok := existing[uid]; ok && prev != "" {
			name = prev // keep the canonical name already registered with the host
		}
		raw, err := buildAuthFileJSON(sa, false, "桌面会话收养 · "+time.Unix(sa.Auth.AdoptedAt, 0).Format("2006-01-02 15:04"), nil)
		if err != nil {
			continue
		}
		if err := hostAuthPersistFn(name, raw); err != nil {
			log.Printf("adopt: persist %s failed: %v", name, err)
			continue
		}
		adopted++
	}
	if adopted > 0 {
		log.Printf("adopt: %d desktop cookie credential(s) persisted", adopted)
	}
}

// userIdFromCookies pulls the Chromium userId row if the desktop mirrored it
// into the partition store; empty otherwise (pseudo-uid path).
func userIdFromCookies(cookies []mimoCookie) string {
	for _, c := range cookies {
		if c.Name == "userId" && strings.TrimSpace(c.Value) != "" {
			return strings.TrimSpace(c.Value)
		}
	}
	return ""
}

// cookieFingerprint is a stable per-jar digest for pseudo-uid derivation —
// names/domains only, never values (no credential material in ids).
func cookieFingerprint(cookies []mimoCookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts = append(parts, c.Name+"@"+c.Domain)
	}
	return strings.Join(parts, ",")
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
