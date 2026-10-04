// usage_config.go decodes plugin config from config_yaml on every
// register/reconfigure call and resolves the CPAMP usage report URL/key.
// All plugin-level config lives here so the rest of the plugin reads
// consistent, lock-protected snapshots.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// plugin-level config decoded from plugin.register/reconfigure config_yaml.
var (
	// loginProvider: upstream for NEW logins (zai default). Existing accounts
	// keep their own provider field.
	loginProvider   = providerZai
	loginProviderMu sync.RWMutex

	// identity header values (see identity.go).
	identityMu          sync.RWMutex
	identityAppVersion  = ""
	identitySourceTitle = ""
	identityReferer     = ""
	identityLanguage    = ""
	identityTimezone    = ""

	// usageReportURL / usageReportKey: POST NDJSON to CPA-Manager-Plus
	// /v0/management/usage/import (only path that reaches request monitoring;
	// c-shared plugins cannot use host usage.DefaultManager/redisqueue).
	usageReportURL = ""
	usageReportKey = ""
	usageReportMu  sync.RWMutex

	// managementAPIKey: plugin-layer auth for /v0/management/plugins/zcode/*
	// write endpoints. When empty, plugin relies on host-side auth.
	managementAPIKey   = ""
	managementAPIKeyMu sync.RWMutex
)

const defaultUsageReportURL = "http://127.0.0.1:18317/v0/management/usage/import"
const fallbackUsageReportURL = "http://cpa-manager-plus:18317/v0/management/usage/import"

func loadedLoginProvider() string {
	loginProviderMu.RLock()
	defer loginProviderMu.RUnlock()
	return loginProvider
}

