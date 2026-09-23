// session.go owns the cookie lane's session state: the region table (the
// desktop's `op` map, index.beauty.mjs:1731-1733), the /user/xiaomi/me probe
// (the desktop's login/keepalive/renew endpoint — qc() parser parity,
// 3825-3858), region adoption, and the 401-renew ladder's plumbing.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// regionBases is the official region table (desktop bundle 1731-1733; cn is
// the domestic default and the only region in the community proxy).
var regionBases = map[string]string{
	"cn":  "https://mimo-server-cn.xiaomimimo.com/api",
	"sgp": "https://mimo-server-sgp.xiaomimimo.com/api",
	"ru":  "https://mimo-server-ru.xiaomimimo.com/api",
	"in":  "https://mimo-server-in.xiaomimimo.com/api",
}

// regionBaseFor resolves the cookie-lane base for one credential:
// credential-pinned region > config region > cn default.
func regionBaseFor(sa *storedAuth) string {
	if sa != nil {
		if r := strings.TrimSpace(sa.Auth.Region); r != "" {
			if base, ok := regionBases[strings.ToLower(r)]; ok {
				return base
			}
		}
	}
	mode := strings.ToLower(loadedRegionMode())
	if base, ok := regionBases[mode]; ok {
		return base
	}
	return regionBases[regionCN]
}

// buildCookieJarHeader renders the Cookie header for one upstream request:
// only cookies whose domain matches the upstream host (Chromium jar
// semantics — domain cookies match subdomains, host-only cookies match
// exactly) and whose path prefixes the request path. Expired rows drop.
func buildCookieJarHeader(cookies []mimoCookie, target string) string {
	host := requestHost(target)
	if host == "" {
		return ""
	}
	path := requestPath(target)
	now := time.Now().Unix()
	pairs := make([]string, 0, len(cookies))
	for _, c := range cookies {
		if c.Expires > 0 && c.Expires <= now {
			continue
		}
		if !cookieMatchesHost(c.Domain, host) {
			continue
		}
		if !cookieMatchesPath(c.Path, path) {
			continue
		}
		pairs = append(pairs, c.Name+"="+c.Value)
	}
	return strings.Join(pairs, "; ")
}

func requestHost(target string) string {
	// URL-embedded host; tolerate malformed targets by bailing empty.
	u := target
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	} else {
		return ""
	}
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	// Chromium jar matching is port-agnostic; drop :port before comparing.
	if i := strings.LastIndex(u, ":"); i >= 0 && !strings.Contains(u, "]") {
		u = u[:i]
	}
	return strings.ToLower(u)
}

func requestPath(target string) string {
	i := strings.Index(target, "://")
	if i < 0 {
		return "/"
	}
	rest := target[i+3:]
	if j := strings.Index(rest, "/"); j >= 0 {
		p := rest[j:]
		if k := strings.IndexAny(p, "?#"); k >= 0 {
			p = p[:k]
		}
		return p
	}
	return "/"
}

// cookieMatchesHost mirrors Chromium: a domain cookie (leading dot) matches
// the host and any subdomain; a host-only cookie matches exactly.
func cookieMatchesHost(domain, host string) bool {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return false
	}
	if strings.HasPrefix(domain, ".") {
		d := domain[1:]
		return host == d || strings.HasSuffix(host, "."+d)
	}
	return host == domain
}

func cookieMatchesPath(cookiePath, reqPath string) bool {
	cp := strings.TrimSpace(cookiePath)
	if cp == "" {
		cp = "/"
	}
	if !strings.HasPrefix(cp, "/") {
		cp = "/" + cp
	}
	if cp == "/" {
		return true
	}
	if !strings.HasPrefix(reqPath, cp) {
		return false
	}
	// RFC 6265 path-match: the char after the match must be "/" or nothing.
	if len(reqPath) == len(cp) {
		return true
	}
	return reqPath[len(cp)] == '/'
}

// -----------------------------------------------------------------------------
// me probe (renew / region adopt)
// -----------------------------------------------------------------------------

// meProbeResult is the qc()-shaped outcome of a /user/xiaomi/me probe.
type meProbeResult struct {
	LoggedIn   bool
	UserID     string
	Region     string
	Rejected   bool
	ServerCode any
}

// meURL returns {base}/user/xiaomi/me (the desktop's Ll() builder,
// index.beauty.mjs:3784-3785).
func meURL(base string) string {
	return strings.TrimRight(base, "/") + "/user/xiaomi/me"
}

