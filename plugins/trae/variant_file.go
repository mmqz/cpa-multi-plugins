package main

// variant_file.go — v0.12.27: per-variant credential file namespaces.
//
// Bug report this fixes (user-confirmed 2026-09-04): "solo 的清理了只剩下
// init 了？貌似还是转换成 init" — after a solo login the pre-existing init
// (cn) credential of the SAME Trae account disappeared, and/or the solo
// account showed up as init.
//
// Root cause (source-traced, no guessing):
//
//   Since the v0.12.0 merge (commit 06f737f) every CN/SOLO login saved its
//   credential as `trae-<uid>.json` — the FILE NAME carried no variant
//   dimension. The legacy standalone plugins used `trae-cn-<uid>.json` and
//   `trae-solo-cn-<uid>.json` (adopt.go still claims those prefixes), so
//   cn+solo dual logins of one account coexisted as two files. The merged
//   plugin collapsed both variants into ONE namespace: a re-login of the
//   same Trae account (same uid — solo and cn share the trae.cn user
//   system, only ClientID/platform differ) under the other variant silently
//   OVERWROTE the file and flipped its auth.variant. The panel's auth_index
//   dedupe is NOT the culprit: auth_index is sha256(type+abs(path))[:8]
//   (CLIProxyAPI sdk/cliproxy/auth/types.go indexSeed), i.e. file-level —
//   it never merges two different files.
//
// Fix:
//   1. credentialFileName — solo logins now save to their OWN namespace
//      (`trae-solo-cn-<uid>.json`, the legacy standalone plugin's naming so
//      pre-merge files and new files share one scheme). cn keeps
//      `trae-<uid>.json` (v0.12.0+ compatibility), intl keeps
//      `trae-intl-<uid>.json` (intlproviderName already namespaced).
//   2. migrateSoloFileNames — one-shot, idempotent migration for credentials
//      written by v0.12.0-0.12.26: any type=trae file whose auth.variant is
//      solo but whose name lacks the solo namespace is renamed into it and
//      re-registered via host.auth.save. Existing files are never
//      overwritten (conservative skip).

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// credentialFileName maps a login variant + account uid to the credential
// file name. solo lives in its own namespace (legacy trae-solo-cn plugin
// naming) so a cn+solo dual login of one Trae account coexists as two files
// instead of the later login overwriting the earlier one.
func credentialFileName(variant, uid string) string {
	uid = strings.TrimSpace(uid)
	switch normalizeVariant(variant) {
	case variantSolo:
		return fmt.Sprintf("%s-solo-cn-%s.json", providerName, uid)
	case variantIntl:
		return fmt.Sprintf("%s-intl-%s.json", providerName, uid)
	default:
		return fmt.Sprintf("%s-%s.json", providerName, uid)
	}
}

// soloNamespacePrefix is the file-name prefix of the solo credential
// namespace (also recognized by adopt.go's legacy claim logic).
const soloNamespacePrefix = providerName + "-solo-"

// migrateSoloFileNames moves v0.12.0-0.12.26 solo credentials written into
// the shared `trae-<uid>.json` namespace into the solo namespace. Idempotent:
// files already carrying the solo prefix, non-solo variants, unparsable JSON
// and conflicting targets are left untouched. Runs on every CN dashboard
// load (before heal) — cheap after the first pass (nothing left to move).
func migrateSoloFileNames() int {
	dir := cachedAuthDir()
	if dir == "" {
		return 0
	}
	names, err := diskCredentialFiles(dir, providerName+"-")
	if err != nil {
		return 0
	}
	moved := 0
	for _, name := range names {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, soloNamespacePrefix) ||
			strings.HasPrefix(lower, providerName+"-intl-") {
			continue // already namespaced
		}
		full := filepath.Join(dir, name)
		raw, errRead := os.ReadFile(full)
		if errRead != nil || len(raw) == 0 || !json.Valid(raw) {
			continue // torn/invalid: heal.go governs these, never rename garbage
		}
		var probe struct {
			Type string `json:"type"`
			Auth struct {
				Variant string `json:"variant"`
			} `json:"auth"`
			Account struct {
				UID string `json:"uid"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			continue
		}
		if strings.TrimSpace(probe.Type) != providerName {
			continue // legacy-type files keep their (already prefixed) names
		}
		variant := probe.Auth.Variant
		if variant == "" {
			variant = sniffVariantFromJSON(raw)
		}
		if normalizeVariant(variant) != variantSolo {
			continue // cn (and intl sniffed) files stay where they are
		}
		uid := strings.TrimSpace(probe.Account.UID)
		if uid == "" {
			// Derive from the name: trae-<uid>.json -> <uid>.
			uid = strings.TrimSuffix(strings.TrimPrefix(lower, providerName+"-"), ".json")
		}
		target := credentialFileName(variantSolo, uid)
		if strings.EqualFold(target, name) {
			continue
		}
		targetPath := filepath.Join(dir, target)
		if _, statErr := os.Stat(targetPath); statErr == nil {
			// Conservative: never overwrite an existing credential (a legacy
			// trae-solo-cn file for the same account may hold the older but
			// user-verified token). The shared-namespace file keeps working.
			log.Printf("trae migrate: %s -> %s skipped (target exists)", name, target)
			continue
		}
		if err := os.Rename(full, targetPath); err != nil {
			log.Printf("trae migrate: rename %s: %v", name, err)
			continue
		}
		// Register the new name immediately (v0.12.26 lesson: do not rely on
		// the watcher; host.auth.save upserts the manager record directly).
		if err := healSaveFn(target, raw); err != nil {
			log.Printf("trae migrate: register %s: %v", target, err)
			continue
		}
		moved++
		log.Printf("trae migrate: %s -> %s (solo namespace)", name, target)
	}
	return moved
}
