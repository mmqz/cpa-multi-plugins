// adopt.go migrates legacy codebuddy-cn auth files to the merged workbuddy
// provider. History: workbuddy and codebuddy-cn were separate plugins against
// the SAME backend (copilot.tencent.com) sharing one credit pool, so they are
// merged into this single plugin (v0.9.0). Files written by the old
// codebuddy-cn plugin carry type/provider "codebuddy-cn" and would otherwise
// be orphaned (the host routes auth files to plugins by the file's type
// field), so on startup we rewrite them into canonical workbuddy files:
//
//	codebuddy-cn-<uid>.json  →  workbuddy-<uid>.json
//	type/provider: codebuddy-cn  →  workbuddy
//	auth.loginPlatform = "ide"   (CodeBuddy IDE login origin → X-IDE-* headers)
//
// The migration is idempotent, guarded by a min re-run interval, and never
// blocks plugin registration (runs in a background goroutine).
package main

import (
	"encoding/json"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	adoptMu       sync.Mutex
	lastAdoptRun  time.Time
	adoptInterval = time.Minute
)

// startAdoption kicks the background migration. Safe to call on every
// register/reconfigure — the guard collapses repeated calls.
func startAdoption() {
	go func() {
		adoptMu.Lock()
		if !lastAdoptRun.IsZero() && time.Since(lastAdoptRun) < adoptInterval {
			adoptMu.Unlock()
			return
		}
		lastAdoptRun = time.Now()
		adoptMu.Unlock()
		adoptForeignAuths()
	}()
}

// isLegacyCodebuddyAuthName reports whether an auth file name originates from
// the removed codebuddy-cn plugin.
func isLegacyCodebuddyAuthName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(lower, "codebuddy-cn-") || lower == "codebuddy-cn.json"
}

// isLegacyCodebuddyIntlAuthName reports whether an auth file name originates
// from the removed codebuddy-intl plugin (merged in v0.11.0). Those accounts
// live on the codebuddy.ai realm and adopt into region-qualified
// workbuddy-intl-<uid>.json names (never collide with CN/Global files).
func isLegacyCodebuddyIntlAuthName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(lower, "codebuddy-intl-") || lower == "codebuddy-intl.json"
}

// adoptForeignAuths rewrites every legacy codebuddy-cn auth file into the
// canonical workbuddy form. Files whose UID already exists as a workbuddy
// auth are treated as duplicates and removed (same backend credential).
func adoptForeignAuths() {
	files, err := hostAuthList()
	if err != nil {
		log.Printf("adopt: host auth list failed: %v", err)
		return
	}
	// Index existing canonical workbuddy files by UID-bearing name.
	existing := make(map[string]struct{}, len(files))
	for _, f := range files {
		if !isLegacyCodebuddyAuthName(f.Name) && !isLegacyCodebuddyIntlAuthName(f.Name) {
			existing[strings.ToLower(f.Name)] = struct{}{}
		}
	}
	adopted, deduped, skipped := 0, 0, 0
	for _, f := range files {
		if !isLegacyCodebuddyAuthName(f.Name) && !isLegacyCodebuddyIntlAuthName(f.Name) {
			continue
		}
		phys, err := hostAuthGetPhysical(f.AuthIndex)
		if err != nil {
			log.Printf("adopt %s: get failed: %v", f.Name, err)
			skipped++
			continue
		}
		sa, err := parseStored(phys.JSON)
		if err != nil || sa == nil || strings.TrimSpace(sa.Account.UID) == "" {
			log.Printf("adopt %s: unparsable or missing uid — left in place (re-import manually)", f.Name)
			skipped++
			continue
		}
		// Pin the Intl realm before deriving the canonical name: codebuddy.ai
		// accounts adopt into workbuddy-intl-<uid>.json and must carry the
		// domain + IDE login origin for routing/headers.
		if isLegacyCodebuddyIntlAuthName(f.Name) && strings.TrimSpace(sa.Auth.Domain) == "" {
			sa.Auth.Domain = "codebuddy.ai"
		}
		canonical := authFileNameFor(sa)
		_, dup := existing[strings.ToLower(canonical)]
		if dup {
			// Same credential already present as a workbuddy auth — drop the
			// legacy copy instead of registering the account twice.
			if phys.Path != "" && filepath.IsAbs(phys.Path) {
				if err := deleteAuthFileInDir(phys.Path, filepath.Dir(phys.Path)); err != nil {
					log.Printf("adopt %s: duplicate cleanup failed: %v", f.Name, err)
					skipped++
					continue
				}
			}
			log.Printf("adopt %s: duplicate of %s — legacy file removed", f.Name, canonical)
			deduped++
			continue
		}
		// Pin the IDE login origin so requests carry the X-IDE-* client
		// headers the upstream saw when this token was minted.
		if strings.TrimSpace(sa.Auth.LoginPlatform) == "" {
			sa.Auth.LoginPlatform = "ide"
		}
		// Preserve an existing note if the file carried one.
		family := "codebuddy-cn"
		if isLegacyCodebuddyIntlAuthName(f.Name) {
			family = "codebuddy-intl"
		}
		note := "migrated from " + family
		var meta struct {
			Note string `json:"note"`
		}
		if json.Unmarshal(phys.JSON, &meta) == nil && strings.TrimSpace(meta.Note) != "" {
			note = meta.Note
		}
		raw, err := buildAuthFileJSON(sa, phys.Disabled, note, nil)
		if err != nil {
			log.Printf("adopt %s: encode failed: %v", f.Name, err)
			skipped++
			continue
		}
		if err := hostAuthPersist(canonical, "", raw); err != nil {
			log.Printf("adopt %s: save %s failed: %v", f.Name, canonical, err)
			skipped++
			continue
		}
		if phys.Path != "" && filepath.IsAbs(phys.Path) {
			if err := deleteAuthFileInDir(phys.Path, filepath.Dir(phys.Path)); err != nil {
				log.Printf("adopt %s: saved as %s but legacy cleanup failed: %v", f.Name, canonical, err)
			}
		}
		log.Printf("adopt %s: migrated to %s (login platform: ide)", f.Name, canonical)
		adopted++
		existing[strings.ToLower(canonical)] = struct{}{}
	}
	if adopted+deduped+skipped > 0 {
		log.Printf("adopt: done — migrated %d, deduped %d, skipped %d", adopted, deduped, skipped)
	}
}
