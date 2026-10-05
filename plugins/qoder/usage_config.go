// usage_config.go decodes plugin config from config_yaml on every
// register/reconfigure call and resolves the CPAMP usage report URL/key.
// All plugin-level config lives here so the rest of the plugin reads
// consistent, lock-protected snapshots.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// check-in schedule: 10:00, 15:00 and 21:00 local time.
// 10 replaces 9: the CN daily check-in activity opens at 10:00 local
// (hope0719/qoder-check-in README, 2026-09-18~09-30 activity — "每天
// 10:00 起可领 100 Credits"); a 09:00 tick hits "活动未开始" and fails.
// 21 stays as the evening retry/keepalive companion.
// 15 joins in v0.8.57 (field report "init 还是有概率不能签到"): a mid-day
// compensation tick between the 10:00 refresh and the evening companion —
// when the 10:00 state was bad (hidden row + cold probe memo, a latched
// NOT_ELIGIBLE spanning the refresh, upstream grant lag), the account
// self-heals the same afternoon instead of waiting for 21:00. Every gate
// in front of the claim is idempotent (TodayCheckedIn / ALREADY_CLAIMED /
// per-round probe latch), so the extra tick costs a status read per account
// on the healthy path.
var checkinHours = []int{10, 15, 21}

// plugin-level config decoded from plugin.register/reconfigure config_yaml.
var (
	checkinAuto   = true // enabled by default
	checkinAutoMu sync.RWMutex

	// usageReportURL / usageReportKey: POST NDJSON to CPA-Manager-Plus
	// /v0/management/usage/import (only path that reaches request monitoring;
	// c-shared plugins cannot use host usage.DefaultManager/redisqueue).
	//
	// Resolution order (community-style, like codex-auth-importer env injection):
	//  1) plugins.configs.qoderwork.usage_report_* in config.yaml
	//  2) env USAGE_REPORT_URL / USAGE_REPORT_KEY / CPAMP_ADMIN_KEY
	//  3) secret files (docker secrets / bind-mount), e.g. /run/secrets/cpamp_admin_key
	// Default URL targets the compose service name of CPA-Manager-Plus.
	usageReportURL = defaultUsageReportURL
	usageReportKey = ""
	usageReportMu  sync.RWMutex

	// managementAPIKey: plugin-layer auth for /v0/management/plugins/qoderwork/*
	// write endpoints. When empty, plugin relies on host-side auth (CPA's
	// management middleware) — that's the historical default and stays
	// backward-compatible. When set via config_yaml management_key: or env
	// WB_MANAGEMENT_KEY, handleManagement enforces constant-time Bearer match
	// plus per-IP token-bucket rate limiting on mutating endpoints.
	managementAPIKey   = ""
	managementAPIKeyMu sync.RWMutex

	// streamHeadTimeoutSecs: config_yaml stream_head_timeout, integer seconds,
	// default 0 = disabled. When > 0 the async streaming hand-off waits up to
	// that long for the first decisive upstream frame, so a failure that lands
	// before the model starts answering can still be reported to the host as a
	// plain failed request carrying a real HTTP status (see streamHeadGate).
	streamHeadTimeoutSecs int
	streamHeadTimeoutMu   sync.RWMutex

	// claimUnverified: config_yaml claim_unverified, default false. When true
	// the Pro-claim flow may claim a CLAIMABLE campaigns row whose reward
	// face value could NOT be verified (unreadable or absent) instead of
	// only reporting it. Rows that reveal a readable reward are never
	// claimed here — the flag only covers the cannot-verify bucket.
	claimUnverified   bool
	claimUnverifiedMu sync.RWMutex

	// checkinRoundSeeds: config_yaml checkin_round_seeds, v0.8.58. Operator
	// supplied per-region daily-round campaign UUIDs for the bypass probe's
	// last-resort fallback ("intl=01a0…,cn=01a0…"). Deployments whose
	// campaigns list is permanently identity-filtered (Intl + derived
	// machine identity) never see the daily row, so their probe memo never
	// seeds — a seed id lets check-in still claim. The upstream claim
	// verdict stays the authority: a stale seed answers typed
	// CAMPAIGN_NOT_ACTIVE and is retried after the 30-min latch expires.
	checkinRoundSeeds   map[string]string
	checkinRoundSeedsMu sync.RWMutex
)

