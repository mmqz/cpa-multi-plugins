// host_auth.go wraps the host's auth-store RPC (host.auth.list / get).
// These are the only paths the plugin uses to read auth files; writes go
// through hostAuthPersist / hostAuthPersistMigrate in authfile.go.
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// rpcHostAuthListResponse mirrors the host's host.auth.list envelope result.
type rpcHostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type rpcHostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
}

// hostAuthList returns all zcode credentials known to the host. The
// "zcode-" prefix matches canonical zcode-<provider>-<uid>.json plus the
// legacy single file zcode.json.
func hostAuthList() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.list: bad envelope")
	}
	var resp rpcHostAuthListResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	// Fresh slice — resp.Files[:0] would alias the RPC response's backing
	// array. Filter by filename prefix, NOT by Type/Provider: many existing
	// auth files on disk don't carry a "type"/"provider" field, and
	// EqualFold("", providerName) returns false for them. Filename prefix is
	// the only reliable cross-version discriminator.
	out := make([]pluginapi.HostAuthFileEntry, 0, len(resp.Files))
	prefix := providerName + "-"
	for _, f := range resp.Files {
		if strings.HasPrefix(strings.ToLower(f.Name), prefix) {
			out = append(out, f)
		}
	}
	return out, nil
}

// hostAuthGet fetches the credential JSON for one auth index.
func hostAuthGet(authIndex string) (*storedAuth, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, err
	}
	return parseStored(phys.JSON)
}

// hostAuthGetBundle is one host.auth.get for both storage and physical
// metadata (avoids a double-RPC in dashboard paths).
func hostAuthGetBundle(authIndex string) (*storedAuth, *hostAuthPhysical, error) {
	phys, err := hostAuthGetPhysical(authIndex)
	if err != nil {
		return nil, nil, err
	}
	sa, err := parseStored(phys.JSON)
	if err != nil {
		return nil, phys, err
	}
	return sa, phys, nil
}

// hostAuthListFiles is the unfiltered list (models/adopt paths filter
// themselves).
func hostAuthListFiles() ([]pluginapi.HostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.list failed")
	}
	var resp rpcHostAuthListResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// hostAuthGetByIndex fetches the raw JSON for one auth index.
func hostAuthGetByIndex(authIndex string) ([]byte, error) {
	body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGet, body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.get failed")
	}
	var resp rpcHostAuthGetResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return resp.JSON, nil
}
