// adopt.go migrates legacy zcode auth shapes to the canonical file layout.
//
// zcode ships without legacy predecessors, so adoption today only normalizes
// the single-file zcode.json shape (no UID suffix) when it carries a UID:
// the JSON is rewritten to zcode-<provider>-<uid>.json and the legacy file
// removed. Idempotent (files already carrying the canonical name are
// skipped), guarded by a min re-run interval, and never blocks plugin
// registration (background).
package main

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"
)

var (
	adoptMu      sync.Mutex
	lastAdoptRun time.Time
)

// startAdoption kicks the background migration. Safe to call on every
// register/reconfigure — the guard collapses repeated calls.
func startAdoption() {
	go func() {
		adoptMu.Lock()
		if !lastAdoptRun.IsZero() && time.Since(lastAdoptRun) < time.Minute {
			adoptMu.Unlock()
			return
		}
		lastAdoptRun = time.Now()
		adoptMu.Unlock()
		adoptLegacyAuths()
	}()
}

// adoptLegacyAuths rewrites single-file zcode.json auths that carry a UID to
// the canonical zcode-<provider>-<uid>.json name.
func adoptLegacyAuths() {
	files, err := hostAuthList()
	if err != nil {
		log.Printf("adopt: host auth list failed: %v", err)
		return
	}
	adopted := 0
	for _, f := range files {
		name := strings.ToLower(strings.TrimSpace(f.Name))
		if name != authFileName { // zcode.json — the only legacy shape today
			continue
		}
		phys, err := hostAuthGetPhysical(f.AuthIndex)
		if err != nil {
			log.Printf("adopt %s: get failed: %v", f.Name, err)
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(phys.JSON, &probe)
		_ = probe
		sa, err := parseStored(phys.JSON)
		if err != nil || sa == nil {
			continue
		}
		if sanitizeUIDForFileName(sa.Account.UID) == "" {
			continue // anonymous single-account file stays as-is
		}
		disabled := parseDisabledFromAuthJSON(phys.JSON)
		note := displayNoteWithPrev(sa, nil, disabled, noteCreditsFromJSON(phys.JSON))
		raw, err := buildAuthFileJSON(sa, disabled, note, nil)
		if err != nil {
			continue
		}
		canonical := authFileNameFor(sa)
		if canonical == f.Name {
			continue
		}
		if err := hostAuthPersistFn(canonical, raw); err != nil {
			log.Printf("adopt %s: save failed: %v", f.Name, err)
			continue
		}
		log.Printf("adopt: %s -> %s", f.Name, canonical)
		adopted++
	}
	if adopted > 0 {
		log.Printf("adopt: done — migrated %d legacy zcode auth file(s)", adopted)
	}
}
