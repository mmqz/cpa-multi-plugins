// authfile.go owns every physical auth-file path the plugin touches: the
// mimo-<lane>-<uid>.json naming rule, UID sanitization (path-traversal
// defense), and the write helpers that talk to the host's auth store via
// host.auth.* RPC.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var unsafeUIDChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// sanitizeUIDForFileName strips path-traversal material from a UID.
func sanitizeUIDForFileName(uid string) string {
	uid = strings.TrimSpace(uid)
	uid = unsafeUIDChars.ReplaceAllString(uid, "_")
	if uid == "" || uid == "." || uid == ".." {
		return ""
	}
	if len(uid) > 64 {
		uid = uid[:64]
	}
	return uid
}

// authLaneFor returns the credential's lane (key default — sk is the primary
// lane by design; see docs/MIMO_AUTH.md §5).
func authLaneFor(sa *storedAuth) string {
	if sa == nil || strings.TrimSpace(sa.Auth.Lane) == "" {
		return laneKey
	}
	if sa.Auth.Lane == laneCookie {
		return laneCookie
	}
	return laneKey
}

// authFileNameFor matches toAuthData naming: mimo-<lane>-<uid>.json when UID
// is known; bare "mimo.json" is the legacy single-account shape.
func authFileNameFor(sa *storedAuth) string {
	if sa != nil {
		if uid := sanitizeUIDForFileName(sa.Account.UID); uid != "" {
			return "mimo-" + authLaneFor(sa) + "-" + uid + ".json"
		}
	}
	return authFileName
}

// resolveAuthFileTarget picks the canonical file name for save. The host's
// physical name wins when it already carries our family prefix.
func resolveAuthFileTarget(sa *storedAuth, phys *hostAuthPhysical) (name, path string) {
	name = authFileNameFor(sa)
	if phys != nil {
		path = strings.TrimSpace(phys.Path)
		physName := strings.TrimSpace(phys.Name)
		if physName != "" && isOurFamilyFileName(physName) {
			name = physName
		}
	}
	return name, path
}

type hostAuthPhysical struct {
	AuthIndex string
	Name      string
	Path      string
	JSON      []byte
	Disabled  bool
}

// hostAuthGetPhysicalFn / hostAuthPersistFn are indirection points so tests
// can exercise note-writing paths without a live host RPC bridge.
var (
	hostAuthGetPhysicalFn = hostAuthGetPhysical
	hostAuthPersistFn     = hostAuthPersist
)

func hostAuthGetPhysical(authIndex string) (*hostAuthPhysical, error) {
	body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGet, body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.get: bad envelope")
	}
	var resp rpcHostAuthGetResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return &hostAuthPhysical{
		AuthIndex: resp.AuthIndex,
		Name:      resp.Name,
		Path:      resp.Path,
		JSON:      resp.JSON,
		Disabled:  parseDisabledFromAuthJSON(resp.JSON),
	}, nil
}

