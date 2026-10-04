// models_groups.go — v0.8.27: per-region model catalog endpoint for the
// panel's model-exclusion picker (GET /plugins/qoder/models/groups). Port of
// the workbuddy v0.9.40 design onto qoder's cn/intl region boundary.
//
// Why: the HOST's global oauth-excluded-models page cannot offer candidate
// lists for plugin channels (model-definitions/:channel is an upstream-static
// 8-channel catalog; unknown channels 400 "unknown channel"), so excluding a
// qoder model there meant hand-typing ids. This endpoint serves the catalog
// from the plugin's OWN knowledge — the per-credential model_cache snapshots
// (v0.8.27) — grouped by region, and the panel renders per-region checkboxes
// that write the host's qoder-<region> sub-keys via PATCH
// /v0/management/oauth-excluded-models. Consumption stays host-config-shaped
// (filterExcludedModelsForRegion), so the global page and this picker drive
// the SAME exclusion map — no parallel truth, nothing plugin-proprietary to
// persist.
//
// Read-only by default: groups are served from the newest model_cache
// snapshot per region (zero upstream calls). ?refresh=1 additionally runs one
// live discovery per region through fetchDynamicModelsFromStorage's full
// chain (cache → discover → peer → persisted), which also re-stamps the
// snapshot — the exact path model.for_auth uses, so what the picker shows and
// what the runtime advertises cannot diverge.
//
// The catalog (snapshot/discovery) is the PRE-exclusion full list, so
// excluded models remain visible and re-checkable — un-excluding never
// requires a lucky upstream round-trip.
package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// modelGroupRegionOrder is the fixed presentation order; a region with zero
// credentials still renders (empty group) so the picker shows every channel.
var modelGroupRegionOrder = []string{"cn", "intl"}

// modelGroupRegionLabel renders the human-facing region title. Keep in sync
// with the login_region enum description in main.go and the panel badges.
func modelGroupRegionLabel(region string) string {
	switch region {
	case "cn":
		return "CN（qoder.com.cn）"
	case "intl":
		return "Intl（qoder.com）"
	default:
		return region
	}
}

// modelGroupEntry is one catalog row. Name carries the display name when the
// snapshot/discovery provided one.
type modelGroupEntry struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// modelGroup is one region's catalog as the panel picker consumes it.
type modelGroup struct {
	Realm        string            `json:"realm"`
	Label        string            `json:"label"`
	Models       []modelGroupEntry `json:"models"`
	Count        int               `json:"count"`
	Source       string            `json:"source,omitempty"`     // snapshot | refreshed
	FetchedAt    string            `json:"fetched_at,omitempty"` // RFC3339 UTC
	NeedsRefresh bool              `json:"needs_refresh"`        // empty catalog → suggest the refresh button
	Credentials  int               `json:"credentials"`          // qoder credentials seen in this region
	Note         string            `json:"note,omitempty"`       // refresh failure reason when a refresh produced nothing
}

type modelGroupsResponse struct {
	Groups []modelGroup `json:"groups"`
	Hint   string       `json:"hint"`
}

// regionCatalogEntry is one credential's contribution to a region catalog scan.
type regionCatalogEntry struct {
	region   string
	hasToken bool
	storage  []byte // physical credential document (read-only view)
	snapshot *persistedModelCache
}

