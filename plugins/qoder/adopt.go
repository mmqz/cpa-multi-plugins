// adopt.go migrates legacy qoder-cn / qoder-intl auth files to the merged
// qoder provider (v0.10.0).
//
// History: qoder-cn (openapi.qoder.com.cn) and qoder-intl (openapi.qoder.sh)
// were separate plugins against DIFFERENT backends, so unlike the workbuddy
// merge (same backend → dedupe by UID) there is NO dedupe here. Filenames
// are KEPT — qoder-cn-<uid>.json / qoder-intl-<uid>.json already carry the
// canonical "qoder-" prefix — and only the JSON is rewritten in place:
//
//	type/provider: qoder-cn | qoder-intl → qoder
//	auth.region:   cn | intl (explicit; region derived from the filename)
//	auth.domain:   qoder.com.cn | qoder.com (the intl plugin hardcoded the CN domain)
//
// The host routes auth files to plugins by the file's type field, so without
// this rewrite legacy files would be orphaned. Idempotent (files already
// carrying type "qoder" + an explicit region are skipped), guarded by a min
// re-run interval, and never blocks plugin registration (background).
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
		adoptForeignAuths()
	}()
}

// legacyQoderRegion maps a legacy auth file name to its region ("" = not legacy).
func legacyQoderRegion(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.HasPrefix(lower, "qoder-intl-"), lower == "qoder-intl.json":
		return regionIntl
	case strings.HasPrefix(lower, "qoder-cn-"), lower == "qoder-cn.json":
		return regionCN
	}
	return ""
}

// adoptForeignAuths rewrites every legacy qoder-cn/qoder-intl auth file in
// place (same filename, updated JSON).
func adoptForeignAuths() {
	files, err := hostAuthList()
	if err != nil {
		log.Printf("adopt: host auth list failed: %v", err)
		return
	}
	adopted := 0
	for _, f := range files {
		region := legacyQoderRegion(f.Name)
		if region == "" {
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
		if strings.EqualFold(strings.TrimSpace(probe.Type), providerName) {
			continue // already canonical (this run or a previous one)
		}
		sa, err := parseStored(phys.JSON)
		if err != nil || sa == nil {
			log.Printf("adopt %s: unparsable — left in place (re-import manually)", f.Name)
			continue
		}
		sa.Auth.Region = region
		sa.Auth.Domain = domainForRegion(region)
		disabled := parseDisabledFromAuthJSON(phys.JSON)
		note := "migrated from " + map[string]string{regionCN: "qoder-cn", regionIntl: "qoder-intl"}[region]
		if raw, err := buildAuthFileJSON(sa, disabled, note, nil); err == nil {
			if err := hostAuthPersist(f.Name, "", raw); err != nil {
				log.Printf("adopt %s: save failed: %v", f.Name, err)
				continue
			}
			log.Printf("adopt %s: type→%s, region=%s", f.Name, providerName, region)
			adopted++
		} else {
			log.Printf("adopt %s: encode failed: %v", f.Name, err)
		}
	}
	if adopted > 0 {
		log.Printf("adopt: done — migrated %d legacy qoder auth file(s)", adopted)
	}
}