// probeMe performs one cookie-lane me probe for the credential: 200 with
// code=0+userId means the session is alive; anything else means renew
// failed (the desktop's "renewLogin → expire ladder").
func probeMe(sa *storedAuth) (meProbeResult, error) {
	base := regionBaseFor(sa)
	target := meURL(base)
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return meProbeResult{}, err
	}
	applyCookieLaneHeaders(req, sa, "")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return meProbeResult{}, fmt.Errorf("me probe: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return parseMeResponse(resp.StatusCode, raw), nil
}

// parseMeResponse mirrors the desktop qc(): a numeric code inside the server
// rejection set marks the session rejected; code=0 with a data.userId marks
// it logged in and carries region/country.
func parseMeResponse(status int, body []byte) meProbeResult {
	var obj struct {
		Code any             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(body, &obj)
	if obj.Code != nil {
		if code, ok := obj.Code.(float64); ok && serverRejectedCode(code) {
			return meProbeResult{Rejected: true, ServerCode: code}
		}
	}
	if status != 200 {
		return meProbeResult{}
	}
	var data struct {
		UserID any    `json:"userId"`
		Region string `json:"region"`
	}
	if err := json.Unmarshal(obj.Data, &data); err != nil {
		return meProbeResult{}
	}
	userID := ""
	if s, ok := data.UserID.(string); ok {
		userID = s
	} else if f, ok := data.UserID.(float64); ok {
		userID = fmt.Sprintf("%d", int64(f))
	}
	if userID == "" {
		return meProbeResult{}
	}
	return meProbeResult{LoggedIn: true, UserID: userID, Region: strings.ToLower(strings.TrimSpace(data.Region))}
}

// serverRejectedCode mirrors the desktop's PE set (3815-3823): a small band
// of business codes the me endpoint uses to signal "this session is dead"
// (401 class). Anything else is inconclusive → fail-open like the desktop.
func serverRejectedCode(code float64) bool {
	switch int(code) {
	case 401, 403:
		return true
	}
	return false
}

// renewCookieSession is the 401-driven renew: one me probe; on success the
// credential's region may be adopted (auto mode only) and persisted. Returns
// whether the session renewed.
func renewCookieSession(sa *storedAuth) bool {
	res, err := probeMe(sa)
	if err != nil || !res.LoggedIn {
		return false
	}
	// Region adoption (desktop w()/nq() parity): only in auto mode, only to
	// a region in the official table, only persisted when it actually moves.
	if r := res.Region; r != "" && strings.ToLower(loadedRegionMode()) == "auto" {
		if _, ok := regionBases[r]; ok && r != strings.ToLower(strings.TrimSpace(sa.Auth.Region)) {
			sa.Auth.Region = r
			if raw, err := buildAuthFileJSON(sa, false, "", nil); err == nil {
				_ = hostAuthPersistFn(authFileNameFor(sa), raw)
			}
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// Header application
// -----------------------------------------------------------------------------

var cookieLaneHeaderMu sync.RWMutex

// renewCookieSessionFn is an indirection point so tests can drive the 401
// ladder without a live upstream session.
var renewCookieSessionFn = renewCookieSession

// applyCookieLaneHeaders applies the official desktop engine-lane fingerprint
// (docs/MIMO_AUTH.md §2): no Authorization, X-Mimo-Source: mimocode-cli-free,
// X-Client-Version, and the adopted jar as Cookie. uaPassThrough leaves any
// client-supplied UA untouched (the desktop never spoofs one on this lane).
func applyCookieLaneHeaders(req *http.Request, sa *storedAuth, _ string) {
	req.Header.Set("Content-Type", "application/json")
	// Authorization must never ride the cookie lane — the desktop deletes it
	// (index.beauty.mjs:1987); delete both spellings defensively.
	req.Header.Del("Authorization")
	req.Header.Set("X-Mimo-Source", sourceCookieLane)
	cookieLaneHeaderMu.RLock()
	ver := loadedXClientVersion()
	cookieLaneHeaderMu.RUnlock()
	if ver != "" {
		req.Header.Set("X-Client-Version", ver)
	}
	if sa != nil && len(sa.Auth.Cookies) > 0 {
		if cookie := buildCookieJarHeader(sa.Auth.Cookies, req.URL.String()); cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
	}
}

// applyKeyLaneHeaders applies the official CLI fingerprint
// (mimo.ts:200 + OpenAI-compat auth): Bearer sk + X-Mimo-Source: mimocode-cli.
func applyKeyLaneHeaders(req *http.Request, sa *storedAuth, _ string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sa.Auth.SK)
	req.Header.Set("X-Mimo-Source", sourceKeyLane)
}