// handleModelGroupsQuery answers GET /plugins/qoder/models/groups.
// Returns (status, payload); status 200 carries modelGroupsResponse.
func handleModelGroupsQuery(req pluginapi.ManagementRequest) (int, any) {
	refresh := strings.TrimSpace(req.Query.Get("refresh")) == "1"

	files, err := hostAuthListFn()
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": "auth list failed: " + err.Error()}
	}

	perRegion := map[string][]*regionCatalogEntry{}
	for _, f := range files {
		phys, err := hostAuthGetPhysicalFn(f.AuthIndex)
		if err != nil || phys == nil || len(phys.JSON) == 0 {
			continue
		}
		sa, err := parseStored(phys.JSON)
		if err != nil || sa == nil {
			continue
		}
		// hostAuthListFn already filtered foreign owners; region resolution
		// mirrors persistedSnapshotForStorage's same-region guard.
		region := authRegion(sa)
		e := &regionCatalogEntry{
			region:   region,
			hasToken: strings.TrimSpace(sa.Auth.AccessToken) != "",
			storage:  phys.JSON,
		}
		if c := readModelCacheDoc(phys.JSON); c != nil && c.Region == region {
			e.snapshot = c
		}
		perRegion[region] = append(perRegion[region], e)
	}

	groups := make([]modelGroup, 0, len(modelGroupRegionOrder))
	for _, region := range modelGroupRegionOrder {
		groups = append(groups, buildModelGroup(region, perRegion[region], refresh))
	}
	return http.StatusOK, modelGroupsResponse{
		Groups: groups,
		Hint:   "勾选=启用；取消勾选保存后该模型从渠道注册列表移除。与 CPA 全局页驱动同一份 oauth-excluded-models 配置。",
	}
}

// buildModelGroup resolves one region's catalog: newest same-region snapshot
// first; with refresh=1 a live discovery (via the for_auth chain) takes
// precedence and re-stamps the snapshot. Never serves a cross-region snapshot.
func buildModelGroup(region string, entries []*regionCatalogEntry, refresh bool) modelGroup {
	g := modelGroup{
		Realm:       region,
		Label:       modelGroupRegionLabel(region),
		Models:      []modelGroupEntry{},
		Credentials: len(entries),
	}

	var best *persistedModelCache
	for _, e := range entries {
		if e.snapshot == nil {
			continue
		}
		if best == nil || persistedFetchedAt(e.snapshot) > persistedFetchedAt(best) {
			best = e.snapshot
		}
	}

	if refresh {
		if pick := pickRefreshCredential(entries); pick != nil {
			models := fetchDynamicModelsFromStorage(pick.storage)
			if len(models) > 0 {
				g.Models = modelGroupEntries(models)
				g.Count = len(g.Models)
				g.Source = "refreshed"
				g.FetchedAt = time.Now().UTC().Format(time.RFC3339)
				return g
			}
			g.Note = "刷新未获得模型（上游失败或凭证无效）；以下为最近一次落盘目录"
		} else {
			g.Note = "无可用凭证，无法实时刷新"
		}
	}

	if best != nil {
		g.Models = modelGroupEntries(best.Models)
		g.Count = len(g.Models)
		g.Source = "snapshot"
		g.FetchedAt = best.FetchedAt
	}
	if len(g.Models) == 0 {
		g.NeedsRefresh = true
		if g.Note == "" {
			g.Note = "暂无目录：该渠道还没有成功拉取过模型，点「刷新目录」试一次"
		}
	}
	return g
}

// pickRefreshCredential chooses the credential to drive a refresh with:
// token-bearing and enabled first, then any token bearer, then any entry —
// the for_auth chain answers from the persisted snapshot even without a
// token, but a token gives it a real chance at fresh discovery.
func pickRefreshCredential(entries []*regionCatalogEntry) *regionCatalogEntry {
	var any, withToken *regionCatalogEntry
	for _, e := range entries {
		if any == nil {
			any = e
		}
		if !e.hasToken {
			continue
		}
		if withToken == nil {
			withToken = e
		}
	}
	if withToken != nil {
		return withToken
	}
	return any
}

// modelGroupEntries projects ModelInfos into picker rows (id + display name).
func modelGroupEntries(models []pluginapi.ModelInfo) []modelGroupEntry {
	out := make([]modelGroupEntry, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = m.Name
		}
		if strings.EqualFold(name, m.ID) {
			name = ""
		}
		out = append(out, modelGroupEntry{ID: m.ID, Name: name})
	}
	return out
}
