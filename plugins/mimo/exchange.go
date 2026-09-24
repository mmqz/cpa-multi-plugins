// exchange.go — M2 serviceToken minting: the two-phase
// serviceLogin → STS exchange that turns the partition jar's plaintext
// account-domain cookies into a live upstream ticket.
//
// Wire shape is dual-sourced (docs/MIMO_AUTH.md §3.3 static + §6.2 measured):
//
//	P1  GET https://account.xiaomi.com/pass/serviceLogin
//	      ?_locale=zh_CN&_snsNone=true&sid=<sid>&_json=true
//	    Cookie: passToken; userId; cUserId [; uLocale]
//	    → 200 text/plain "&&&START&&&{...}&&&END&&&" where the JSON carries
//	      code=0, ssecurity, nonce, location. NOTE: nonce rides the wire as a
//	      BARE JSON number on the measured machine — see nonceT below.
//
//	P2  GET <location>&clientSign=<urlencode(base64(sha1("nonce="+nonce+"&"+ssecurity)))>
//	    (deliberately cookieless)
//	    → Set-Cookie: serviceToken, userId, <sid>_ph, <sid>_slh.
//
// Upstream auth facts measured on a logged-in real machine (2026-09-23,
// desktop 26.922.222056) that static analysis alone could not show:
//
//  1. The chat endpoint does NOT accept a bare `Cookie: serviceToken=` —
//     requests must carry the bundle's buildCookieHeader order
//     userId → serviceToken → cUserId → <sid>_ph (see buildCookieHeader).
//  2. Every upstream request needs `X-Mimo-Source: mimocode-cli-free` and no
//     Authorization header (the desktop's lq() injects/deletes both —
//     applyCookieLaneHeaders already did this).
//  3. /user/xiaomi/me is unusable as a health probe on this lane: it 302s to
//     serviceLogin even for traffic the chat endpoint accepts. Session health
//     is therefore judged by the chat call itself (401/302/html → re-exchange
//     → retry once).
//  4. P1's nonce is a BARE JSON number on the measured wire (2026-09-24 real
//     machine, e.g. 4341996316119746560 — 19 digits, past float64's 53-bit
//     mantissa). A plain string field cannot unmarshal it at all; an
//     any/float64 field would silently corrupt it. nonceT keeps the literal
//     digits verbatim — clientSign hashes exactly those.
//
// The ticket is never written to disk by the desktop (it re-mints per run);
// we persist ours in the host auth store next to the bootstrap rows and
// re-exchange on failure — the same lifecycle, one cache level deeper.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// serviceLoginBase is the passport host for P1. A var so tests can point the
// whole exchange at an httptest server.
var serviceLoginBase = "https://account.xiaomi.com"

// regionSID maps a region table key to the passport service id the exchange
// must bind. sgp/mimopc and cn/mimopc are measured (real machine 2026-09-23:
// P2 mint via sid=mimosgp, and the me-302 callback showed sid=mimopc for the
// cn cluster); ru/in sids were never observed — minting there is rejected
// rather than guessed.
func regionSID(region string) string {
	switch strings.ToLower(strings.TrimSpace(region)) {
	case "sgp":
		return "mimosgp"
	case "cn":
		return "mimopc"
	default:
		return ""
	}
}

// regionCandidates lists the exchange's region attempts in order. Pinned
// regions try themselves only; auto tries sgp first (the measured working
// cluster) then cn, skipping a region whose sid is already stored on the
// credential — a sid that just failed a chat call is not worth re-minting.
func regionCandidates(mode, currentRegion string) []string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != "" && mode != "auto" {
		if _, ok := regionBases[mode]; ok {
			return []string{mode}
		}
	}
	current := strings.ToLower(strings.TrimSpace(currentRegion))
	candidates := []string{"sgp", "cn"}
	out := make([]string, 0, len(candidates))
	for _, r := range candidates {
		if r == current {
			continue
		}
		out = append(out, r)
	}
	return out
}

// clientSign computes the P2 proof: urlencode(base64(sha1("nonce="+nonce+"&"+ssecurity))).
// Measured on the real machine; also the shape the desktop's /sts callback
// expects (index.beauty.mjs /api/sts?sign=…, docs/MIMO_AUTH.md §6.2).
func clientSign(nonce, ssecurity string) string {
	sum := sha1.Sum([]byte("nonce=" + nonce + "&" + ssecurity))
	return url.QueryEscape(base64.StdEncoding.EncodeToString(sum[:]))
}

