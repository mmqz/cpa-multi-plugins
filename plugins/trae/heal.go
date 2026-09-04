package main

// heal.go — v0.12.26: credential-registration self-healing + atomic writes.
//
// Root cause this file addresses (source-traced + live-probed 2026-09-04,
// host CLIProxyAPI watcher/synthesizer/dispatcher):
//
// The host runs a FULL auth-dir reconciliation on every config reload and on
// every incremental auth event it cannot attribute incrementally
// (internal/watcher/clients.go: reloadClients -> refreshAuthState ->
// snapshotCoreAuths -> FileSynthesizer.Synthesize over every *.json).
// If a credential file cannot be parsed AT THAT INSTANT (empty / torn read
// while a writer holds it open), synthesizeFileAuths returns nil WITHOUT an
// error (internal/watcher/synthesizer/file.go: json.Unmarshal fail ->
// `return nil, nil`), the auth is missing from the reconciled snapshot, and
// prepareAuthUpdatesLocked (internal/watcher/dispatcher.go) emits a DELETE:
// the credential vanishes from the in-memory manager — panels and
// /v0/management/auth-files stop listing it — while the file REMAINS on
// disk. "Plugin shows fewer accounts than actual credentials."
//
// The state also fails to self-heal: when the writer finishes, the file's
// Write event reaches addOrUpdateClientLocked, but lastAuthHashes still
// holds the pre-write hash (only rescanAuth=true reloads rebuild it), the
// finished content matches, and the event is skipped as "unchanged"
// (clients.go: hash match -> skip) — no re-registration. Only a host restart
// (rescanAuth=true) or a later content change brings the account back.
//
// Verified live 2026-09-04 (cpa-test, trae v0.12.25): emptying any trae-*.json
// (typed OR type-less) then flipping config debug true/false reproduced the
// disappearance (7 -> 6 -> 5 entries; files still on disk; "full reloads: 6"
// in the host log); restoring the bytes restored the listing.
//
// Two defenses live here:
//   1. atomicWriteJSON — plugin-side writes never expose a torn/empty file
//      to the host's reconciliation (temp + fsync + rename; the temp name
//      deliberately does NOT end in .json so the watcher ignores it).
//   2. healAuthRegistration — panel loads compare disk vs host registration
//      and re-save any disk-only credential through host.auth.save, which
//      upserts the manager record directly (bypassing the watcher skip).

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// atomicWriteJSON writes data to path without any observable torn state:
// create a sibling temp file (name not ending in .json so the host watcher
// ignores it), write, fsync, then rename over the destination. Rename is
// atomic on POSIX and Windows-on-same-volume; readers see either the old
// file or the complete new one, never a half-written credential.
func atomicWriteJSON(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cpa-tmp-*.part")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}

// diskCredentialFiles lists the *.json file NAMES under authDir whose lowercased
// name carries the given prefix. Pure disk scan — no RPC — so it is unit-testable.
func diskCredentialFiles(authDir, prefix string) ([]string, error) {
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return nil, err
	}
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e == nil || e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if prefix != "" && !strings.HasPrefix(lower, prefix) {
			continue
		}
		if !strings.HasSuffix(lower, ".json") {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// healAuthRegistration re-registers credentials that exist on disk under
// authDir but are absent from the host's in-memory manager (listed supplies
// the host view). Only syntactically valid JSON is re-saved — a torn file is
// never propagated. Each heal goes through host.auth.save, whose saveAuthFile
// upserts the manager record immediately (auth_callbacks.go: write + upsert),
// independent of the watcher's hash-skip. Returns how many were healed.
//
// healSaveFn is a package var so tests can intercept the save RPC.
var healSaveFn = hostAuthSave

func healAuthRegistration(prefix string, listed func() ([]pluginapi.HostAuthFileEntry, error)) int {
	dir := cachedAuthDir()
	if dir == "" {
		return 0
	}
	hostFiles, err := listed()
	if err != nil {
		return 0
	}
	have := make(map[string]bool, len(hostFiles))
	for _, f := range hostFiles {
		have[strings.ToLower(f.Name)] = true
	}
	diskNames, err := diskCredentialFiles(dir, prefix)
	if err != nil {
		return 0
	}
	healed := 0
	for _, name := range diskNames {
		if have[strings.ToLower(name)] {
			continue
		}
		full := filepath.Join(dir, name)
		data, errRead := os.ReadFile(full)
		if errRead != nil || len(data) == 0 || !json.Valid(data) {
			// Torn/invalid file: re-saving garbage would poison the store.
			log.Printf("trae heal: skip unparsable %s (torn or invalid JSON)", name)
			continue
		}
		if err := healSaveFn(name, data); err != nil {
			log.Printf("trae heal: re-save %s: %v", name, err)
			continue
		}
		healed++
		log.Printf("trae heal: re-registered %s (missing from host manager; file intact on disk)", name)
	}
	return healed
}
