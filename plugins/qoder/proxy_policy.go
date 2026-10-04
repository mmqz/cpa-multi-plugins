// proxy_policy.go carries the host-configured upstream proxy into the plugin's
// direct HTTP transports. The billing/campaign path bypasses the host http
// bridge (v0.8.51 — the bridge's default transport negotiates HTTP/2, which
// the qoder APISIX gateways drop mid-request), and that bypass also dropped
// every host transport policy — including the proxy configured in CPA's
// config.yaml (`proxy-url`). Deployments that need a proxy to reach
// openapi.qoder.sh (Intl) lost that coverage when the bypass landed.
//
// The host delivers its config to the plugin inside every AuthParseRequest
// (startup / import) and model discovery request as HostConfigSummary.ProxyURL.
// We cache it the moment it arrives and honor it in the shared direct
// transports — restoring host proxy parity while keeping the h1.1 direct path
// that fixed the HTTP/2 EOFs.
package main

import (
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

var (
	hostProxyMu  sync.RWMutex
	hostProxyURL string
)

// rememberHostProxy caches the host-configured upstream proxy URL delivered in
// HostConfigSummary. Empty values are ignored (hosts without proxy config);
// re-delivery with the same value is a no-op, a changed value updates the
// cache (config reload → next parse/model callback refreshes it).
func rememberHostProxy(proxyURL string) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return
	}
	if _, err := url.Parse(proxyURL); err != nil {
		log.Printf("qoder: ignoring invalid host proxy URL %q: %v", redactSecrets(proxyURL), err)
		return
	}
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	if hostProxyURL == proxyURL {
		return
	}
	hostProxyURL = proxyURL
	log.Printf("qoder: direct HTTP path will use host-configured proxy (from CPA config proxy-url)")
}

// currentHostProxyURL returns the cached host-configured proxy URL ("" when
// none was delivered yet).
func currentHostProxyURL() string {
	hostProxyMu.RLock()
	defer hostProxyMu.RUnlock()
	return hostProxyURL
}

// directProxyFunc is the Proxy policy for the direct transports: an explicit
// host-configured proxy (config.yaml proxy-url) wins; otherwise fall back to
// the standard environment variables (HTTPS_PROXY / HTTP_PROXY / NO_PROXY).
// With neither configured this returns nil — plain direct connection, exactly
// the historical default.
func directProxyFunc(req *http.Request) (*url.URL, error) {
	if u := currentHostProxyURL(); u != "" {
		return url.Parse(u)
	}
	return http.ProxyFromEnvironment(req)
}
