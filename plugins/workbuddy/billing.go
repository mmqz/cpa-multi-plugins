// billing.go owns the upstream billing API surface: check-in status, user
// resource (credits / packages), payment type, and the perform-* call wrappers
// for daily check-in and trial claim. Includes the shared JSON helpers used
// to tolerate the upstream's loosely-typed response shapes, and the region
// helpers that decide CN vs Global endpoint.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// isGlobalDomain reports whether the domain belongs to the international
// (www.workbuddy.ai) WorkBuddy service.  The CN service uses
// www.codebuddy.cn; Global uses www.workbuddy.ai.
func isGlobalDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// realm values used for login routing and panel display.
const (
	regionCN     = "cn"
	regionGlobal = "global"
	regionIntl   = "intl"
)

// isIntlDomain reports whether the domain belongs to the codebuddy.ai (Intl)
// realm adopted from the merged codebuddy-intl plugin (v0.11.0).
func isIntlDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	return d == "codebuddy.ai" || strings.HasSuffix(d, ".codebuddy.ai")
}

// panelRegion returns the display realm: cn | global | intl. Billing and
// lifecycle keep using accountRegion (cn|global) so Intl accounts preserve
// their exact pre-merge behavior (they ran the CN code path in the old
// codebuddy-intl plugin).
func panelRegion(sa *storedAuth) string {
	if sa != nil {
		if isGlobalDomain(sa.Auth.Domain) {
			return regionGlobal
		}
		if isIntlDomain(sa.Auth.Domain) {
			return regionIntl
		}
	}
	return regionCN
}

// upstreamBaseForRegion returns the chat/auth gateway for a login region.
func upstreamBaseForRegion(region string) string {
	switch region {
	case regionIntl:
		return upstreamBaseIntl
	case regionGlobal:
		return upstreamBaseGlobal
	default:
		return upstreamBaseCN
	}
}

// applyRealmHeaders adjusts protocol headers for the Intl (codebuddy.ai)
// realm: the Intl gateway expects the IDE client header set (X-IDE-Type
// "IDE", product 1.100.0) and no X-Requested-With. Runs AFTER
// applyPlatformHeaders so Intl values win for adopted codebuddy-intl
// accounts (their LoginPlatform is "ide").
func applyRealmHeaders(req *http.Request, sa *storedAuth) {
	if sa == nil || !isIntlDomain(sa.Auth.Domain) {
		return
	}
	req.Header.Del("X-Requested-With")
	req.Header.Set("X-IDE-Type", "IDE")
	req.Header.Set("X-IDE-Name", "CodeBuddy")
	req.Header.Set("X-IDE-Version", "1.100.0")
	req.Header.Set("X-Product-Version", "1.100.0")
}

// accountRegion returns "cn" or "global" based on the auth's domain field.
// Empty domain (legacy auth files) defaults to "cn" for backward compat.
func accountRegion(sa *storedAuth) string {
	if sa != nil {
		// Explicit region wins (written by login + import adoption).
		switch strings.ToLower(strings.TrimSpace(sa.Auth.Region)) {
		case "intl":
			return "intl"
		case "global":
			return "global"
		case "cn":
			return "cn"
		}
		// Legacy files: domain sniff. codebuddy.ai is the Intl IDE realm
		// (merged codebuddy-intl plugin) — it is NOT workbuddy.ai Global
		// and must not fall into the CN default (v0.12.15 fix: Intl
		// credentials surfaced as "CN" in the credential manager).
		d := strings.ToLower(strings.TrimSpace(sa.Auth.Domain))
		if isGlobalDomain(d) {
			return "global"
		}
		if d == "codebuddy.ai" || strings.HasSuffix(d, ".codebuddy.ai") {
			return "intl"
		}
	}
	return "cn"
}

// setBillingBase temporarily overrides billingBase for tests; returns a
// restore func.
func setBillingBase(s string) func() {
	old := billingBase
	billingBase = s
	return func() { billingBase = old }
}

// setBillingBaseGlobal temporarily overrides billingBaseGlobal for tests.
func setBillingBaseGlobal(s string) func() {
	old := billingBaseGlobal
	billingBaseGlobal = s
	return func() { billingBaseGlobal = old }
}

