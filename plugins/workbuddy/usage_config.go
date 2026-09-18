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
	"strings"
	"sync"
	"time"
)

// check-in schedule: 09:00 and 21:00 local time.
var checkinHours = []int{9, 21}

// plugin-level config decoded from plugin.register/reconfigure config_yaml.
var (
	checkinAuto   = true // enabled by default
	checkinAutoMu sync.RWMutex

	// tasksAuto gates the growth-center daily bonus loop (task center, v0.9.13):
	// activity report / makeup / accept / buddy travel / streak redeem /
	// lottery / reward claims. Rides the same 09:00/21:00 ticks as check-in
	// (only fires when checkin_auto is also on, since that owns the tick).
	tasksAuto   = true // enabled by default
	tasksAutoMu sync.RWMutex

	// loginPlatform selects the client variant used for NEW logins:
	// "CLI" (workbuddy) or "ide" (CodeBuddy IDE). Configured via
	// config_yaml login_platform: and read at auth.login_start time.
	loginPlatform   = "CLI"
	loginPlatformMu sync.RWMutex

	// loginRegion selects the upstream realm for NEW logins: "cn"
	// (copilot.tencent.com, default), "intl" (codebuddy.ai, IDE client;
	// merged codebuddy-intl plugin v0.11.0) or "global" (workbuddy.ai).
	loginRegion   = regionCN
	loginRegionMu sync.RWMutex

	// usageReportURL / usageReportKey: POST NDJSON to CPA-Manager-Plus
	// /v0/management/usage/import (only path that reaches request monitoring;
	// c-shared plugins cannot use host usage.DefaultManager/redisqueue).
	//
	// Resolution order (community-style, like codex-auth-importer env injection):
	//  1) plugins.configs.workbuddy.usage_report_* in config.yaml
	//  2) env USAGE_REPORT_URL / USAGE_REPORT_KEY / CPAMP_ADMIN_KEY
	//  3) secret files (docker secrets / bind-mount), e.g. /run/secrets/cpamp_admin_key
	// Default URL targets the compose service name of CPA-Manager-Plus.
	usageReportURL = defaultUsageReportURL
	usageReportKey = ""
	usageReportMu  sync.RWMutex

	// managementAPIKey: plugin-layer auth for /v0/management/plugins/workbuddy/*
	// write endpoints. When empty, plugin relies on host-side auth (CPA's
	// management middleware) — that's the historical default and stays
	// backward-compatible. When set via config_yaml management_key: or env
	// WB_MANAGEMENT_KEY, handleManagement enforces constant-time Bearer match
	// plus per-IP token-bucket rate limiting on mutating endpoints.
	managementAPIKey   = ""
	managementAPIKeyMu sync.RWMutex

	// pinnedModels: per-realm user-pinned model ID lists from config_yaml
	// models_cn / models_global / models_intl (comma-separated upstream IDs).
	// When set for a realm, the credential model output is exactly this list
	// and dynamic discovery is skipped for it (v0.12.19) — the user-written
	// "which models this credential supports" contract. Reconfigure with the
	// key absent resets that realm to discovery + static fallback.
	pinnedModels   = map[string][]string{}
	pinnedModelsMu sync.RWMutex
)

// pinnedModelIDsForRealm snapshots the pinned ID list for a realm (nil when
// the realm has no pins).
func pinnedModelIDsForRealm(realm string) []string {
	pinnedModelsMu.RLock()
	defer pinnedModelsMu.RUnlock()
	ids, ok := pinnedModels[realm]
	if !ok {
		return nil
	}
	return append([]string(nil), ids...)
}

// Default URL tries localhost first (works for both bare-metal and Docker
// host-network), falls back to Docker compose service name. The probe runs
// once at configure() time; a reachable endpoint wins.
//
// For users who run CPA Manager Plus on a different host/port, set
// usage_report_url in plugin config or env USAGE_REPORT_URL.
const defaultUsageReportURL = "http://127.0.0.1:18317/v0/management/usage/import"

const fallbackUsageReportURL = "http://cpa-manager-plus:18317/v0/management/usage/import"