// Default URL tries localhost first (works for both bare-metal and Docker
// host-network), falls back to Docker compose service name. The probe runs
// once at configure() time; a reachable endpoint wins.
//
// For users who run CPA Manager Plus on a different host/port, set
// usage_report_url in plugin config or env USAGE_REPORT_URL.
const defaultUsageReportURL = "http://127.0.0.1:18317/v0/management/usage/import"

const fallbackUsageReportURL = "http://cpa-manager-plus:18317/v0/management/usage/import"

// configure decodes plugin config from the lifecycle request.
// v0.8.29 (issue #24): login_region is STICKY — it only changes when the
// incoming config explicitly carries the key (same semantics trae shipped in
// v0.12.3). The host may resend Register/Reconfigure with a bare or foreign
// config block (e.g. during auth-store churn); resetting to cn on those made
// intl logins flip regions mid-flight. Values are read from a real YAML
// decode first (yaml.v3 rides in via the SDK), so flow-style one-liners
// (`trae: {enabled: true, login_region: intl}`) and JSON payloads parse too;
// the historical line-scan stays as the fallback for malformed YAML.
func configure(raw []byte) {
	// Parse config without holding any lock (fixes nested-lock hazard).
	nextCheckinAuto := true
	nextLifecycleAuto := true
	nextSchedulerMode := schedulerModeOff // reset to default on reconfigure
	nextKeepaliveAuto := true
	nextLoginRegion := ""  // sticky: empty = keep current (issue #24)
	nextLoginDialect := "" // sticky: empty = keep current (v0.8.40)
	nextMgmtKey := ""
	nextStreamHeadTimeout := 0
	nextClaimUnverified := false
	nextRoundSeeds := ""

	cfgURL, cfgKey := "", ""
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err == nil {
			// v0.8.29 (issue #24): decode the payload as real YAML
			// first — yaml.v3 already rides in via the SDK, and this
			// sees flow-style one-liners and JSON that the line-scan
			// below can never match. Line-scan results stay
			// authoritative for block style (both agree there); the
			// map only ADDS keys the scan cannot see.
			if m, ok := decodePluginConfigYAML(req.ConfigYAML); ok {
				if v, present := m["login_region"]; present {
					nextLoginRegion = normalizeRegion(configScalarString(v))
				}
				if v, present := m["login_dialect"]; present {
					if d := strings.ToLower(configScalarString(v)); d == loginDialectCockpit || d == loginDialectDesktop {
						nextLoginDialect = d
					}
				}
				if v, present := m["checkin_auto"]; present {
					nextCheckinAuto = configScalarBool(v)
				}
				if v, present := m["lifecycle_auto"]; present {
					nextLifecycleAuto = configScalarBool(v)
				}
				if v, present := m["token_keepalive"]; present {
					nextKeepaliveAuto = configScalarBool(v)
				}
				if v, present := m["scheduler_mode"]; present && configScalarString(v) == schedulerModeCredits {
					nextSchedulerMode = schedulerModeCredits
				}
				if v, present := m["usage_report_url"]; present {
					cfgURL = configScalarString(v)
				}
				if v, present := m["usage_report_key"]; present {
					cfgKey = configScalarString(v)
				}
				if v, present := m["management_key"]; present {
					nextMgmtKey = configScalarString(v)
				}
				if v, present := m["stream_head_timeout"]; present {
					if secs, errParse := strconv.Atoi(configScalarString(v)); errParse == nil {
						nextStreamHeadTimeout = secs
					}
				}
				if v, present := m["claim_unverified"]; present {
					nextClaimUnverified = configScalarBool(v)
				}
				if v, present := m["checkin_round_seeds"]; present {
					nextRoundSeeds = configScalarString(v)
				}
			}
			for _, line := range strings.Split(string(req.ConfigYAML), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "checkin_auto:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "checkin_auto:"))
					nextCheckinAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "lifecycle_auto:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "lifecycle_auto:"))
					v = strings.Trim(v, "\"'")
					nextLifecycleAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "scheduler_mode:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "scheduler_mode:"))
					v = strings.Trim(v, "\"'")
					if v == schedulerModeCredits {
						nextSchedulerMode = schedulerModeCredits
					}
				}
				if strings.HasPrefix(line, "usage_report_url:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "usage_report_url:"))
					cfgURL = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "usage_report_key:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "usage_report_key:"))
					cfgKey = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "management_key:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "management_key:"))
					nextMgmtKey = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "token_keepalive:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "token_keepalive:"))
					v = strings.Trim(v, "\"'")
					nextKeepaliveAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "login_region:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "login_region:"))
					v = strings.Trim(v, "\"'")
					nextLoginRegion = normalizeRegion(v)
				}
				if strings.HasPrefix(line, "login_dialect:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "login_dialect:"))
					v = strings.Trim(v, "\"'")
					if d := strings.ToLower(v); d == loginDialectCockpit || d == loginDialectDesktop {
						nextLoginDialect = d
					}
				}
				if strings.HasPrefix(line, "stream_head_timeout:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "stream_head_timeout:"))
					v = strings.TrimSpace(strings.Trim(v, "\"'"))
					// A non-integer value leaves the default (0 = off): the gate
					// must never turn a typo in config.yaml into a stall.
					if secs, errParse := strconv.Atoi(v); errParse == nil {
						nextStreamHeadTimeout = secs
					}
				}
				if strings.HasPrefix(line, "claim_unverified:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "claim_unverified:"))
					nextClaimUnverified = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "checkin_round_seeds:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "checkin_round_seeds:"))
					nextRoundSeeds = strings.Trim(v, "\"'")
				}
			}
		}
	}

	// Apply each setting under its own lock — no nesting.
	checkinAutoMu.Lock()
	checkinAuto = nextCheckinAuto
	checkinAutoMu.Unlock()

	lifecycleAutoMu.Lock()
	lifecycleAuto = nextLifecycleAuto
	lifecycleAutoMu.Unlock()

	schedulerModeMu.Lock()
	schedulerMode = nextSchedulerMode
	schedulerModeMu.Unlock()

	keepaliveAutoMu.Lock()
	keepaliveAuto = nextKeepaliveAuto
	keepaliveAutoMu.Unlock()

	// Sticky (issue #24): only an explicit login_region key moves the
	// pointer; bare/foreign reconfigures keep whatever was configured.
	if nextLoginRegion != "" {
		loginRegionMu.Lock()
		if loginRegion != nextLoginRegion {
			log.Printf("qoder: login_region=%s applied (new logins target %s)", nextLoginRegion, strings.ToUpper(nextLoginRegion))
		}
		loginRegion = nextLoginRegion
		loginRegionMu.Unlock()
	}

	// Sticky (v0.8.40): only an explicit login_dialect key moves the pointer.
	if nextLoginDialect != "" {
		loginDialectMu.Lock()
		if loginDialect != nextLoginDialect {
			log.Printf("qoder: login_dialect=%s applied (new intl logins use the %s wire dialect)", nextLoginDialect, nextLoginDialect)
		}
		loginDialect = nextLoginDialect
		loginDialectMu.Unlock()
	}

	claimUnverifiedMu.Lock()
	claimUnverified = nextClaimUnverified
	claimUnverifiedMu.Unlock()

	applyRoundSeeds(nextRoundSeeds)

	setStreamHeadTimeout(nextStreamHeadTimeout)

	// management key: config_yaml > env > keep existing. Empty stays empty
	// (plugin-layer auth disabled, host middleware still guards).
	if nextMgmtKey == "" {
		nextMgmtKey = strings.TrimSpace(os.Getenv("WB_MANAGEMENT_KEY"))
	}
	managementAPIKeyMu.Lock()
	managementAPIKey = nextMgmtKey
	managementAPIKeyMu.Unlock()

	resolveUsageReport(cfgURL, cfgKey)
	ensureScheduler()
}

