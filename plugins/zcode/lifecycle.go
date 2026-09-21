// lifecycle.go applies quota-driven lifecycle actions (disable on exhaustion)
// and keeps auth-file notes in sync with live quota. Simplified from the
// qoder plugin: zcode has no check-in restore path, so re-enable happens when
// a panel refresh observes fresh quota.
package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// lifecycleState remembers the last note/disabled pair written per auth so
// redundant host.auth.save round-trips are skipped.
var (
	lifecycleState sync.Map // auth_id -> lifecycleStamp
)

type lifecycleStamp struct {
	disabled bool
	note     string
}

func lifecycleStateUnchanged(authID string, disabled bool, note string) bool {
	v, ok := lifecycleState.Load(authID)
	if !ok {
		return false
	}
	st, ok := v.(*lifecycleStamp)
	return ok && st.disabled == disabled && st.note == note
}

func rememberLifecycleState(authID string, disabled bool, note string) {
	if authID == "" {
		return
	}
	lifecycleState.Store(authID, &lifecycleStamp{disabled: disabled, note: note})
}

// pruneLifecycleState drops entries for auths that no longer exist.
func pruneLifecycleState(live map[string]struct{}) {
	lifecycleState.Range(func(key, value any) bool {
		id, _ := key.(string)
		if _, ok := live[id]; !ok {
			lifecycleState.Delete(key)
		}
		return true
	})
}

// disableAuth persists disabled:true with a reason-carrying note.
func disableAuth(authIndex, authID string, sa *storedAuth, cr *creditsSummary, reason string) error {
	note := displayNote(sa, cr, true)
	if reason != "" {
		note += " · " + reason
	}
	raw, err := buildAuthFileJSON(sa, true, note, nil)
	if err != nil {
		return err
	}
	if err := hostAuthPersistFn(authFileNameFor(sa), raw); err != nil {
		return err
	}
	rememberLifecycleState(authID, true, note)
	return nil
}

// reenableAuth persists disabled:false.
func reenableAuth(authIndex, authID string, sa *storedAuth, cr *creditsSummary) error {
	note := displayNote(sa, cr, false)
	raw, err := buildAuthFileJSON(sa, false, note, nil)
	if err != nil {
		return err
	}
	if err := hostAuthPersistFn(authFileNameFor(sa), raw); err != nil {
		return err
	}
	rememberLifecycleState(authID, false, note)
	return nil
}

// existingNoteCredits reads the credit segment already stored on disk so a
// failed quota query can keep the last known value instead of regressing the
// card to "积分未知".
func existingNoteCredits(authIndex string) string {
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err != nil || phys == nil {
		return ""
	}
	return noteCreditsFromJSON(phys.JSON)
}

// noteCreditsFromJSON extracts the credit segment from a raw auth-file body.
func noteCreditsFromJSON(raw []byte) string {
	var pjson struct {
		Note string `json:"note"`
	}
	if json.Unmarshal(raw, &pjson) != nil {
		return ""
	}
	return creditSegmentFromNote(pjson.Note)
}

// syncAuthNote writes note without changing disabled state.
//
// cr == nil means "quota unknown this round" (upstream error or not fetched
// yet). In that case the previously stored credit segment is preserved: a
// transient billing failure must not erase a note the user already sees.
func syncAuthNote(authIndex, authID string, sa *storedAuth, cr *creditsSummary, disabled bool) error {
	if sa == nil {
		return nil
	}
	note := displayNoteWithPrev(sa, cr, disabled, existingNoteCredits(authIndex))
	if lifecycleStateUnchanged(authID, disabled, note) {
		return nil
	}
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err == nil && phys != nil {
		// re-read disabled + note from disk as source of truth
		diskDisabled := parseDisabledFromAuthJSON(phys.JSON)
		name, _ := resolveAuthFileTarget(sa, phys)
		note = displayNoteWithPrev(sa, cr, diskDisabled, noteCreditsFromJSON(phys.JSON))
		if lifecycleStateUnchanged(authID, diskDisabled, note) {
			return nil
		}
		raw, err := buildAuthFileJSON(sa, diskDisabled, note, nil)
		if err != nil {
			return err
		}
		if err := hostAuthPersistFn(name, raw); err != nil {
			return err
		}
		rememberLifecycleState(authID, diskDisabled, note)
		return nil
	}
	raw, err := buildAuthFileJSON(sa, disabled, note, nil)
	if err != nil {
		return err
	}
	if err := hostAuthPersistFn(authFileNameFor(sa), raw); err != nil {
		return err
	}
	rememberLifecycleState(authID, disabled, note)
	return nil
}

// reconcileOneAccount refreshes quota and applies lifecycle for one auth.
func reconcileOneAccount(authIndex, authID string, force bool) (action lifecycleAction, err error) {
	if !lifecycleEnabled() {
		return lifecycleNone, nil
	}
	sa, phys, err := hostAuthGetBundle(authIndex)
	if err != nil {
		return lifecycleNone, err
	}
	disabled := false
	if phys != nil {
		disabled = phys.Disabled
	}

	var cr *creditsSummary
	if !force {
		if v, ok := accountCache.Load(authID); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 && e.credits != nil && time.Since(e.fetched) < accountCacheTTL {
				cr = e.credits
			}
		}
	}
	if cr == nil {
		_, cr2, _ := cachedAccountDetails(authID, sa, true)
		cr = cr2
		if cr == nil {
			return lifecycleNone, nil
		}
	}

	if disabled {
		// Disabled account with fresh positive quota → re-enable.
		if !isCreditsExhausted(cr) && cr.TotalRemain > 0 {
			if err := reenableAuth(authIndex, authID, sa, cr); err != nil {
				return lifecycleNone, err
			}
			return lifecycleNone, nil
		}
		// still disabled: refresh note (throttled)
		_ = syncAuthNote(authIndex, authID, sa, cr, true)
		return lifecycleNone, nil
	}

	act := lifecycleActionFor(authProviderFor(sa), cr)
	switch act {
	case lifecycleDisable:
		return lifecycleDisable, disableAuth(authIndex, authID, sa, cr, "耗尽")
	default:
		// healthy: keep note fresh (throttled)
		_ = syncAuthNote(authIndex, authID, sa, cr, false)
		return lifecycleNone, nil
	}
}