// setBillingBaseIntl temporarily overrides billingBaseIntl for tests.
// v0.9.60 (issue #39 CI follow-up): TestBillingHeaders_IntlRealm used to
// dial the real www.codebuddy.ai because only the CN/Global bases had test
// setters — the suite passed wherever the domain resolved (developer
// machines) and timed out on GitHub runners, whose DNS maps it to 0.0.0.1.
func setBillingBaseIntl(s string) func() {
	old := billingBaseIntl
	billingBaseIntl = s
	return func() { billingBaseIntl = old }
}

// billingBaseFor returns the billing API base URL for the given auth's domain.
// CN accounts → https://www.codebuddy.cn; Global → https://www.workbuddy.ai.
// Falls back to the test-overridable billingBase for CN/nil.
// billingBaseIntl is the CodeBuddy Intl (www.codebuddy.ai) billing base.
// v0.12.10 fix: Intl accounts were posting check-in / meter calls to the CN
// gas station (www.codebuddy.cn) where their Bearer tokens are unknown — the
// APISIX gateway answered with the 401 HTML page that surfaced as
// "parse failed: invalid character '<'" right after a successful Intl login.
var billingBaseIntl = "https://www.codebuddy.ai"

func billingBaseFor(sa *storedAuth) string {
	if sa != nil {
		if isGlobalDomain(sa.Auth.Domain) {
			return billingBaseGlobal
		}
		// v0.12.10 fix: Intl (codebuddy.ai) accounts must hit the Intl gas
		// station, not the CN one — CN's APISIX gateway rejects Intl Bearer
		// tokens with the 401 HTML page ("parse failed: invalid character
		// '<'") immediately after a successful Intl login.
		if isIntlDomain(sa.Auth.Domain) {
			return billingBaseIntl
		}
	}
	return billingBase
}

// -----------------------------------------------------------------------------
// Billing / check-in API calls
// -----------------------------------------------------------------------------