// parseRoundSeeds turns the config scalar "intl=<uuid>,cn=<uuid>" (or
// space/semicolon separated) into the per-region map. Entries whose value is
// not a 36-char UUID shape are dropped silently — a pasted act- key must
// never ride the claim path (the upstream demands the UUID form; live
// verified 2026-10-05). Unknown region labels are kept normalized so
// "global=" maps to intl.
func parseRoundSeeds(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' }) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		region := normalizeRegion(k)
		id := strings.TrimSpace(v)
		if !roundSeedUUIDShape(id) {
			continue
		}
		out[region] = strings.ToLower(id)
	}
	return out
}

// roundSeedUUIDShape matches the canonical 8-4-4-4-12 hex UUID the claim
// endpoint demands.
func roundSeedUUIDShape(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}

// applyRoundSeeds stores the operator's seed map (nil/empty clears it).
func applyRoundSeeds(s string) {
	parsed := parseRoundSeeds(s)
	checkinRoundSeedsMu.Lock()
	checkinRoundSeeds = parsed
	checkinRoundSeedsMu.Unlock()
}

// operatorRoundSeed returns the operator's seed for one region ("" when
// unset).
func operatorRoundSeed(region string) string {
	checkinRoundSeedsMu.RLock()
	defer checkinRoundSeedsMu.RUnlock()
	return checkinRoundSeeds[region]
}

