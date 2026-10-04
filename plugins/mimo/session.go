// session.go owns the cookie lane's session state: the region table (the
// desktop's `op` map, index.beauty.mjs:1731-1733), the M2 service-ticket
// renewal (the serviceLogin→STS exchange, exchange.go), and the request
// ladder's plumbing. The former /user/xiaomi/me probe is RETIRED: measured
// on the real machine (2026-09-23), me 302s to serviceLogin even for
// requests the chat endpoint accepts, so it cannot judge session health
// (docs/MIMO_AUTH.md §6.2) — renewal is now a fresh mint.
package main

import (
	"log"
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

// renewCookieSession is the ladder's renew: one fresh serviceLogin→STS mint
// from the jar's bootstrap rows (exchange.go — the desktop re-mints per run;
// we do the same on demand). On success the minted rows and the region/sid
// they were bound to are persisted so subsequent requests start warm.
// Returns whether the session renewed.
func renewCookieSession(sa *storedAuth) bool {
	if !exchangeForCredential(sa) {
		return false
	}
	if raw, err := buildAuthFileJSON(sa, false, "", nil); err == nil {
		if perr := hostAuthPersistFn(authFileNameFor(sa), raw); perr != nil {
			// The in-memory mutation still serves the in-flight retry;
			// persistence failure just means the next request re-mints.
			log.Printf("renew: persist failed: %v", perr)
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
		if cookie := renderCookieHeader(sa, req.URL.String()); cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
	}
}

// renderCookieHeader picks the right Cookie shape for one upstream request:
//
//   - jar holds a minted serviceToken → the M2 assembled header
//     (buildCookieHeader order userId→serviceToken→cUserId→<sid>_ph,
//     measured on the real machine; a bare serviceToken cookie 302s).
//   - jar holds only legacy *.xiaomimimo.com rows (old desktop builds that
//     did persist service rows) → the M1 whole-jar render (desktop jar
//     semantics, buildCookieJarHeader).
//   - bootstrap rows only (no mint yet) → nothing: the request will bounce,
//     and the ladder's re-mint is the correct response. Sending account
//     rows upstream is the one shape that must never happen (§3.3).
func renderCookieHeader(sa *storedAuth, target string) string {
	cookies := sa.Auth.Cookies
	if cookieValue(cookies, "serviceToken") != "" {
		return buildCookieHeader(cookies, regionSID(sa.Auth.Region))
	}
	for _, c := range cookies {
		if isMimoUpstreamHost(c.Domain) {
			return buildCookieJarHeader(cookies, target)
		}
	}
	return ""
}

// applyKeyLaneHeaders applies the official CLI fingerprint
// (mimo.ts:200 + OpenAI-compat auth): Bearer sk + X-Mimo-Source: mimocode-cli.
func applyKeyLaneHeaders(req *http.Request, sa *storedAuth, _ string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sa.Auth.SK)
	req.Header.Set("X-Mimo-Source", sourceKeyLane)
}