func billingHeaders(req *http.Request, sa *storedAuth) {
	req.Header.Set("Authorization", "Bearer "+sa.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if sa.Account.UID != "" {
		req.Header.Set("X-User-Id", sa.Account.UID)
	}
	if sa.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", sa.Account.EnterpriseID)
		req.Header.Set("X-Tenant-Id", sa.Account.EnterpriseID)
	}
	if sa.Auth.Domain != "" {
		req.Header.Set("X-Domain", sa.Auth.Domain)
	}
	// 2026-10-25 (per-account billing identity): the billing plane is where
	// N gateway-fronted accounts would otherwise present zero device
	// separation — one gateway shape on every call from every account. The
	// official non-chat family carries X-Machine-ID (OS hardware UUID) /
	// X-Session-ID (per-process UUID) plus a fresh X-Request-ID per call, so
	// a gateway derives the two stable ids per account — same account always
	// the same device, never shared across accounts — or the shared value
	// becomes a correlation key tying the whole pool to one machine
	// (workbuddy2api-hub wb_accounts.py / wb_fingerprint.py, its #448).
	// Shapes: hyphenated UUIDv4 for the stable ids (deriveAccountDeviceID),
	// bare 32-hex for the request id (accountRequestID).
	req.Header.Set("X-Machine-ID", deriveAccountDeviceID(sa, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountDeviceID(sa, "session"))
	req.Header.Set("X-Request-ID", accountRequestID())
	req.Header.Set("User-Agent", "CodeBuddy")
	// Intl realm parity (applyRealmHeaders): the codebuddy.ai gateway expects
	// the IDE client header set and no X-Requested-With. Without these the
	// request is treated as a browser call and bounced (401).
	if isIntlDomain(sa.Auth.Domain) {
		req.Header.Del("X-Requested-With")
		req.Header.Set("X-IDE-Type", "IDE")
		req.Header.Set("X-IDE-Name", "CodeBuddy")
		req.Header.Set("X-IDE-Version", "1.100.0")
		req.Header.Set("X-Product-Version", "1.100.0")
	}
}

func billingCall(sa *storedAuth, path string, body any) (json.RawMessage, error) {
	data, err := billingCallOnce(sa, path, body, 0)
	attempts := 1
	all5xx := is5xxBillingErr(err)
	for _, d := range billingRetryDelays {
		if err == nil || !isTransientBillingErr(err) {
			break
		}
		// v0.9.65: a connection-level failure (EOF / reset / TLS-class) on the
		// just-used dial path demotes the cached-good IP before the next
		// attempt. Poisoned DNS answers and dead CDN edges accept TCP and kill
		// the TLS flight — and without the demotion the dialer's own
		// "winning"-IP stickiness re-dials the same broken edge on every
		// remaining attempt, burning the whole ladder on one dead IP while
		// healthy candidates sit unused right behind it (2026-10-23 field
		// report: 4/4 attempts tls=EOF on the same sys-DNS IP).
		if isConnLevelBillingErr(err) {
			billingDemoteGoodIP(billingBaseFor(sa))
		}
		time.Sleep(d)
		// Retry ladder (v0.9.52): attempt 1 and 3 run on the rescue transport
		// (fresh connection per attempt, tcp4-first dialing) — the pooled path
		// just failed with a transport error, so hammering the exact same
		// socket state is how 3-for-3 EOF exhaustion happens. Attempt 2 forces
		// the HOST BRIDGE despite the codebuddy.ai direct bypass: the bridge
		// applies the host's own transport policy (config.yaml proxy-url), the
		// one path that works when the deployment's proxy lives only in CPA
		// config and the plugin hasn't seen a HostConfigSummary yet.
		data, err = billingCallOnce(sa, path, body, attempts)
		attempts++
		all5xx = all5xx && is5xxBillingErr(err)
	}
	// 0.9.68 (magpie packages/workbuddy 0.1.31 parity): a billing 401 gets
	// ONE token renewal and ONE resend, whatever the token's clock says.
	// The keepalive refresh keeps offline sessions alive on its own schedule;
	// a session that died between keepalive ticks used to surface as a hard
	// failure on every billing call until the next daily cycle.
	if err != nil && is401BillingRejection(err) {
		fresh, rerr := billing401RenewOnce(sa)
		switch {
		case rerr == nil && fresh != nil:
			data, err = billingCallOnce(fresh, path, body, 0)
			if err == nil {
				return data, nil
			}
			if is401BillingRejection(err) {
				err = fmt.Errorf("%w — renewed once and still refused: re-login required", err)
			}
		case isBilling401SessionDead(rerr):
			err = fmt.Errorf("%w — refresh token refused (session dead): re-login required", err)
		default:
			// Throttled / snapshot without host identity / no refresh token /
			// renewal failed for another reason: keep the plain 401 shape —
			// the next cycle retries the heal, or a concurrent renewal has
			// already refreshed the credential for callers that re-read it.
		}
	}
	if err != nil && isTransientBillingErr(err) {
		// Transport-class exhaustion: keep the underlying error first (its
		// `Post "url": EOF` shape stays greppable and transient-classified),
		// append the actionable deployment hint, then the staged net-diag
		// probe (v0.9.53: dns → tcp → tls → http per candidate IP) so a
		// field report pinpoints the failing stage instead of another bare
		// EOF. Probe results are cached for 30s and skipped entirely for
		// IP-literal/localhost bases (hermetic tests).
		//
		// Also reset dial state: stickiness clearly picked a losing path,
		// so forget the cached-good IP / dead marks and drop pooled
		// connections (a poisoned pooled conn would otherwise serve the
		// next billing call straight from the pool).
		billingResetDialState()
		if all5xx {
			// v0.9.65: every attempt got an HTTP answer back — the gateway is
			// REACHABLE; it (or a service behind it) is failing. Reporting
			// "unreachable" here sent a 2026-10-23 field report chasing local
			// network/proxy causes while the net-diag in the very same line
			// showed http=200 on direct probes: the failure was upstream
			// (code 10000, the billing gateway's internal keycloak call).
			return data, fmt.Errorf("%w — billing gateway answered HTTP 5xx on all %d attempts (upstream internal error: the gateway itself or a service behind it is failing — server-side, not a local network problem; the next scheduled check-in retries automatically)%s", err, attempts, billingNetDiagSuffix(billingBaseFor(sa)))
		}
		return data, fmt.Errorf("%w — billing gateway unreachable after %d attempts (direct pooled+rescue fresh-conn/v4 and host-bridge paths; set proxy-url in CPA config.yaml or HTTPS_PROXY on the host process if codebuddy.ai/workbuddy.ai needs a proxy from this network)%s", err, attempts, billingNetDiagSuffix(billingBaseFor(sa)))
	}
	return data, err
}

// isTransientBillingErr reports whether err came from an upstream 5xx or a
// transport failure (both retryable). 4xx and business-code errors are not.
//
// v0.12.64: the comment always promised transport retries but the impl only
// matched 5xx prefixes — transport EOFs ("Post \"https://...\": EOF" from the
// codebuddy.ai gateway closing the connection mid-request) surfaced straight
// to the panel as hard failures. Now classified: EOF / connection reset /
// broken pipe / client-timeout / TLS-handshake-timeout / dial failures.
// Business errors ("parse failed: ...", "code NNNN: ...") match none of these.
func isTransientBillingErr(err error) bool {
	if err == nil {
		return false
	}
	if is5xxBillingErr(err) {
		return true
	}
	// Application-layer answers first: the server SPOKE — a business code or a
	// non-JSON body came back with a non-5xx status. These are never transport
	// failures, even when the upstream's own error text embeds transport-shaped
	// substrings (2026-10-23 field report: code 10000's msg contains
	// `Get "http://keycloak-cluster-alias/...": EOF` — the server's INTERNAL
	// call failed; our transport worked and must not be retried as if it
	// hadn't).
	if isAppLayerBillingErr(err) {
		return false
	}
	transport := []string{
		": EOF",                     // Go http client: server closed without a response
		"connection reset",          // TCP RST mid-request
		"broken pipe",               // write after peer closed
		"Client.Timeout",            // http.Client Timeout exceeded
		"context deadline exceeded", // per-request ctx budget (host bridge too)
		"TLS handshake timeout",     // net/http handshake dial
		"i/o timeout",               // dial/read deadline
		"no such host",              // DNS miss (transient resolver flaps)
		"connection refused",        // listener temporarily down
	}
	for _, pat := range transport {
		if strings.Contains(msgOf(err), pat) {
			return true
		}
	}
	return false
}

// isAppLayerBillingErr reports whether err is an application-layer answer
// (business code envelope, non-JSON body parse failure, or credential-level
// gateway rejection) — shapes produced only after the transport succeeded.
func isAppLayerBillingErr(err error) bool {
	if err == nil {
		return false
	}
	msg := msgOf(err)
	return strings.HasPrefix(msg, "code=") ||
		strings.HasPrefix(msg, "parse failed:") ||
		strings.HasPrefix(msg, "credential rejected")
}

// is5xxBillingErr reports whether err is the upstream-5xx shape produced by
// billingCallOnce ("http 500 from ...") or the growth HTTP error family —
// the server ANSWERED, it just answered badly. Retryable, but fundamentally
// different from a transport failure: the network path is fine.
func is5xxBillingErr(err error) bool {
	if err == nil {
		return false
	}
	msg := msgOf(err)
	return strings.HasPrefix(msg, "http 5") || strings.HasPrefix(msg, "http=5") || strings.Contains(msg, "status 5")
}

// isConnLevelBillingErr reports whether err means the connection itself died
// (EOF / reset / TLS-class / IO-timeout) rather than the server answering
// (5xx — reachable, server-side) or the resolver failing. The retry ladder
// uses it to demote the cached-good dial IP: a TCP-good-but-TLS-dead edge
// produces exactly these errors, and re-dialing it on every retry burns the
// whole ladder on one broken candidate.
func isConnLevelBillingErr(err error) bool {
	if err == nil {
		return false
	}
	if is5xxBillingErr(err) {
		return false
	}
	// Application-layer answers (business code / parse failure / credential
	// rejection) reached us over a working transport — never conn-level, even
	// if the upstream's own error text embeds transport-shaped substrings.
	if isAppLayerBillingErr(err) {
		return false
	}
	msg := msgOf(err)
	for _, pat := range []string{
		": EOF",
		"server closed idle connection", // Go transport: pooled keep-alive conn cut by the peer (CDN/EdgeOne idle reaping) — 0.9.66, CI flake 2026-10-23
		"unexpected EOF",                // truncated response body mid-read — same transport class
		"connection reset",
		"broken pipe",
		"TLS handshake timeout",
		"tls:", // tls: handshake failure / cert verification / record-layer errors
		"i/o timeout",
		"Client.Timeout",
		"context deadline exceeded",
	} {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}

// msgOf renders err's message without a nil-pointer dance at every caller.
func msgOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// billingCallOnce performs one billing HTTP attempt. attempt 0 uses the pooled
// sharedHTTPClient; attempt 2 marks the context for the host-bridge fallback
// (host transport policy incl. config.yaml proxy-url); all other attempts >= 1
// mark the context so hostHTTPDoDirect routes through rescueHTTPClient (fresh
// connection per attempt, tcp4-first).
func billingCallOnce(sa *storedAuth, path string, body any, attempt int) (json.RawMessage, error) {
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader([]byte("{}"))
	}
	base := billingBaseFor(sa)
	// v0.9.48: bound the billing call with a context timeout so a hung
	// codebuddy.ai gateway connection (field report: EOF after 6.84s)
	// doesn't block the full 120s host-bridge ceiling. 15s is enough
	// for any legitimate meter/checkin/plan call (all are fast JSON APIs).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if attempt > 0 {
		if attempt == 2 {
			ctx = withHTTPBridge(ctx)
		} else {
			ctx = withHTTPRescue(ctx)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, reader)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	raw := resp.Body
	// Upstream 5xx is transient — classify it so billingCall can retry,
	// and keep a redacted response body snippet for diagnosis (A-42).
	if resp.StatusCode >= 500 {
		snippet := strings.TrimSpace(redactSecrets(string(raw)))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		return nil, fmt.Errorf("http %d from %s: %s", resp.StatusCode, path, snippet)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		snippet := strings.TrimSpace(redactSecrets(string(raw)))
		if len(snippet) > 120 {
			snippet = snippet[:120]
		}
		// v0.9.47: a 401/403 whose body is not even JSON is the gateway
		// bouncing the Bearer token before the app layer speaks JSON
		// (APISIX/nginx "401 Authorization Required" HTML page). Classify it
		// as the credential-level rejection it is — growthHTTPError carries
		// the status so growthErrStatus / isGrowthSessionDead recognize it —
		// instead of the misleading "parse failed: invalid character '<'"
		// that hid a dead session behind a JSON parse error.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &growthHTTPError{status: resp.StatusCode, msg: "credential rejected by gateway (non-JSON response): " + snippet}
		}
		// parse failed usually means upstream returned a non-JSON error page
		// (e.g. APISIX 401 HTML for session-dead). Include a redacted snippet
		// so the panel / logs can surface the real cause instead of a bare
		// "parse failed" (P0-2 UX: was impossible to distinguish session dead
		// from a malformed response).
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, snippet)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, truncateRedacted(env.Msg, 120))
	}
	return env.Data, nil
}

