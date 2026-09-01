// region.go — per-account region routing for the merged qoder plugin.
//
// History: qoder-cn (openapi.qoder.com.cn) and qoder-intl (openapi.qoder.sh)
// were separate plugins whose logic differed only in host / client-id
// constants. Merged in v0.10.0 into this single plugin: each auth file
// carries its region explicitly (auth.region), with the stored domain as a
// legacy fallback. New logins target the region configured via login_region.
package main

import (
	"strings"
	"sync"
)

const (
	regionCN   = "cn"
	regionIntl = "intl"

	domainCN   = "qoder.com.cn"
	domainIntl = "qoder.com"
)

var (
	loginRegionMu sync.RWMutex
	loginRegion   = regionCN // region for NEW logins (config login_region)
)

// normalizeRegion maps any stored region hint onto cn/intl (default cn).
func normalizeRegion(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case regionIntl, "global":
		return regionIntl
	default:
		return regionCN
	}
}

// authRegion resolves the region of one stored account:
//  1. explicit auth.region (written by login + adoption),
//  2. legacy fallback: domain sniff (qoder.sh / qoder.com without .cn → intl),
//  3. default cn.
func authRegion(sa *storedAuth) string {
	if sa != nil {
		if r := normalizeRegion(sa.Auth.Region); r == regionIntl {
			return regionIntl
		}
		d := strings.ToLower(sa.Auth.Domain)
		if strings.Contains(d, "qoder.sh") ||
			(strings.Contains(d, "qoder.com") && !strings.Contains(d, "qoder.com.cn")) {
			return regionIntl
		}
	}
	return regionCN
}

// domainForRegion returns the realm string stored in auth.domain.
func domainForRegion(region string) string {
	if region == regionIntl {
		return domainIntl
	}
	return domainCN
}

// upstreamBaseForRegion / gatewayBaseForRegion route by login/account region.
func upstreamBaseForRegion(region string) string {
	if region == regionIntl {
		return upstreamBaseIntl
	}
	return upstreamBaseCN
}

func gatewayBaseForRegion(region string) string {
	if region == regionIntl {
		return gatewayBaseIntl
	}
	return gatewayBaseCN
}

// upstreamBaseFor / gatewayBaseFor route by the account's region.
func upstreamBaseFor(sa *storedAuth) string { return upstreamBaseForRegion(authRegion(sa)) }
func gatewayBaseFor(sa *storedAuth) string  { return gatewayBaseForRegion(authRegion(sa)) }

// Region-aware endpoint builders. The package-level endpoint* constants stay
// as the CN defaults; all request sites use the For/ForRegion variants.
func endpointJobTokenExchangeFor(sa *storedAuth) string {
	return upstreamBaseFor(sa) + "/api/v1/jobToken/exchange"
}

func endpointJobTokenRefreshFor(sa *storedAuth) string {
	return upstreamBaseFor(sa) + "/api/v1/jobToken/refresh"
}

func endpointUserInfoForRegion(region string) string {
	return upstreamBaseForRegion(region) + "/api/v1/userinfo"
}

func endpointProUpgradeFor(sa *storedAuth) string {
	return upstreamBaseFor(sa) + "/sash/api/v1/me/pro-upgrade/claim"
}

func endpointChatFor(sa *storedAuth) string {
	return gatewayBaseFor(sa) + "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
}

func endpointModelsFor(sa *storedAuth) string {
	return gatewayBaseFor(sa) + "/algo/api/v2/model/list?Encode=1"
}

// Login-region config accessors (parsed from login_region in configure()).
func loadedLoginRegion() string {
	loginRegionMu.RLock()
	defer loginRegionMu.RUnlock()
	return loginRegion
}

func setLoginRegion(r string) {
	loginRegionMu.Lock()
	loginRegion = r
	loginRegionMu.Unlock()
}

// qoderWebsiteFor / qoderClientIDFor / qoderRedirectURIFor select the
// device-authorization entry point per region (was hardcoded per plugin).
func qoderWebsiteFor(region string) string {
	if region == regionIntl {
		return qoderWebsiteIntl
	}
	return qoderWebsiteCN
}

func qoderClientIDFor(region string) string {
	if region == regionIntl {
		return qoderClientIDIntl
	}
	return qoderClientIDCN
}

func qoderRedirectURIFor(region string) string {
	if region == regionIntl {
		return qoderRedirectURIIntl
	}
	return qoderRedirectURICN
}