// hostAuthPersist saves credential JSON via host.auth.save.
func hostAuthPersist(name string, raw []byte) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty auth file name")
	}
	saveReq := pluginapi.HostAuthSaveRequest{
		Name: name,
		JSON: raw,
	}
	saveBody, _ := json.Marshal(saveReq)
	rawResp, err := hostCall(pluginabi.MethodHostAuthSave, saveBody)
	if err != nil {
		return fmt.Errorf("host.auth.save: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(rawResp, &env); err != nil || !env.OK {
		msg := "host.auth.save failed"
		if env.Error != nil && env.Error.Message != "" {
			msg = truncateRedacted(env.Error.Message, 200)
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// buildAuthFileJSON produces the host-save payload: nested storage + top-level
// metadata. extra merges additional top-level keys (optional).
func buildAuthFileJSON(sa *storedAuth, disabled bool, note string, extra map[string]any) ([]byte, error) {
	if sa == nil {
		return nil, fmt.Errorf("nil storedAuth")
	}
	storage, err := json.Marshal(sa)
	if err != nil {
		return nil, err
	}
	var nested map[string]any
	if err := json.Unmarshal(storage, &nested); err != nil {
		return nil, err
	}
	out := map[string]any{
		"type":     providerName,
		"provider": providerName,
		"disabled": disabled,
		"note":     note,
		"auth":     nested["auth"],
		"account":  nested["account"],
	}
	for k, v := range extra {
		out[k] = v
	}
	return json.Marshal(out)
}

// parseDisabledFromAuthJSON reads top-level disabled from physical auth JSON.
func parseDisabledFromAuthJSON(raw []byte) bool {
	var m struct {
		Disabled bool `json:"disabled"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Disabled
}

// -----------------------------------------------------------------------------
// Stored credential
// -----------------------------------------------------------------------------

// storedAuth is the on-disk shape of a mimo credential.
type storedAuth struct {
	Auth    mimoTokens  `json:"auth"`
	Account mimoAccount `json:"account"`
}

// mimoTokens holds one credential of either lane.
//
// key lane: SK is the platform-issued `sk` from the OAuth blob (permanent,
// like the CLI's auth.json api key — no refresh); BaseURL is the OAuth `url`
// field (the CLI stores it as metadata.base_url and never calls api
// .xiaomimimo.com unless the platform issued it).
//
// cookie lane: Cookies is the adopted jar — the partition's account-domain
// bootstrap rows (passToken/userId/cUserId/uLocale) plus the M2-minted
// service rows (serviceToken, <sid>_ph, <sid>_slh) and, on legacy builds,
// any *.xiaomimimo.com session rows. Region/SID pin the lane's base and the
// passport sid the service ticket is bound to.
type mimoTokens struct {
	Lane        string       `json:"lane"`                  // key | cookie
	SK          string       `json:"sk,omitempty"`          // key lane chat credential
	BaseURL     string       `json:"baseURL,omitempty"`     // key lane OAuth-issued base ("" → api.xiaomimimo.com/v1)
	UID         string       `json:"uid,omitempty"`         // platform uid (both lanes when known)
	Region      string       `json:"region,omitempty"`      // cookie lane region (cn/sgp/ru/in; "" → config)
	SID         string       `json:"sid,omitempty"`         // cookie lane: passport sid the minted ticket is bound to (mimosgp/mimopc)
	Cookies     []mimoCookie `json:"cookies,omitempty"`     // cookie lane jar (bootstrap + minted)
	Source      string       `json:"source,omitempty"`      // cookie lane: adopt provenance (for diagnostics)
	AdoptedAt   int64        `json:"adoptedAt,omitempty"`   // cookie lane: unix seconds
	ExchangedAt int64        `json:"exchangedAt,omitempty"` // cookie lane: last successful serviceLogin→STS mint (unix seconds)
}

// mimoCookie is one adopted Chromium cookie row (persisted in the host auth
// store, which is the credential store by contract).
type mimoCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"httpOnly,omitempty"`
	Expires  int64  `json:"expires,omitempty"` // unix seconds; 0 = session cookie
}

type mimoAccount struct {
	UID         string `json:"uid"`
	DisplayName string `json:"displayName,omitempty"`
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	// Accept both shapes seen in the wild:
	//   nested: {"auth":{...},"account":{...}} (plugin/oauth/adopt output)
	//   flat:   {"lane":...,"sk":...,"uid":...} (manager-written files)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var sa storedAuth
	if _, nested := probe["auth"]; nested {
		if err := json.Unmarshal(raw, &sa); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
	} else {
		var flat struct {
			Lane        string       `json:"lane"`
			SK          string       `json:"sk"`
			BaseURL     string       `json:"baseURL"`
			UID         string       `json:"uid"`
			Region      string       `json:"region"`
			SID         string       `json:"sid"`
			Cookies     []mimoCookie `json:"cookies"`
			Source      string       `json:"source"`
			AdoptedAt   int64        `json:"adoptedAt"`
			ExchangedAt int64        `json:"exchangedAt"`
			UID2        string       `json:"accountUid"`
			Display     string       `json:"displayName"`
		}
		if err := json.Unmarshal(raw, &flat); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		sa.Auth = mimoTokens{
			Lane: flat.Lane, SK: flat.SK, BaseURL: flat.BaseURL, UID: flat.UID,
			Region: flat.Region, SID: flat.SID, Cookies: flat.Cookies, Source: flat.Source,
			AdoptedAt: flat.AdoptedAt, ExchangedAt: flat.ExchangedAt,
		}
		sa.Account = mimoAccount{UID: firstNonEmpty(flat.UID, flat.UID2), DisplayName: flat.Display}
	}
	switch authLaneFor(&sa) {
	case laneKey:
		if strings.TrimSpace(sa.Auth.SK) == "" {
			return nil, fmt.Errorf("parse_error: missing sk")
		}
	case laneCookie:
		if len(sa.Auth.Cookies) == 0 {
			return nil, fmt.Errorf("parse_error: missing cookies")
		}
	}
	return &sa, nil
}

// -----------------------------------------------------------------------------
// Auth handlers (parse / label / note)
// -----------------------------------------------------------------------------

// isOurFamilyFileName reports whether a type-less auth file belongs to this
// plugin by name: the canonical mimo- prefix. The filename is the only
// trustworthy discriminator for files without an explicit type (see the
// ownership note in handleParseAuth).
func isOurFamilyFileName(name string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), providerName+"-")
}

// isOurDeclaredType reports whether an explicitly declared auth "type"
// belongs to this plugin.
func isOurDeclaredType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "mimo", "mimo-key", "mimo-cookie", "xiaomi":
		return true
	}
	return false
}