// configure decodes plugin config from the lifecycle request.
func configure(raw []byte) {
	nextLoginProvider := "" // sticky: empty = keep current (issue #24)
	nextLifecycleAuto := true
	nextMgmtKey := ""
	cfgURL, cfgKey := "", ""
	cfgVersion, cfgTitle, cfgReferer, cfgLang, cfgTz := "", "", "", "", ""
	nextOffPeak := false
	nextOffPeakMaxWait := time.Duration(0)
	nextClaimAuto := true
	nextClaimPlanID := ""
	nextClaimPoll, nextClaimCooldown := 0, 0

	if len(raw) > 0 {
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err == nil {
			// v0.1.9 (issue #24): decode the payload as real YAML first --
			// yaml.v3 rides in via the SDK, and this sees flow-style
			// one-liners ({k: v}) and JSON that the line-scan below can
			// never match. Block style parses identically in both paths.
			if m, ok := decodePluginConfigYAML(req.ConfigYAML); ok {
				if v, present := m["login_provider"]; present {
					nextLoginProvider = normalizeProvider(configScalarString(v))
				}
				if v, present := m["lifecycle_auto"]; present {
					nextLifecycleAuto = configScalarBool(v)
				}
				if v, present := m["offpeak"]; present {
					nextOffPeak = configScalarBool(v)
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
				if v, present := m["claim_auto"]; present {
					nextClaimAuto = configScalarBool(v)
				}
				if v, present := m["claim_plan_id"]; present {
					nextClaimPlanID = configScalarString(v)
				}
				if v, present := m["claim_poll_seconds"]; present {
					if secs, perr := strconv.ParseInt(configScalarString(v), 10, 64); perr == nil && secs > 0 {
						nextClaimPoll = int(secs)
					}
				}
				if v, present := m["claim_cooldown_seconds"]; present {
					if secs, perr := strconv.ParseInt(configScalarString(v), 10, 64); perr == nil && secs > 0 {
						nextClaimCooldown = int(secs)
					}
				}
				if v, present := m["offpeak_max_wait"]; present {
					if secs, perr := strconv.ParseInt(configScalarString(v), 10, 64); perr == nil && secs > 0 {
						nextOffPeakMaxWait = time.Duration(secs) * time.Second
					}
				}
			}
			for _, line := range strings.Split(string(req.ConfigYAML), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "login_provider:") {
					v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "login_provider:")), "\"'")
					nextLoginProvider = normalizeProvider(v)
				}
				if strings.HasPrefix(line, "lifecycle_auto:") {
					v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "lifecycle_auto:")), "\"'")
					nextLifecycleAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "offpeak:") {
					v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "offpeak:")), "\"'")
					nextOffPeak = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "offpeak_max_wait:") {
					v := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "offpeak_max_wait:")), "\"'")
					if secs, perr := strconv.ParseInt(v, 10, 64); perr == nil && secs > 0 {
						nextOffPeakMaxWait = time.Duration(secs) * time.Second
					}
				}
				if strings.HasPrefix(line, "usage_report_url:") {
					cfgURL = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "usage_report_url:")), "\"'")
				}
				if strings.HasPrefix(line, "usage_report_key:") {
					cfgKey = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "usage_report_key:")), "\"'")
				}
				if strings.HasPrefix(line, "management_key:") {
					nextMgmtKey = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "management_key:")), "\"'")
				}
				if strings.HasPrefix(line, "claim_auto:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "claim_auto:"))
					nextClaimAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "claim_plan_id:") {
					nextClaimPlanID = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "claim_plan_id:")), "\"'")
				}
				if strings.HasPrefix(line, "claim_poll_seconds:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "claim_poll_seconds:"))
					if secs, perr := strconv.ParseInt(v, 10, 64); perr == nil && secs > 0 {
						nextClaimPoll = int(secs)
					}
				}
				if strings.HasPrefix(line, "claim_cooldown_seconds:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "claim_cooldown_seconds:"))
					if secs, perr := strconv.ParseInt(v, 10, 64); perr == nil && secs > 0 {
						nextClaimCooldown = int(secs)
					}
				}
				if strings.HasPrefix(line, "identity_version:") {
					cfgVersion = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "identity_version:")), "\"'")
				}
				if strings.HasPrefix(line, "identity_source_title:") {
					cfgTitle = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "identity_source_title:")), "\"'")
				}
				if strings.HasPrefix(line, "identity_referer:") {
					cfgReferer = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "identity_referer:")), "\"'")
				}
				if strings.HasPrefix(line, "identity_language:") {
					cfgLang = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "identity_language:")), "\"'")
				}
				if strings.HasPrefix(line, "identity_timezone:") {
					cfgTz = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "identity_timezone:")), "\"'")
				}
			}
		}
	}

	// Sticky (issue #24): only an explicit login_provider key moves the
	// pointer; bare/foreign reconfigures keep whatever was configured.
	if nextLoginProvider != "" {
		loginProviderMu.Lock()
		loginProvider = nextLoginProvider
		loginProviderMu.Unlock()
	}

	lifecycleAutoMu.Lock()
	lifecycleAuto = nextLifecycleAuto
	lifecycleAutoMu.Unlock()

	identityMu.Lock()
	identityAppVersion = cfgVersion
	identitySourceTitle = cfgTitle
	identityReferer = cfgReferer
	identityLanguage = cfgLang
	identityTimezone = cfgTz
	identityMu.Unlock()

	configureOffPeak(nextOffPeak, nextOffPeakMaxWait)

	// management key: config_yaml > env > keep existing.
	if nextMgmtKey == "" {
		nextMgmtKey = strings.TrimSpace(os.Getenv("ZCODE_MANAGEMENT_KEY"))
	}
	managementAPIKeyMu.Lock()
	managementAPIKey = nextMgmtKey
	managementAPIKeyMu.Unlock()

	claimConfigMu.Lock()
	claimAuto = nextClaimAuto
	claimPlanID = strings.TrimSpace(nextClaimPlanID)
	if nextClaimPoll > 0 {
		claimPollSeconds = nextClaimPoll
	}
	if nextClaimCooldown > 0 {
		claimCooldownSecs = nextClaimCooldown
	}
	claimConfigMu.Unlock()
	startClaimSchedulerOnce()

	resolveUsageReport(cfgURL, cfgKey)
}

// resolveUsageReport fills usageReportURL/key from config → env → secret files.
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
		readSecretFile("/run/secrets/cpamp_admin_key"),
		readSecretFile("/run/secrets/cpamp-admin-key"),
	)
	// Without an admin key every report is a guaranteed 401, and even the
	// unauthenticated reachability probe alone trips CPA's management
	// anti-brute-force ban — disable reporting entirely unless a key is set.
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

// probeUsageReportURL tries localhost first, then the compose service name.
// 401/403 responses disqualify a candidate.
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