// cstZone is China Standard Time. Check-in calendars the upstream reports are
// CN-local calendar days, so "does checkin_dates contain today" must be
// evaluated in Asia/Shanghai — not in the host process's timezone.
var cstZone = time.FixedZone("CST", 8*60*60)

// parseCheckinStatusPayload decodes one check-in status body. hasTodayKey
// reports whether the payload explicitly carried a today_checked_in-style
// field — activity-shaped payloads (checkin-activity-status) intermittently
// omit it entirely, and a missing field must never be mistaken for "not
// checked in" (that conflation was the 2026-09-20 "已签到显示未签到" bug).
func parseCheckinStatusPayload(data json.RawMessage) (sum *checkinSummary, hasTodayKey bool, err error) {
	var m map[string]any
	if err = json.Unmarshal(data, &m); err != nil {
		return nil, false, err
	}
	sum = &checkinSummary{
		Active:          jsonBool(m, "active", "Active"),
		TodayCheckedIn:  jsonBool(m, "today_checked_in", "todayCheckedIn"),
		StreakDays:      jsonI64(m, "streak_days", "streakDays"),
		DailyCredit:     jsonI64(m, "daily_credit", "dailyCredit"),
		TodayCredit:     jsonI64(m, "today_credit", "todayCredit"),
		TotalCredits:    jsonI64(m, "total_credits", "totalCredits"),
		WeekCheckinDays: jsonI64(m, "week_checkin_days", "weekCheckinDays"),
		ActivityName:    jsonStr(m, "activity_name", "activityName"),
		Season:          jsonI64(m, "season", "season"),
	}
	if dates, ok := m["checkin_dates"].([]any); ok {
		for _, d := range dates {
			if s, ok := d.(string); ok {
				sum.CheckinDates = append(sum.CheckinDates, s)
			}
		}
	} else if dates, ok := m["checkinDates"].([]any); ok {
		for _, d := range dates {
			if s, ok := d.(string); ok {
				sum.CheckinDates = append(sum.CheckinDates, s)
			}
		}
	}
	_, hasTodayKey = m["today_checked_in"]
	if !hasTodayKey {
		_, hasTodayKey = m["todayCheckedIn"]
	}
	// Cross-check: when the payload carries the check-in calendar, "today is
	// in it" outranks an absent/false today_checked_in field.
	today := time.Now().In(cstZone).Format("2006-01-02")
	for _, d := range sum.CheckinDates {
		if strings.HasPrefix(d, today) {
			sum.TodayCheckedIn = true
			break
		}
	}
	return sum, hasTodayKey, nil
}

