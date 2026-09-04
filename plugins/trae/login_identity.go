// login_identity.go — v0.12.25: stable per-account credential-file identity.
//
// Bug report this fixes: the same account logged in twice showed as multiple
// panel accounts ("凭证 init 的账户同一个显示多个，因为我交了两次"). Root cause
// chain (code-traced, cockpit-tools cross-checked):
//
//  1. The upstream callback URL carries a userInfo parameter with the
//     account's real identity (TraeCallbackPayload.userInfo — cockpit-tools
//     falls back to it whenever GetUserInfo fails: trae_oauth.rs:2707-2734).
//     This plugin ignored it.
//  2. GetUserInfo (cloudide/api/v3/trae/GetUserInfo) can fail (network/401)
//     or answer 200 with an unexpected envelope → empty uid, and the poll
//     path (handlePollLogin) proceeded "with empty UID" → the nameless
//     trae-.json; the self-complete path (v0.12.17-0.12.24) fell back to the
//     PER-LOGIN loginTraceID → trae-<uuid>.json, a fresh file name for every
//     submission of the SAME account — the reported duplicates.
//
// Fix: parse the callback's userInfo echo and resolve the file identity as
// GetUserInfo → callback userInfo → a STABLE per-realm "unknown" name. The
// unknown fallback deliberately collides (one file per realm, overwritten on
// re-login) instead of minting unique garbage per submission; a re-login
// always repairs it, and both real sources make the case vanishingly rare.
package main

import (
	"encoding/json"
	"net/url"
	"strings"
)

// cbIdentity is the account identity echoed by the upstream callback URL.
type cbIdentity struct {
	UID      string
	Nickname string
}

// callbackIdentityKeys lists the query-parameter spellings of the userInfo
// echo (cockpit-tools reads "userInfo"; tolerate the obvious variants).
var callbackIdentityKeys = []string{"userInfo", "user_info", "UserInfo", "userinfo"}

// pickString returns the first non-empty string among the JSON keys tried in
// order (mirrors cockpit-tools pick_string multi-path lookups).
func pickString(m map[string]any, keys []string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// parseCallbackIdentity extracts the account identity from the callback's
// userInfo parameter. The value is a JSON object (possibly JSON-quoted once
// more by nested encoding — decoded recursively, depth-capped). Unknown
// shapes are ignored: identity here is best-effort by design, the fresh
// GetUserInfo result always wins.
func parseCallbackIdentity(vals url.Values) cbIdentity {
	for _, k := range callbackIdentityKeys {
		raw := strings.TrimSpace(vals.Get(k))
		if raw == "" {
			continue
		}
		if id := identityFromJSON(raw, 0); id.UID != "" || id.Nickname != "" {
			return id
		}
	}
	return cbIdentity{}
}

func identityFromJSON(raw string, depth int) cbIdentity {
	if depth > 2 || raw == "" {
		return cbIdentity{}
	}
	trimmed := strings.TrimSpace(raw)
	// Strip one layer of JSON string quoting ("{\"UserID\":...}") if present.
	if strings.HasPrefix(trimmed, "\"") {
		var inner string
		if err := json.Unmarshal([]byte(trimmed), &inner); err == nil && inner != "" {
			return identityFromJSON(inner, depth+1)
		}
		return cbIdentity{}
	}
	if !strings.HasPrefix(trimmed, "{") {
		return cbIdentity{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return cbIdentity{}
	}
	uid := pickString(m, []string{"UserID", "userId", "uid", "UID", "user_id"})
	nickname := pickString(m, []string{"ScreenName", "screenName", "Nickname", "nickname", "Name", "name", "displayName"})
	return cbIdentity{UID: uid, Nickname: nickname}
}

// unknownUIDFallback returns the stable last-resort file identity for a realm.
// It is intentionally CONSTANT per realm: repeated logins of the same account
// overwrite the same file instead of minting trae-<uuid>.json duplicates.
func unknownUIDFallback(realm string) string {
	if realm == "intl" {
		return "unknown-intl"
	}
	return "unknown-cn"
}

// resolveLoginUID picks the credential-file identity for a completed login.
// Precedence: fresh GetUserInfo result → callback userInfo echo → stable
// per-realm unknown fallback. Never returns a per-login value.
func resolveLoginUID(getUserInfoUID, callbackUID, realm string) string {
	if s := strings.TrimSpace(getUserInfoUID); s != "" {
		return s
	}
	if s := strings.TrimSpace(callbackUID); s != "" {
		return s
	}
	return unknownUIDFallback(realm)
}

// resolveLoginNickname mirrors resolveLoginUID for the display name.
func resolveLoginNickname(getUserInfoNickname, callbackNickname string) string {
	if s := strings.TrimSpace(getUserInfoNickname); s != "" {
		return s
	}
	return strings.TrimSpace(callbackNickname)
}