func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// Ownership check (CPA native contract): the host routes by the file's
	// top-level "type" field. Files without a type fall back to polling every
	// plugin — first Handled=true wins. Only claim files whose declared type
	// matches us, or whose filename carries our prefix.
	var probeType struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(req.RawJSON, &probeType)
	declared := strings.ToLower(strings.TrimSpace(probeType.Type))
	if declared != "" && !isOurDeclaredType(declared) {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if declared == "" {
		// No type declared: claim ONLY when the filename carries our family.
		// req.Provider cannot prove ownership here — the host's callParseAuths
		// rewrites an empty Provider to the POLLED plugin's own identifier.
		if !isOurFamilyFileName(req.FileName) {
			return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
		}
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	// CRITICAL: echo back the host-provided FileName AND leave ID empty —
	// CPA falls back to authIDForPath(path) which derives ID from the file
	// path and always matches the watcher's key (prevents duplicate records).
	ad := toAuthData(sa, parseDisabledFromAuthJSON(req.RawJSON))
	ad.ID = ""
	if fn := strings.TrimSpace(req.FileName); fn != "" {
		ad.FileName = fn
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    ad,
	})
}

func toAuthData(sa *storedAuth, disabled bool) pluginapi.AuthData {
	storage, _ := json.Marshal(sa)
	id := providerName
	fileName := authFileName
	if sa != nil {
		if uid := sanitizeUIDForFileName(sa.Account.UID); uid != "" {
			id = uid
			fileName = authFileNameFor(sa)
		}
	}
	return pluginapi.AuthData{
		Provider:    providerName,
		ID:          id,
		FileName:    fileName,
		Label:       labelForAuth(sa),
		Disabled:    disabled,
		StorageJSON: storage,
		Metadata: map[string]any{
			"lane":   authLaneFor(sa),
			"region": strings.TrimSpace(sa.Auth.Region),
			"uid":    strings.TrimSpace(sa.Account.UID),
		},
	}
}

// labelForAuth tags the host label with the lane.
func labelForAuth(sa *storedAuth) string {
	base := "MiMo"
	lane := "KEY"
	if sa != nil {
		if strings.TrimSpace(sa.Account.DisplayName) != "" {
			base = strings.TrimSpace(sa.Account.DisplayName)
		}
		if authLaneFor(sa) == laneCookie {
			lane = "COOKIE"
		}
		if r := strings.TrimSpace(sa.Auth.Region); r != "" {
			lane += "/" + strings.ToUpper(r)
		}
	}
	return base + " [" + lane + "]"
}
