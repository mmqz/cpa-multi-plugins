// authfile.go owns every physical auth-file path the plugin touches: the
// zcode-<provider>-<uid>.json naming rule, UID sanitization (path-traversal
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

// authProviderFor returns the account's provider (zai default for legacy).
func authProviderFor(sa *storedAuth) string {
	if sa == nil {
		return providerZai
	}
	return normalizeProvider(sa.Auth.Provider)
}

// authPlanFor returns the account's plan (coding-plan default).
func authPlanFor(sa *storedAuth) string {
	if sa == nil || strings.TrimSpace(sa.Auth.Plan) == "" {
		return planCoding
	}
	return normalizePlan(sa.Auth.Plan)
}

// authFileNameFor matches toAuthData naming: zcode-<provider>-<uid>.json when
// UID is known; bare "zcode.json" is legacy single-account only.
func authFileNameFor(sa *storedAuth) string {
	if sa != nil {
		if uid := sanitizeUIDForFileName(sa.Account.UID); uid != "" {
			return "zcode-" + authProviderFor(sa) + "-" + uid + ".json"
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
		"logo":     pluginLogoURL,
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