// setStreamHeadTimeout stores the head-gate window in seconds. Negative values
// clamp to 0 (= disabled): the gate is opt-in, and "off" must be reachable from
// config.yaml alone (stream_head_timeout: -1 is a plausible way to ask for it).
func setStreamHeadTimeout(secs int) {
	if secs < 0 {
		secs = 0
	}
	streamHeadTimeoutMu.Lock()
	streamHeadTimeoutSecs = secs
	streamHeadTimeoutMu.Unlock()
}

// streamHeadTimeout returns the configured head-gate window. Zero disables the
// gate, which keeps the async streaming hand-off byte-identical to v0.12.84.
func streamHeadTimeout() time.Duration {
	streamHeadTimeoutMu.RLock()
	secs := streamHeadTimeoutSecs
	streamHeadTimeoutMu.RUnlock()
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// claimUnverifiedEnabled reports whether the Pro-claim flow may claim a
// CLAIMABLE row whose reward face value could not be verified. Default
// off — the face-value gate stays the safe default; this is an explicit
// per-deployment opt-in.
func claimUnverifiedEnabled() bool {
	claimUnverifiedMu.RLock()
	defer claimUnverifiedMu.RUnlock()
	return claimUnverified
}

// resolveUsageReport fills usageReportURL/key from config → env → secret files.
// Mirrors community plugins that inject management keys via env/build (e.g.
// codex-auth-importer CODEX_AUTH_IMPORTER_MANAGEMENT_KEY), not plaintext CPA
// remote-management.secret-key (that field is bcrypt-hashed).
func resolveUsageReport(cfgURL, cfgKey string) {
	url := firstNonEmpty(
		strings.TrimSpace(cfgURL),
		strings.TrimSpace(os.Getenv("USAGE_REPORT_URL")),
		strings.TrimSpace(os.Getenv("CPAMP_USAGE_IMPORT_URL")),
	)
	key := firstNonEmpty(
		strings.TrimSpace(cfgKey),
		strings.TrimSpace(os.Getenv("USAGE_REPORT_KEY")),
		strings.TrimSpace(os.Getenv("CPAMP_ADMIN_KEY")),
		strings.TrimSpace(os.Getenv("CPA_MANAGER_ADMIN_KEY")),
		readSecretFile(os.Getenv("USAGE_REPORT_KEY_FILE")),
		readSecretFile(os.Getenv("CPAMP_ADMIN_KEY_FILE")),
		readSecretFile(os.Getenv("CPA_MANAGER_ADMIN_KEY_FILE")),
		// docker compose secrets default path
		readSecretFile("/run/secrets/cpamp_admin_key"),
		readSecretFile("/run/secrets/cpamp-admin-key"),
		// optional bind-mounts used on this host
		readSecretFile("/CLIProxyAPI/secrets/cpamp-admin-key"),
		readSecretFile("/CLIProxyAPI/secrets/cpamp_admin_key"),
	)
	// v0.12.7: without an admin key every report is a guaranteed 401, and even
	// the unauthenticated reachability probe alone trips CPA's management
	// anti-brute-force ban — locking the user out of their own management UI.
	// Disable reporting entirely unless a key is configured.
	if strings.TrimSpace(key) == "" {
		url = ""
	} else if url == "" {
		url = probeUsageReportURL(key)
	}
	usageReportMu.Lock()
	usageReportURL = url
	usageReportKey = key
	usageReportMu.Unlock()
}

// probeUsageReportURL tries localhost first (bare-metal + Docker host-network),
// then Docker compose service name. v0.12.7: the probe carries the admin key
// and 401/403 responses disqualify a candidate — selecting an endpoint that
// rejects us only feeds CPA's management brute-force ban. Returns "" when no
// candidate accepts the key (reporting stays disabled; forwardUsageToCPAMP
// already skips on empty URL).
func probeUsageReportURL(key string) string {
	for _, candidate := range []string{defaultUsageReportURL, fallbackUsageReportURL} {
		if probeURL(candidate, 2*time.Second, key) {
			return candidate
		}
	}
	return ""
}

// probeURL does a quick authenticated GET to check if the endpoint accepts us.
func probeURL(target string, timeout time.Duration, key string) bool {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	if strings.TrimSpace(key) != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// v0.12.7: 401/403 = reachable but hostile to our reports — unusable.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return false
	}
	return resp.StatusCode > 0
}

func readSecretFile(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