// mergeOR folds a second endpoint's view into the receiver. Booleans OR —
// the failure mode under fix is a false "not checked in", never a false
// "already" — while scalar/summary fields fill only when the receiver has
// nothing (first endpoint's richer view wins).
func (s *checkinSummary) mergeOR(other *checkinSummary) {
	if s == nil || other == nil {
		return
	}
	if other.TodayCheckedIn {
		s.TodayCheckedIn = true
	}
	if other.Active {
		s.Active = true
	}
	if s.StreakDays == 0 {
		s.StreakDays = other.StreakDays
	}
	if s.DailyCredit == 0 {
		s.DailyCredit = other.DailyCredit
	}
	if s.TodayCredit == 0 {
		s.TodayCredit = other.TodayCredit
	}
	if s.TotalCredits == 0 {
		s.TotalCredits = other.TotalCredits
	}
	if s.WeekCheckinDays == 0 {
		s.WeekCheckinDays = other.WeekCheckinDays
	}
	if s.ActivityName == "" {
		s.ActivityName = other.ActivityName
	}
	if s.Season == 0 {
		s.Season = other.Season
	}
	if len(s.CheckinDates) == 0 {
		s.CheckinDates = other.CheckinDates
	}
}

// fetchCheckinStatus resolves today's check-in state. 2026-09-20 field
// report: credentials that already checked in sometimes showed 未签到 in the
// panel. Root cause: the first endpoint's payload intermittently lacks
// today_checked_in (activity-shaped object); a missing field parsed as
// false, and that false poisoned the cache for a full TTL window. Now: a
// payload WITHOUT today evidence is treated as ambiguous — the second
// endpoint is consulted and both views OR-merge; a payload WITH evidence
// returns immediately (single upstream call in the common case, unchanged).
func fetchCheckinStatus(sa *storedAuth) (*checkinSummary, error) {
	var best *checkinSummary
	var lastErr error
	for _, path := range []string{"/v2/billing/meter/checkin-activity-status", "/v2/billing/meter/checkin-status"} {
		data, err := billingCall(sa, path, nil)
		if err != nil {
			lastErr = err
			continue
		}
		sum, hasTodayKey, perr := parseCheckinStatusPayload(data)
		if perr != nil {
			lastErr = perr
			continue
		}
		if best == nil {
			best = sum
		} else {
			best.mergeOR(sum)
		}
		if hasTodayKey || sum.TodayCheckedIn {
			return best, nil // authoritative today evidence — stop probing
		}
		// Ambiguous payload: probe the other endpoint before answering.
	}
	if best != nil {
		return best, nil
	}
	return nil, lastErr
}

