// split_build.go — split-channel flavor support (issue #29).
//
// The repo ships TWO release packages per platform from the SAME source:
//
//   - cpa-multi-plugins-<os>-<arch>.zip — the unified flavor (default):
//     one plugin per provider merging cn/intl/global realms behind a single
//     identifier with per-account realm config.
//   - cpa-multi-plugins-split-<os>-<arch>.zip — the split-channel flavor:
//     the legacy per-variant plugins (codebuddy-cn / codebuddy-intl, plus
//     the qoder/trae equivalents), each registering as its own panel entry
//     with its own OAuth login pinned to its realm.
//
// A workbuddy split build is produced purely at BUILD TIME — no source fork:
//
//		go build -ldflags "-X main.providerName=codebuddy-intl -X main.pluginVariantPin=intl"
//
//	  - providerName (var since the split flavor) → the registered plugin id,
//	    auth-file namespace and panel entry.
//	  - pluginVariantPin → the DEFAULT for the sticky login_region config;
//	    an explicit login_region in config.yaml still wins. Per-account realm
//	    routing (cn / global / intl) is untouched — the pin only defaults
//	    NEW logins.
//
// Empty values (the unified flavor default) keep behavior byte-for-byte.
package main

// pluginVariantPin is the build-time realm pin ("cn" / "intl"); empty for
// the unified flavor. Injected via -ldflags -X. See the file comment.
var pluginVariantPin = ""

// providerName is a VAR (split flavor, issue #29): the unified build keeps
// the default "workbuddy"; the split-channel builds rename it via
// -ldflags "-X main.providerName=codebuddy-intl". See the file comment.
var providerName = "workbuddy"

func init() {
	applyVariantPin()
}

// applyVariantPin seeds the sticky login-region default from the build-time
// pin. Called from init() (before any host configure()) and directly from
// tests. Explicit login_region config still overrides later.
func applyVariantPin() {
	switch pluginVariantPin {
	case "":
		return
	case "cn", "intl":
		loginRegionMu.Lock()
		loginRegion = pluginVariantPin
		loginRegionMu.Unlock()
	default:
		panic("workbuddy: invalid pluginVariantPin " + pluginVariantPin)
	}
}
