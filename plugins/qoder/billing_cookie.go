package main

// billing_cookie.go — the billing surface's web-session handshake.
//
// Adapted from bfSan/qoder-cpa-plugin f05e9e3 (live-verified 2026-09-21 on
// openapi.qoder.com.cn): Qoder's billing endpoints (/sash/api/v1/*,
// /api/v2/*) began refusing a bare Bearer token with 401
// {"code":"UNAUTHORIZED","message":"missing cookie header"}. The desktop
// client never hits this because it runs inside an Electron session whose
// cookie jar attaches acw_tc / qoder_csrf_token automatically and mirrors
// the CSRF cookie into an X-CSRF-Token header.
//
// The plugin has no browser session, so it replays the same handshake:
// one bootstrap GET per account (any endpoint works; /api/v1/me is the
// cheapest) makes upstream emit its session cookies; afterwards every
// billing call carries Cookie + X-CSRF-Token alongside the existing
// Authorization/Cosy headers. Where upstream does not gate on cookies the
// extra headers are inert (issue #27 cross-check: campaigns/claim answered
// identically with real, random and placeholder identity values), so this
// is pure line-alignment with the official client's traffic.
//
// Jars are keyed per credential (region|uid) and never shared — the same
// isolation the per-account machine identity requires; two auth files for
// the same account deliberately share one jar (same upstream session).

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
)

const csrfCookieName = "qoder_csrf_token"

var (
	billingJarMu sync.Mutex
	billingJars  sync.Map // accountKey -> *billingJar
)

type billingJar struct {
	mu           sync.Mutex
	jar          *cookiejar.Jar
	bootstrapped bool
}

// absorb folds Set-Cookie headers from any billing response into the jar so
// rotated cookies (acw_tc rotates server-side) stay current without a second
// bootstrap.
func (b *billingJar) absorb(baseURL string, headers http.Header) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b == nil || b.jar == nil || headers == nil {
		return
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return
	}
	cookies := readSetCookieHeaders(headers)
	if len(cookies) > 0 {
		b.jar.SetCookies(u, cookies)
	}
}

// billingJarFor returns the per-account jar, creating it lazily.
func billingJarFor(sa *storedAuth) *billingJar {
	key := billingAccountKey(sa)
	if v, ok := billingJars.Load(key); ok {
		return v.(*billingJar)
	}
	billingJarMu.Lock()
	defer billingJarMu.Unlock()
	if v, ok := billingJars.Load(key); ok {
		return v.(*billingJar)
	}
	var jar *cookiejar.Jar
	if created, err := cookiejar.New(nil); err == nil {
		jar = created
	}
	b := &billingJar{jar: jar}
	billingJars.Store(key, b)
	return b
}

// billingAccountKey isolates jars per credential. Falls back to the token
// prefix so two auth files for the same account still share one jar.
func billingAccountKey(sa *storedAuth) string {
	if sa == nil {
		return "anonymous"
	}
	if uid := strings.TrimSpace(sa.Account.UID); uid != "" {
		return authRegion(sa) + "|" + uid
	}
	token := strings.TrimSpace(sa.Auth.AccessToken)
	if len(token) > 8 {
		return authRegion(sa) + "|" + token[:8]
	}
	return authRegion(sa) + "|" + token
}

// readSetCookieHeaders parses Set-Cookie response headers into cookies the
// jar can store. net/http exposes the reverse (Response.Cookies) but not a
// header-only helper, and the host bridge hands us raw headers rather than a
// Response object.
func readSetCookieHeaders(headers http.Header) []*http.Cookie {
	raw, ok := headers["Set-Cookie"]
	if !ok {
		raw = headers["Set-cookie"]
	}
	out := make([]*http.Cookie, 0, len(raw))
	for _, line := range raw {
		if c := parseOneCookie(line); c != nil {
			out = append(out, c)
		}
	}
	return out
}

func parseOneCookie(line string) *http.Cookie {
	parts := strings.Split(line, ";")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return nil
	}
	nameValue := strings.SplitN(strings.TrimSpace(parts[0]), "=", 2)
	if len(nameValue) != 2 || nameValue[0] == "" {
		return nil
	}
	c := &http.Cookie{Name: nameValue[0], Value: nameValue[1], Raw: line}
	for _, attr := range parts[1:] {
		attr = strings.TrimSpace(attr)
		if attr == "" {
			continue
		}
		keyValue := strings.SplitN(attr, "=", 2)
		key := strings.ToLower(strings.TrimSpace(keyValue[0]))
		value := ""
		if len(keyValue) == 2 {
			value = keyValue[1]
		}
		switch key {
		case "path":
			c.Path = value
		case "domain":
			c.Domain = value
		case "secure":
			c.Secure = true
		case "httponly":
			c.HttpOnly = true
		}
	}
	return c
}

// resetBillingJars drops cached jars (test seam).
func resetBillingJars() {
	billingJars.Range(func(key, _ any) bool {
		billingJars.Delete(key)
		return true
	})
}

// bootstrapBillingSession performs the one-shot call that makes upstream emit
// its session cookies. The response status is irrelevant — only Set-Cookie is
// consumed — but the bootstrapped latch is deliberately set BEFORE the call so
// a broken network cannot add a failing dial to every later billing call. If
// the race leaves the jar empty, response absorption on the regular billing
// fetches self-heals it.
func bootstrapBillingSession(sa *storedAuth) {
	b := billingJarFor(sa)
	b.mu.Lock()
	if b.bootstrapped || b.jar == nil {
		b.mu.Unlock()
		return
	}
	b.bootstrapped = true
	b.mu.Unlock()

	// Route through billingBaseFor so tests can intercept the handshake via
	// the same override every billing call uses.
	base := billingBaseFor(sa)
	req, err := http.NewRequest(http.MethodGet, base+"/api/v1/me", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Qoder")
	req.Header.Set("Cosy-ClientType", billingClientType)
	req.Header.Set("Cosy-Version", billingClientVer)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return
	}
	b.absorb(base, resp.Headers)
}

// cookiesFor builds the Cookie header value and the CSRF token for a base URL.
func (b *billingJar) cookiesFor(baseURL string) (string, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b == nil || b.jar == nil {
		return "", ""
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", ""
	}
	var parts []string
	csrf := ""
	for _, c := range b.jar.Cookies(u) {
		parts = append(parts, c.Name+"="+c.Value)
		if c.Name == csrfCookieName {
			csrf = c.Value
		}
	}
	return strings.Join(parts, "; "), csrf
}

// applyBillingSessionHeaders attaches Cookie + X-CSRF-Token to a billing
// request, bootstrapping the session on first use. Absent cookies are simply
// omitted — a request without them is exactly what the plugin sent before
// this handshake existed.
func applyBillingSessionHeaders(req *http.Request, sa *storedAuth) {
	if req == nil || sa == nil {
		return
	}
	bootstrapBillingSession(sa)
	base := billingBaseFor(sa)
	cookie, csrf := billingJarFor(sa).cookiesFor(base)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
}

// absorbBillingResponse folds a billing response's Set-Cookie headers into
// the account jar. One-line call at the main fetch sites keeps the jar fresh
// without extra round trips.
func absorbBillingResponse(sa *storedAuth, base string, resp *hostHTTPResponse) {
	if sa == nil || resp == nil || len(resp.Headers) == 0 {
		return
	}
	billingJarFor(sa).absorb(base, resp.Headers)
}
