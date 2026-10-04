// split_build.go — split-channel flavor support (issue #29).
//
// The repo ships TWO release packages per platform from the SAME source:
//
//   - cpa-multi-plugins-<os>-<arch>.zip — the unified flavor (default):
//     one plugin per provider merging cn/intl/solo realms behind a single
//     identifier with per-account variant config.
//   - cpa-multi-plugins-split-<os>-<arch>.zip — the split-channel flavor:
//     the legacy per-variant plugins (trae-cn / trae-solo-cn / trae-intl,
//     plus the qoder/workbuddy equivalents), each registering as its own
//     panel entry with its own OAuth login pinned to its realm.
//
// A trae split build is produced purely at BUILD TIME — no source fork:
//
//	   go build -ldflags "-X main.providerName=trae-intl -X main.pluginVariantPin=intl"
//
//	- providerName (var since the split flavor) → the registered plugin id,
//	  credential namespace and panel entry.
//	- pluginVariantPin → the DEFAULT for the sticky login_variant config;
//	  an explicit login_variant in config.yaml still wins (issue #24 sticky
//	  semantics are untouched). Per-account variants keep their own realm
//	  routing — the pin only defaults NEW logins.
//
// Empty values (the unified flavor default) keep behavior byte-for-byte.
package main

// pluginVariantPin is the build-time realm pin ("cn" / "solo" / "intl");
// empty for the unified flavor. Injected via -ldflags -X. See the file
// comment.
var pluginVariantPin = ""

// providerName is a VAR (split flavor, issue #29): the unified build keeps
// the default "trae"; the split-channel builds rename it via
// -ldflags "-X main.providerName=trae-intl" so the plugin registers as its
// own panel entry / credential namespace. See the file comment.
var providerName = "trae"

func init() {
	applyVariantPin()
}

// applyVariantPin seeds the sticky login-variant default from the build-time
// pin. Called from init() (before any host configure()) and directly from
// tests. Explicit login_variant config still overrides later.
func applyVariantPin() {
	switch pluginVariantPin {
	case "":
		return
	case variantCN, variantSolo, variantIntl:
		loginVariantMu.Lock()
		loginVariant = pluginVariantPin
		loginVariantMu.Unlock()
	default:
		panic("trae: invalid pluginVariantPin " + pluginVariantPin)
	}
}