// serviceLoginURL builds the P1 URL.
func serviceLoginURL(sid string) string {
	return serviceLoginBase + "/pass/serviceLogin?_locale=zh_CN&_snsNone=true&sid=" +
		url.QueryEscape(sid) + "&_json=true"
}

// bootstrapNames are the account-domain cookie rows the exchange consumes
// (all four were present in the measured working P1 request).
var bootstrapNames = map[string]bool{
	"passToken": true,
	"userId":    true,
	"cUserId":   true,
	"uLocale":   true,
}

// pickBootstrap extracts the exchange's cookie set from an adopted jar: the
// passToken/userId/cUserId/uLocale rows, deduped by name with the account
// host (.account.xiaomi.com) preferred over the bare .xiaomi.com mirror.
func pickBootstrap(cookies []mimoCookie) []mimoCookie {
	byName := map[string]mimoCookie{}
	for _, c := range cookies {
		if !bootstrapNames[c.Name] || strings.TrimSpace(c.Value) == "" {
			continue
		}
		prev, ok := byName[c.Name]
		if !ok || rankAccountHost(c.Domain) < rankAccountHost(prev.Domain) {
			byName[c.Name] = c
		}
	}
	// Stable emission order for reproducible headers/tests.
	order := []string{"passToken", "userId", "cUserId", "uLocale"}
	out := make([]mimoCookie, 0, len(order))
	for _, name := range order {
		if c, ok := byName[name]; ok {
			out = append(out, c)
		}
	}
	return out
}

// rankAccountHost orders hosts for bootstrap dedup: 0 = .account.xiaomi.com,
// 1 = .xiaomi.com, 2 = anything else.
func rankAccountHost(domain string) int {
	d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(domain), "."))
	switch {
	case d == "account.xiaomi.com" || strings.HasSuffix(d, ".account.xiaomi.com"):
		return 0
	case d == "xiaomi.com" || strings.HasSuffix(d, ".xiaomi.com"):
		return 1
	default:
		return 2
	}
}

// errPassTokenExpired marks the P1 rejection whose fix is a desktop re-login
// (the passToken itself is dead — retrying cannot help).
var errPassTokenExpired = errors.New("passToken rejected by passport (re-login the desktop to refresh it)")

// nonceT absorbs both nonce wire shapes passport has shown: a bare JSON
// number (the measured real-machine shape, 2026-09-24) and a quoted string
// (historical fixtures). The literal digits must survive verbatim — P2's
// clientSign hashes them — so the raw token text is kept as-is: a plain
// string field cannot unmarshal the number form, and an any/float64 field
// would round a 19-digit nonce past float64's 53-bit mantissa, silently
// corrupting the signature. Empty/null values are NOT rejected here: the
// parse-level field check reports them after the passport code check, so a
// code=1020 rejection keeps precedence over shape complaints.
type nonceT string

func (n *nonceT) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		return nil // JSON null → zero value; the parse-level field check reports it.
	}
	if len(s) >= 2 && s[0] == '"' {
		var q string
		if err := json.Unmarshal(b, &q); err != nil {
			return err
		}
		s = strings.TrimSpace(q)
	}
	*n = nonceT(s)
	return nil
}

// serviceLoginResult is the parsed P1 payload.
type serviceLoginResult struct {
	Code     any    `json:"code"`
	Security string `json:"ssecurity"`
	Nonce    nonceT `json:"nonce"`
	Location string `json:"location"`
}

// parseServiceLogin strips the &&&START&&&/&&&END&&& framing the passport
// JSON endpoint wraps around its payload and validates the fields P2 needs.
func parseServiceLogin(body []byte) (*serviceLoginResult, error) {
	s := strings.TrimSpace(string(body))
	s = strings.TrimPrefix(s, "&&&START&&&")
	s = strings.TrimSuffix(s, "&&&END&&&")
	s = strings.TrimSpace(s)
	var res serviceLoginResult
	if err := json.Unmarshal([]byte(s), &res); err != nil {
		return nil, fmt.Errorf("serviceLogin json: %w", err)
	}
	switch code := res.Code.(type) {
	case float64:
		if code != 0 {
			// 1020-ish rejections mean the passToken no longer authenticates.
			return nil, fmt.Errorf("%w (passport code %v)", errPassTokenExpired, code)
		}
	case string:
		if code != "" && code != "0" {
			return nil, fmt.Errorf("%w (passport code %s)", errPassTokenExpired, code)
		}
	}
	if res.Security == "" || res.Nonce == "" || res.Location == "" {
		return nil, fmt.Errorf("serviceLogin response missing ssecurity/nonce/location")
	}
	return &res, nil
}

