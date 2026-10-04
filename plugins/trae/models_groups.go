// models_groups.go — v0.12.63: per-variant model catalog endpoint for the
// panel's model-exclusion picker (GET /plugins/trae/models/groups). Port of
// the workbuddy v0.9.40 design onto trae's cn/solo/intl variant namespaces.
//
// Why: the HOST's global oauth-excluded-models page cannot offer candidate
// lists for plugin channels (model-definitions/:channel is an upstream-static
// 8-channel catalog; unknown channels 400 "unknown channel"), so excluding a
// trae model there meant hand-typing suffixed ids. This endpoint serves the
// catalog from the plugin's OWN knowledge — the per-credential model_cache
// snapshots (v0.12.63) — grouped by variant namespace, and the panel renders
// per-variant checkboxes that write the host's trae-<variant> sub-keys via
// PATCH /v0/management/oauth-excluded-models. Consumption stays
// host-config-shaped (models_exclusion.go), so the global page and this
// picker drive the SAME exclusion map — no parallel truth, nothing
// plugin-proprietary to persist.
//
// Read-only by default: groups are served from the newest model_cache
// snapshot per variant (zero upstream calls). ?refresh=1 additionally runs
// one live discovery per variant through the credential's own for_auth chain
// (refresh → discover → snapshot → static), which also re-stamps the
// snapshot — so what the picker shows and what the runtime advertises cannot
// diverge. Excluded models stay listed (the snapshot is pre-exclusion), so
// un-excluding never requires a lucky upstream round-trip.
package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// modelGroupVariantOrder is the fixed presentation order; a variant with
// zero credentials still renders (empty group) so the picker shows every
// channel.
var modelGroupVariantOrder = []string{"cn", "solo", "intl"}

// modelGroupVariantLabel renders the human-facing variant title. Keep in sync
// with the login_variant enum description in main.go and the panel badges.
func modelGroupVariantLabel(variant string) string {
	switch variant {
	case "cn":
		return "CN（trae.cn）"
	case "solo":
		return "Solo CN（trae.cn SOLO）"
	case "intl":
		return "Intl（trae.ai）"
	default:
		return variant
	}
}

// modelGroupEntry is one catalog row. Name carries the display name when the
// snapshot/discovery provided one.
type modelGroupEntry struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// modelGroup is one variant's catalog as the panel picker consumes it.
type modelGroup struct {
	Realm        string            `json:"realm"`
	Label        string            `json:"label"`
	Models       []modelGroupEntry `json:"models"`
	Count        int               `json:"count"`
	Source       string            `json:"source,omitempty"`     // snapshot | refreshed
	FetchedAt    string            `json:"fetched_at,omitempty"` // RFC3339 UTC
	NeedsRefresh bool              `json:"needs_refresh"`        // empty catalog → suggest the refresh button
	Credentials  int               `json:"credentials"`          // trae credentials seen in this variant
	Note         string            `json:"note,omitempty"`       // refresh failure reason when a refresh produced nothing
}

type modelGroupsResponse struct {
	Groups []modelGroup `json:"groups"`
	Hint   string       `json:"hint"`
}

// variantCatalogEntry is one credential's contribution to a variant catalog scan.
type variantCatalogEntry struct {
	variant  string
	hasToken bool
	name     string // host file name — drives the refresh persist path
	storage  []byte // physical credential document (read-only view)
	snapshot *persistedModelCache
}

// handleModelGroupsQuery answers GET /plugins/trae/models/groups.
// Returns (status, payload); status 200 carries modelGroupsResponse.
func handleModelGroupsQuery(req pluginapi.ManagementRequest) (int, any) {
	refresh := strings.TrimSpace(req.Query.Get("refresh")) == "1"

	files, err := hostAuthListFn()
	if err != nil {
		return http.StatusInternalServerError, map[string]any{"error": "auth list failed: " + err.Error()}
	}

	perVariant := map[string][]*variantCatalogEntry{}
	for _, f := range files {
		phys, err := hostAuthRawFn(f.AuthIndex)
		if err != nil || len(phys) == 0 {
			continue
		}
		// hostAuthListFn already filtered foreign owners; variant resolution
		// mirrors persistedSnapshotForStorage's same-variant guard.
		variant := sniffVariantFromJSON(phys)
		e := &variantCatalogEntry{
			variant:  variant,
			hasToken: strings.TrimSpace(extractTraeAccessToken(phys)) != "",
			name:     f.Name,
			storage:  phys,
		}
		if c := readModelCacheDoc(phys); c != nil && c.Realm == variant {
			e.snapshot = c
		}
		perVariant[variant] = append(perVariant[variant], e)
	}

	groups := make([]modelGroup, 0, len(modelGroupVariantOrder))
	for _, variant := range modelGroupVariantOrder {
		groups = append(groups, buildModelGroup(variant, perVariant[variant], refresh))
	}
	return http.StatusOK, modelGroupsResponse{
		Groups: groups,
		Hint:   "勾选=启用；取消勾选保存后该模型从渠道注册列表移除（ID 带渠道后缀）。与 CPA 全局页驱动同一份 oauth-excluded-models 配置。",
	}
}

// buildModelGroup resolves one variant's catalog: newest same-variant
// snapshot first; with refresh=1 a live discovery (via the credential's
// for_auth chain) takes precedence and re-stamps the snapshot. Never serves
// a cross-variant snapshot.
func buildModelGroup(variant string, entries []*variantCatalogEntry, refresh bool) modelGroup {
	g := modelGroup{
		Realm:       variant,
		Label:       modelGroupVariantLabel(variant),
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
			models := refreshVariantCatalog(pick)
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
// token-bearing first, then any entry — the for_auth chain answers from the
// persisted snapshot even without a token, but a token gives it a real
// chance at fresh discovery.
func pickRefreshCredential(entries []*variantCatalogEntry) *variantCatalogEntry {
	var any, withToken *variantCatalogEntry
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

// refreshVariantCatalog runs ONE live discovery for the picked credential
// through the exact path model.for_auth uses (intl credentials ride the
// intl chain, cn/solo the solo_work_lite lane) — pre-exclusion, so the
// picker can also un-exclude.
func refreshVariantCatalog(pick *variantCatalogEntry) []pluginapi.ModelInfo {
	if pick == nil || len(pick.storage) == 0 {
		return nil
	}
	if pick.variant == variantIntl {
		a, err := intlparseStoredAuth(pick.storage)
		if err != nil {
			return nil
		}
		return intlmodelCatalogForAuth(pick.storage, pick.name, a)
	}
	a, err := parseStoredAuth(pick.storage)
	if err != nil {
		return nil
	}
	return modelsForVariant(a, pick.storage)
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