// reconcileAllAccounts walks zcode auths and applies lifecycle.
func reconcileAllAccounts(force bool) []map[string]any {
	if !lifecycleEnabled() {
		return nil
	}
	files, err := hostAuthList()
	if err != nil {
		return []map[string]any{{"error": err.Error()}}
	}
	out := make([]map[string]any, 0, len(files))
	for _, f := range files {
		act, err := reconcileOneAccount(f.AuthIndex, f.ID, force)
		row := map[string]any{"auth_index": f.AuthIndex, "action": act.String()}
		if err != nil {
			row["error"] = err.Error()
		}
		if act != lifecycleNone || err != nil {
			out = append(out, row)
		}
	}
	return out
}

// reconcileAfterExecutorError triggers lifecycle when upstream reports hard quota failure.
func reconcileAfterExecutorError(authID string, status int, body string) {
	if !lifecycleEnabled() || strings.TrimSpace(authID) == "" {
		return
	}
	if !isHardQuotaError(status, body) {
		return
	}
	go func() {
		idx, id := resolveAuthIndexAndID(authID)
		if idx == "" {
			return
		}
		_, _ = reconcileOneAccount(idx, id, true)
	}()
}

// resolveAuthIndexAndID maps executor AuthID (index, file id, or account UID)
// to host auth_index AND auth.ID. Returns ("", "") if not found.
func resolveAuthIndexAndID(authID string) (string, string) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return "", ""
	}
	// Fast path: already an auth_index the host understands.
	if _, err := hostAuthGet(authID); err == nil {
		if files, err := hostAuthList(); err == nil {
			for _, f := range files {
				if f.AuthIndex == authID {
					return authID, f.ID
				}
			}
		}
		return authID, ""
	}
	files, err := hostAuthList()
	if err != nil {
		return "", ""
	}
	wantName := providerName + "-" + authID + ".json"
	for _, f := range files {
		if f.AuthIndex == authID || f.ID == authID || f.Name == authID {
			return f.AuthIndex, f.ID
		}
		if listEntryMatchesUID(f, authID, wantName) {
			return f.AuthIndex, f.ID
		}
	}
	// Slow path: only when list metadata lacks uid (rare shapes).
	for _, f := range files {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			continue
		}
		if strings.TrimSpace(sa.Account.UID) == authID {
			return f.AuthIndex, f.ID
		}
	}
	return "", ""
}

// reconcileByUID finds zcode auth by account UID and applies executor-error lifecycle.
func reconcileByUID(uid string, status int, body string) {
	uid = strings.TrimSpace(uid)
	if uid == "" || !lifecycleEnabled() {
		return
	}
	if !isHardQuotaError(status, body) {
		return
	}
	idx, id := resolveAuthIndexAndID(uid)
	if idx == "" {
		return
	}
	_, _ = reconcileOneAccount(idx, id, true)
}

// invalidateAccountCredits drops cached quota so the next panel/reconcile
// fetch hits upstream. Call after a successful chat completion — otherwise a
// short TTL cache makes "used" look frozen while the user is burning quota.
func invalidateAccountCredits(authID, authUID string) {
	invalidateCredits := func(id string) {
		if v, ok := accountCache.Load(id); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 {
				fresh := *e
				fresh.credits = nil
				fresh.fetched = time.Now()
				accountCache.Store(id, &fresh)
			}
		}
	}
	if authID != "" {
		invalidateCredits(authID)
	}
	if authUID == "" || authUID == authID {
		return
	}
	files, err := hostAuthList()
	if err != nil {
		return
	}
	wantName := providerName + "-" + authUID + ".json"
	for _, f := range files {
		if f.AuthIndex == authID || f.ID == authID || f.Name == authID {
			invalidateCredits(f.ID)
			continue
		}
		if listEntryMatchesUID(f, authUID, wantName) {
			invalidateCredits(f.ID)
		}
	}
}

// listEntryMatchesUID reports whether a host list entry corresponds to a UID
// (by ID or by canonical file name).
func listEntryMatchesUID(f pluginapi.HostAuthFileEntry, uid, wantName string) bool {
	if uid == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(f.ID), uid) {
		return true
	}
	return wantName != "" && strings.EqualFold(strings.TrimSpace(f.Name), wantName)
}

// enrichAuthMetadata builds the standardized auth metadata map.
func enrichAuthMetadata(sa *storedAuth, cr *creditsSummary, disabled bool) map[string]any {
	return enrichAuthMetadataWithPrev(sa, cr, disabled, "")
}

// enrichAuthMetadataWithPrev is enrichAuthMetadata plus a previously known
// credit segment (used by AuthParse so reloads never regress the note).
func enrichAuthMetadataWithPrev(sa *storedAuth, cr *creditsSummary, disabled bool, prevCredits string) map[string]any {
	meta := map[string]any{
		"type":  providerName,
		"note":  displayNoteWithPrev(sa, cr, disabled, prevCredits),
		"logo":  pluginLogoURL,
		"brand": providerName,
	}
	if disabled {
		meta["disabled"] = true
	}
	if sa != nil {
		meta["provider"] = authProviderFor(sa)
		meta["plan"] = authPlanFor(sa)
	}
	return meta
}