// exchangeResult carries one successful mint.
type exchangeResult struct {
	SID     string       // passport sid the ticket is bound to (mimosgp/mimopc)
	Cookies []mimoCookie // minted rows (serviceToken, userId, <sid>_ph, <sid>_slh)
}

// exchangeHTTPClient builds the exchange client. Redirects are NOT followed:
// P1 with _json=true answers 200-in-band, and a redirect means rejection we
// want to see raw instead of a login page we would have to parse.
func exchangeHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// exchangeServiceToken runs P1+P2 for one sid against the bootstrap cookies.
func exchangeServiceToken(bootstrap []mimoCookie, sid string) (*exchangeResult, error) {
	if sid == "" {
		return nil, fmt.Errorf("no passport sid for region (sgp/cn only until more are measured)")
	}
	if len(bootstrap) == 0 {
		return nil, fmt.Errorf("no bootstrap cookies (passToken/userId/cUserId)")
	}
	client := exchangeHTTPClient()

	// ---- P1: serviceLogin(_json) ----
	req, err := http.NewRequest(http.MethodGet, serviceLoginURL(sid), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", joinCookiePairs(bootstrap))
	if ua := loadedExchangeUserAgent(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("serviceLogin: %w", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("serviceLogin: HTTP %d (Unexpected redirect — the bootstrap cookies may be stale)", resp.StatusCode)
	}
	parsed, err := parseServiceLogin(raw)
	if err != nil {
		return nil, err
	}

	// ---- P2: STS ticket mint (cookieless by design) ----
	p2 := parsed.Location
	if !strings.Contains(p2, "?") {
		p2 += "?"
	}
	p2 += "&clientSign=" + clientSign(string(parsed.Nonce), parsed.Security)
	req2, err := http.NewRequest(http.MethodGet, p2, nil)
	if err != nil {
		return nil, err
	}
	if ua := loadedExchangeUserAgent(); ua != "" {
		req2.Header.Set("User-Agent", ua)
	}
	resp2, err := client.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("sts: %w", err)
	}
	// P2's answer is the Set-Cookie headers; the body is drained and discarded.
	_, _ = io.ReadAll(io.LimitReader(resp2.Body, 16<<10))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sts: HTTP %d (clientSign rejected)", resp2.StatusCode)
	}
	minted := mintedCookies(resp2.Cookies())
	if cookieValue(minted, "serviceToken") == "" {
		return nil, fmt.Errorf("sts: no serviceToken in Set-Cookie (sid=%s)", sid)
	}
	return &exchangeResult{SID: sid, Cookies: minted}, nil
}

// mintedCookies converts P2's Set-Cookie rows to jar rows. The STS host is a
// *.xiaomimimo.com server, so missing domain attributes default to the
// domain-wide form; expiry passes through (0 = session-scoped, the common
// shape — the ladder's re-exchange covers staleness).
func mintedCookies(set []*http.Cookie) []mimoCookie {
	out := make([]mimoCookie, 0, len(set))
	for _, c := range set {
		if c == nil || c.Name == "" || c.Value == "" {
			continue
		}
		domain := "." + c.Domain
		if c.Domain == "" || strings.EqualFold(c.Domain, "xiaomimimo.com") {
			domain = ".xiaomimimo.com"
		}
		out = append(out, mimoCookie{
			Name:    c.Name,
			Value:   c.Value,
			Domain:  domain,
			Path:    "/",
			Expires: c.Expires.Unix(),
		})
	}
	return out
}

