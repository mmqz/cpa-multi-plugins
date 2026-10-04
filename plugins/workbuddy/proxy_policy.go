// proxy_policy.go carries the host-configured upstream proxy into the plugin's
// direct HTTP transports. The billing path bypasses the host http bridge
// (v0.9.49 — the bridge's default transport negotiates HTTP/2, which the
// codebuddy.ai APISIX gateway drops mid-request), and that bypass also dropped
// every host transport policy — including the proxy configured in CPA's
// config.yaml (`proxy-url`). Field timeline (2026-10-04): credits display
// worked while billing rode the bridge (proxy applied), broke with
// `Post ".../get-user-resource": EOF` ever since the direct bypass — the
// deployment's proxy lives in config.yaml, not in process env, so the
// v0.9.51 env-var support (HTTPS_PROXY) never saw it.
//
// The host delivers its config to the plugin inside every AuthParseRequest
// (startup / import) and AuthModelRequest / StaticModelRequest (model
// discovery) as HostConfigSummary.ProxyURL. We cache it the moment it arrives
// and honor it in the billing transports — restoring the pre-bypass behavior
// while keeping the h1.1 direct path that fixed the HTTP/2 EOFs.
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
		log.Printf("workbuddy: ignoring invalid host proxy URL %q: %v", redactSecrets(proxyURL), err)
		return
	}
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	if hostProxyURL == proxyURL {
		return
	}
	hostProxyURL = proxyURL
	log.Printf("workbuddy: billing direct path will use host-configured proxy (from CPA config proxy-url)")
}

// currentHostProxyURL returns the cached host-configured proxy URL ("" when
// none was delivered yet).
func currentHostProxyURL() string {
	hostProxyMu.RLock()
	defer hostProxyMu.RUnlock()
	return hostProxyURL
}

// billingProxyFunc is the Proxy policy for the billing transports: an explicit
// host-configured proxy (config.yaml proxy-url) wins; otherwise fall back to
// the standard environment variables (HTTPS_PROXY / HTTP_PROXY / NO_PROXY).
// With neither configured this returns nil — plain direct connection, exactly
// the historical default.
func billingProxyFunc(req *http.Request) (*url.URL, error) {
	if u := currentHostProxyURL(); u != "" {
		return url.Parse(u)
	}
	return http.ProxyFromEnvironment(req)
}
