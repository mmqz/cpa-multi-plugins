// split_build.go — split-channel flavor support (issue #29).
//
// The repo ships TWO release packages per platform from the SAME source:
//
//   - cpa-multi-plugins-<os>-<arch>.zip — the unified flavor (default):
//     one plugin per provider merging cn/intl/solo realms behind a single
//     identifier with per-account region config.
//   - cpa-multi-plugins-split-<os>-<arch>.zip — the split-channel flavor:
//     the legacy per-variant plugins (codebuddy-cn / codebuddy-intl /
//     qoder-cn / qoder-intl / trae-cn / trae-solo-cn / trae-intl), each
//     registering as its own panel entry with its own OAuth login pinned
//     to its realm (issue #29's request).
//
// A split build is produced purely at BUILD TIME — no source fork:
//
//		go build -ldflags "-X main.providerName=qoder-intl -X main.pluginVariantPin=intl"
//
//	  - providerName (var since the split flavor) → the registered plugin id,
//	    auth-file namespace and panel entry.
//	  - pluginVariantPin → the DEFAULT for the sticky login_region config;
//	    an explicit login_region in config.yaml still wins (issue #24 sticky
//	    semantics are untouched).
//
// Empty values (the unified flavor default) keep behavior byte-for-byte.
package main

// pluginVariantPin is the build-time realm pin ("cn" / "intl"); empty for
// the unified flavor. Injected via -ldflags -X. See the file comment.
var pluginVariantPin = ""

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
	case regionCN, regionIntl:
		loginRegionMu.Lock()
		loginRegion = pluginVariantPin
		loginRegionMu.Unlock()
	default:
		panic("qoder: invalid pluginVariantPin " + pluginVariantPin)
	}
}