// cookieValue returns the value of the first cookie with the given name.
func cookieValue(cookies []mimoCookie, name string) string {
	for _, c := range cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// joinCookiePairs renders name=value pairs in slice order.
func joinCookiePairs(cookies []mimoCookie) string {
	pairs := make([]string, 0, len(cookies))
	for _, c := range cookies {
		pairs = append(pairs, c.Name+"="+c.Value)
	}
	return strings.Join(pairs, "; ")
}

// buildCookieHeader renders the upstream Cookie header in the bundle's
// buildCookieHeader order: userId → serviceToken → cUserId → <sid>_ph.
// Measured on the real machine: a bare serviceToken cookie gets 302, the
// ordered set gets 200. Only these names ride the header — passToken/uLocale
// are login-side state that must not leak upstream (docs/MIMO_AUTH.md §3.3),
// and <sid>_slh was not part of the measured working set. When the jar holds
// both an adopted account-domain userId and the minted upstream one, the
// minted (xiaomimimo.com) row wins — it is the identity the ticket is bound
// to. sid="" skips the _ph slot (pre-mint or unknown region).
func buildCookieHeader(cookies []mimoCookie, sid string) string {
	ph := ""
	if sid != "" {
		ph = sid + "_ph"
	}
	order := []string{"userId", "serviceToken", "cUserId", ph}
	pairs := make([]string, 0, len(order))
	for _, name := range order {
		if name == "" {
			continue
		}
		value, ok := pickCookieValue(cookies, name)
		if ok {
			pairs = append(pairs, name+"="+value)
		}
	}
	return strings.Join(pairs, "; ")
}

// pickCookieValue selects one row for a header slot, preferring rows scoped
// to the upstream domain (the minted side) over adopted account-domain rows.
func pickCookieValue(cookies []mimoCookie, name string) (string, bool) {
	var upstream, anyCookie *mimoCookie
	for i := range cookies {
		if cookies[i].Name != name || strings.TrimSpace(cookies[i].Value) == "" {
			continue
		}
		if upstream == nil && isMimoUpstreamHost(cookies[i].Domain) {
			upstream = &cookies[i]
		}
		if anyCookie == nil {
			anyCookie = &cookies[i]
		}
	}
	if upstream != nil {
		return upstream.Value, true
	}
	if anyCookie != nil {
		return anyCookie.Value, true
	}
	return "", false
}

// exchangeForCredential re-mints the service ticket for one credential: try
// each candidate region until the passport accepts, then merge the minted
// rows into the jar (replacing stale service rows) and stamp
// region/sid/time. Persistence is the CALLER's job — the 401/302 ladder and
// the adoption flow use different save shapes (and tests stub neither).
// Returns false when there is no bootstrap material or every candidate
// fails; a passToken rejection short-circuits the remaining candidates
// (the account itself must re-login) and is logged redacted.
func exchangeForCredential(sa *storedAuth) bool {
	if sa == nil || authLaneFor(sa) != laneCookie {
		return false
	}
	bootstrap := pickBootstrap(sa.Auth.Cookies)
	if len(bootstrap) == 0 {
		return false
	}
	for _, region := range regionCandidates(loadedRegionMode(), sa.Auth.Region) {
		res, err := exchangeServiceToken(bootstrap, regionSID(region))
		if err != nil {
			if errors.Is(err, errPassTokenExpired) {
				// No candidate can succeed — the account itself must re-login.
				log.Printf("exchange: %v", err)
				return false
			}
			log.Printf("exchange: %s mint failed: %v", region, err)
			continue
		}
		mergeMinted(sa, res)
		return true
	}
	return false
}

// mergeMinted folds a successful mint into the credential: drop rows this
// mint supersedes (previous serviceToken/_ph/_slh and upstream userId), then
// append the fresh set and stamp the metadata.
func mergeMinted(sa *storedAuth, res *exchangeResult) {
	superseded := map[string]bool{
		"serviceToken":   true,
		res.SID + "_ph":  true,
		res.SID + "_slh": true,
	}
	kept := make([]mimoCookie, 0, len(sa.Auth.Cookies)+4)
	for _, c := range sa.Auth.Cookies {
		// Bootstrap userId (account domain) is kept; a previous mint's
		// upstream userId is replaced by the fresh one below.
		if c.Name == "userId" && isMimoUpstreamHost(c.Domain) {
			continue
		}
		if superseded[c.Name] && isMimoUpstreamHost(c.Domain) {
			continue
		}
		kept = append(kept, c)
	}
	sa.Auth.Cookies = append(kept, res.Cookies...)
	sa.Auth.Region = regionForSID(res.SID)
	sa.Auth.SID = res.SID
	sa.Auth.ExchangedAt = time.Now().Unix()
}

// regionForSID inverts regionSID (mimosgp→sgp, mimopc→cn; "" passthrough).
func regionForSID(sid string) string {
	switch sid {
	case "mimosgp":
		return "sgp"
	case "mimopc":
		return "cn"
	default:
		return ""
	}
}