// packageRemainUsed picks current-cycle remain/used/size for one package.
// Prefer cycle metrics whenever CycleCapacitySize is present; used = size−remain
// so missing CycleCapacityUsed never under-reports consumption.
// Fall back to lifetime Capacity* only when cycle fields are absent entirely.
//
// Daily check-in adds NEW packages (size grows) — capacity grant, not negative
// consumption. Track consumption via used (size−remain), not via remain alone.
func packageRemainUsed(a resourcePackage) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		// If upstream reports a higher explicit used, trust the larger figure.
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			// Keep remain consistent when possible.
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	if a.CycleCapacityRemain > 0 || a.CycleCapacityUsed > 0 {
		remain = a.CycleCapacityRemain
		used = a.CycleCapacityUsed
		// A-41: clamp negatives (branch1 already clamps; branch2/3 did not).
		if remain < 0 {
			remain = 0
		}
		if used < 0 {
			used = 0
		}
		size = remain + used
		if a.CapacitySize > size {
			size = a.CapacitySize
			if size >= remain {
				used = size - remain
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	// A-41: lifetime branch also clamps negative remain/used.
	if remain < 0 {
		remain = 0
	}
	if used < 0 {
		used = 0
	}
	if size <= 0 {
		size = remain + used
	}
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

func fetchUserResource(sa *storedAuth) (*creditsSummary, error) {
	now := time.Now()
	// Status 0=active, 3=exhausted-but-still-listed. PageSize 100 covers the
	// multi-pack free accounts we see in production; paginate if TotalCount
	// ever exceeds it.
	const pageSize = 100
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 pageSize,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := billingCall(sa, "/v2/billing/meter/get-user-resource", body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Response struct {
			Data struct {
				TotalCount  int64             `json:"TotalCount"`
				TotalDosage int64             `json:"TotalDosage"` // package capacity pool, NOT consumption
				Accounts    []resourcePackage `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	// Aggregate ALL packages (体验版 + 多个签到/裂变包 + 其它赠送包).
	// Remain = currently spendable. Used = consumed this cycle. Size = capacity.
	// Daily check-in adds packages → Size and Remain go UP; that is grant, not usage.
	sum := &creditsSummary{}
	for _, a := range resp.Response.Data.Accounts {
		remain, used, size := packageRemainUsed(a)
		sum.TotalRemain += remain
		sum.TotalUsed += used
		sum.TotalSize += size
		sum.Packages = append(sum.Packages, packageSummary{
			Name:       a.PackageName,
			Remain:     remain,
			Used:       used,
			Size:       size,
			CycleStart: a.CycleStartTime,
			CycleEnd:   a.CycleEndTime,
		})
	}
	sum.PackCount = len(sum.Packages)
	// Reconcile used with size-remain so UI totals always add up when size known.
	if sum.TotalSize > 0 {
		derived := sum.TotalSize - sum.TotalRemain
		if derived < 0 {
			derived = 0
		}
		// Prefer the larger of reported-used vs size-remain (never under-report spend).
		if derived > sum.TotalUsed {
			sum.TotalUsed = derived
		}
	}
	// Upstream TotalDosage is the capacity pool (~sum of package sizes), not spend.
	// Use it only as a size floor when pack sizes look incomplete.
	if dosage := resp.Response.Data.TotalDosage; dosage > sum.TotalSize {
		sum.TotalSize = dosage
		derived := sum.TotalSize - sum.TotalRemain
		if derived < 0 {
			derived = 0
		}
		if derived > sum.TotalUsed {
			sum.TotalUsed = derived
		}
	}
	_ = resp.Response.Data.TotalCount
	// v0.9.67 (issue #55): an enterprise-space account answers the personal
	// meter plane with HTTP 200 + EMPTY data (TotalCount 0, Accounts null) —
	// not an error — and folding that into a zero summary made the panel read
	// "余0 已用0 · free" for an account holding a live enterprise allowance,
	// and kept a disabled one from ever re-enabling (shouldReenableCN wants
	// TotalRemain > 0). Enterprise accounts with empty personal data fall back
	// to the enterprise meter plane; a personal account's genuinely-empty
	// result (no packs yet) never enters the fallback.
	if len(resp.Response.Data.Accounts) == 0 && isEnterpriseAccount(sa) {
		if u, uerr := fetchEnterpriseUsage(sa); uerr == nil {
			return u.creditsSummary(), nil
		}
		// Both planes came back empty: report unknown (nil) rather than a
		// zero summary — "no data" must not render as "exhausted".
		return nil, fmt.Errorf("enterprise account: personal meter empty and enterprise usage unavailable")
	}
	return sum, nil
}

func fetchPaymentType(sa *storedAuth) string {
	// v0.9.67 (issue #55): an enterprise account is not on the personal
	// payment ladder — get-payment-type answers "free" there, which the panel
	// used to render as the plan. Name the plane for what it is.
	if isEnterpriseAccount(sa) {
		return "enterprise"
	}
	data, err := billingCall(sa, "/v2/billing/meter/get-payment-type", nil)
	if err != nil {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	if s, ok := m["paymentType"].(string); ok {
		return s
	}
	return ""
}

func performCheckinCall(sa *storedAuth) (map[string]any, error) {
	// v0.9.67 (issue #55): the daily-checkin plane rejects enterprise
	// accounts with 400 biz 10001 "企业账号不支持该操作" (and their
	// checkin-activity-status reads active:false). Short-circuit the manual
	// press with the reason instead of a doomed upstream call.
	if isEnterpriseAccount(sa) {
		return enterpriseCheckinRefusal(), nil
	}
	data, err := billingCall(sa, "/v2/billing/meter/daily-checkin", nil)
	if err != nil {
		// billingCall returns business errors (code != 0) as Go errors; surface
		// them as a structured result so the panel can show "already checked in".
		return map[string]any{"success": false, "message": err.Error()}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	// Explicitly set success as bool — upstream may return "true" (string)
	// which would cause downstream `out["success"] == true` to fail silently
	// (P1-2 logic bug: type mismatch on success field).
	m["success"] = true
	return m, nil
}

// performTrialCall claims the one-time expert trial pack for a Global account.
// Endpoint: POST /billing/ide/trial (note: NOT under /v2/billing/meter/).
// First call: success, +250 credits, 14-day "CodeBuddy One-time Free 2-Week
// Pro Plan Trial".
// Repeat call: code=14051 "has applied trial" — surfaced as already_claimed.
func performTrialCall(sa *storedAuth) (map[string]any, error) {
	data, err := billingCall(sa, "/billing/ide/trial", nil)
	if err != nil {
		msg := err.Error()
		// code=14051 means the trial has already been claimed — not a real error.
		if strings.Contains(msg, "14051") {
			return map[string]any{
				"success":         false,
				"message":         "已领取过专家加油包",
				"already_claimed": true,
			}, nil
		}
		return map[string]any{"success": false, "message": msg}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m["success"] = true
	return m, nil
}

// hasTrialPack reports whether the credits summary already contains the
// Global expert trial pack (one-time, 14-day, 250 credits). Used for the
// panel "claim trial" button state (trial_claimed).
//
// Do NOT match bare Chinese "体验": CN free-tier is literally named
// "CodeBuddy个人体验版" / "体验版" and must remain unclaimed-looking for Global
// trial UI (A-18). Prefer English trial markers from live Global packs.
func hasTrialPack(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	for _, p := range cr.Packages {
		name := strings.ToLower(strings.TrimSpace(p.Name))
		if name == "" {
			continue
		}
		// Live Global: "CodeBuddy One-time Free 2-Week Pro Plan Trial"
		if strings.Contains(name, "trial") {
			return true
		}
		// Alternate English shapes (keep without bare "体验")
		if strings.Contains(name, "pro plan") && (strings.Contains(name, "free") || strings.Contains(name, "one-time") || strings.Contains(name, "2-week") || strings.Contains(name, "2 week")) {
			return true
		}
		// Explicit expert-pack Chinese labels only — never bare 体验/体验版.
		if strings.Contains(name, "专家加油") || strings.Contains(name, "专家体验包") {
			return true
		}
	}
	return false
}

// isCreditsExhausted is the shared "耗尽" definition for panel + scheduler.
// Exhausted = we have usage signal and no remaining credits.
// Missing credits data is NOT exhausted (unknown).
func isCreditsExhausted(cr *creditsSummary) bool {
	if cr == nil {
		return false
	}
	if cr.TotalRemain > 0 {
		return false
	}
	// remain==0: exhausted only when we know there was/is a package total
	// (used>0, size>0, or packages present). Pure zero with no packages = no data.
	if cr.TotalUsed > 0 || cr.TotalSize > 0 {
		return true
	}
	return len(cr.Packages) > 0
}

func jsonBool(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case float64:
				return t != 0
			case string:
				return t == "true" || t == "1"
			}
		}
	}
	return false
}

func jsonI64(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case float64:
				return int64(t)
			case int64:
				return t
			case string:
				var n int64
				fmt.Sscanf(t, "%d", &n)
				return n
			}
		}
	}
	return 0
}

func jsonStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	return ""
}