// configure decodes plugin config from the lifecycle request.
func configure(raw []byte) {
	// Parse config without holding any lock (fixes nested-lock hazard).
	nextCheckinAuto := true
	nextLifecycleAuto := true
	nextSchedulerMode := schedulerModeOff // reset to default on reconfigure
	nextKeepaliveAuto := true
	nextTasksAuto := true
	nextMgmtKey := ""
	nextLoginPlatform := "CLI"
	nextLoginRegion := regionCN

	nextPinned := map[string][]string{}
	var freePromoCfgEntries map[string]time.Time // free_promos override (model→end)
	cfgURL, cfgKey := "", ""
	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err == nil {
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
				if strings.HasPrefix(line, "login_platform:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "login_platform:"))
					v = strings.Trim(v, "\"'")
					if strings.EqualFold(v, "ide") {
						nextLoginPlatform = "ide"
					}
				}
				if strings.HasPrefix(line, "login_region:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "login_region:"))
					v = strings.Trim(v, "\"'")
					if strings.EqualFold(v, "intl") {
						nextLoginRegion = regionIntl
					} else if strings.EqualFold(v, "global") {
						nextLoginRegion = regionGlobal
					}
				}
				if strings.HasPrefix(line, "token_keepalive:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "token_keepalive:"))
					v = strings.Trim(v, "\"'")
					nextKeepaliveAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "tasks_auto:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "tasks_auto:"))
					nextTasksAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "models_cn:") {
					if ids := parsePinnedModelList(strings.TrimPrefix(line, "models_cn:")); len(ids) > 0 {
						nextPinned["cn"] = ids
					}
				}
				if strings.HasPrefix(line, "models_global:") {
					if ids := parsePinnedModelList(strings.TrimPrefix(line, "models_global:")); len(ids) > 0 {
						nextPinned["global"] = ids
					}
				}
				if strings.HasPrefix(line, "models_intl:") {
					if ids := parsePinnedModelList(strings.TrimPrefix(line, "models_intl:")); len(ids) > 0 {
						nextPinned["intl"] = ids
					}
				}
				if strings.HasPrefix(line, "free_promos:") {
					if m, ok := parseFreePromosValue(strings.TrimPrefix(line, "free_promos:")); ok {
						freePromoCfgEntries = m
					}
				}
			}
		}
	}

	// Apply each setting under its own lock — no nesting.
	checkinAutoMu.Lock()
	checkinAuto = nextCheckinAuto
	checkinAutoMu.Unlock()

	tasksAutoMu.Lock()
	tasksAuto = nextTasksAuto
	tasksAutoMu.Unlock()

	loginPlatformMu.Lock()
	loginPlatform = nextLoginPlatform
	loginPlatformMu.Unlock()

	loginPlatformMu.Lock()
	loginPlatform = nextLoginPlatform
	loginPlatformMu.Unlock()

	loginRegionMu.Lock()
	loginRegion = nextLoginRegion
	loginRegionMu.Unlock()

	lifecycleAutoMu.Lock()
	lifecycleAuto = nextLifecycleAuto
	lifecycleAutoMu.Unlock()

	schedulerModeMu.Lock()
	schedulerMode = nextSchedulerMode
	schedulerModeMu.Unlock()

	keepaliveAutoMu.Lock()
	keepaliveAuto = nextKeepaliveAuto
	keepaliveAutoMu.Unlock()

	pinnedModelsMu.Lock()
	pinnedModels = nextPinned
	pinnedModelsMu.Unlock()

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
	// Apply free_promos override on top of the seeded defaults. The cfg value
	// sets/overrides per-model window ends; anything not listed keeps its seed
	// (so deepseek-v4-flash stays free even if not re-listed). A reconfigure
	// without the field leaves the previous cfg merge, and empty cfgEntries
	// means the pure seeded slate.
	slate := make(map[string]time.Time, len(defaultFreePromos)+len(freePromoCfgEntries))
	for k, v := range defaultFreePromos {
		slate[k] = v
	}
	for k, v := range freePromoCfgEntries {
		if v.IsZero() {
			delete(slate, k)
			continue
		}
		slate[k] = v
	}
	freePromos.Lock()
	freePromos.m = slate
	freePromos.Unlock()
	// Migrate legacy codebuddy-cn auth files (merged plugin, v0.9.0).
	startAdoption()
}

// parseFreePromosValue converts the free_promos config value (e.g.
// "deepseek-v4.1-flash=2026-09-25,hy4-preview=2026-09-25" or bare ids) into a
// map of model id → end time (zero = unbounded). Invalid entries are skipped.
func parseFreePromosValue(v string) (map[string]time.Time, bool) {
	out := map[string]time.Time{}
	v = strings.TrimSpace(v)
	v = strings.Trim(v, "\"'")
	if v == "" {
		return nil, false
	}
	// Accept "model=date", "model", comma-separated.
	for _, tok := range strings.Split(v, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		model, dateStr, hasDate := strings.Cut(tok, "=")
		model = strings.TrimSpace(model)
		model = strings.Trim(model, "\"'")
		if model == "" {
			continue
		}
		if !hasDate {
			out[model] = time.Time{}
			continue
		}
		dateStr = strings.TrimSpace(dateStr)
		dateStr = strings.Trim(dateStr, "\"'")
		d, err := time.Parse("2006-01-02", dateStr)
		if err != nil {
			log.Printf("free_promos: skipping %q: bad date %q: %v", model, dateStr, err)
			continue
		}
		// Seed at end-of-day 23:59:59.999 UTC so the DAY ITSELF is free.
		out[model] = d.Add(24*time.Hour - time.Nanosecond)
	}
	return out, true
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

// currentLoginPlatform returns the configured platform for NEW logins.
func currentLoginPlatform() string {
	loginPlatformMu.RLock()
	defer loginPlatformMu.RUnlock()
	if p := strings.TrimSpace(loginPlatform); p != "" {
		return p
	}
	return "CLI"
}

// loadedLoginRegion returns the configured realm for NEW logins.
func loadedLoginRegion() string {
	loginRegionMu.RLock()
	defer loginRegionMu.RUnlock()
	return loginRegion
}

// platformForAuth returns the login platform recorded for an existing
// account. Legacy files without the field default to CLI-style headers.
func platformForAuth(sa *storedAuth) string {
	if sa != nil {
		if p := strings.TrimSpace(sa.Auth.LoginPlatform); p != "" {
			return p
		}
	}
	return "CLI"
}

// applyPlatformHeaders sets the CodeBuddy IDE client headers for ide-
// platform tokens (parity with the former codebuddy-cn plugin). CLI
// tokens keep the historical workbuddy header set (no X-IDE-*).
func applyPlatformHeaders(req *http.Request, platform string) {
	if strings.EqualFold(strings.TrimSpace(platform), "ide") {
		req.Header.Set("X-IDE-Type", "CodeBuddyIDE")
		req.Header.Set("X-IDE-Name", "CodeBuddyIDE")
		req.Header.Set("X-IDE-Version", "4.9.7")
		req.Header.Set("X-Product-Version", "4.9.7")
	}
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
