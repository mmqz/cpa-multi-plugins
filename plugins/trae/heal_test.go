package main

// heal_test.go — v0.12.26: unit tests for the atomic write and the
// registration self-heal (see heal.go for the root-cause write-up).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestAtomicWriteJSON_CompleteAndOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trae-heal-1.json")
	if err := atomicWriteJSON(path, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("atomicWriteJSON: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("read back: %q err=%v", got, err)
	}
	// Overwrite in place — rename must replace the existing file.
	if err := atomicWriteJSON(path, []byte(`{"a":2}`)); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != `{"a":2}` {
		t.Fatalf("after overwrite: %q", got)
	}
	// No temp leftovers.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cpa-tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestDiskCredentialFiles_PrefixAndJSONOnly(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"trae-1.json", "trae-intl-2.json", "trae-nested.json",
		"other-3.json", "trae-notjson.txt", "trae-dir.json",
	} {
		if name == "trae-dir.json" {
			if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := diskCredentialFiles(dir, "trae-")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := []string{"trae-1.json", "trae-intl-2.json", "trae-nested.json"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestHealAuthRegistration_HealsOnlyMissingValidJSON(t *testing.T) {
	dir := t.TempDir()
	validOnDisk := `{"type":"trae","auth":{"accessToken":"a"},"account":{"uid":"u1"}}`
	invalidOnDisk := `{"type":"trae","auth":` // torn write remnant
	if err := os.WriteFile(filepath.Join(dir, "trae-missing.json"), []byte(validOnDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trae-torn.json"), []byte(invalidOnDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trae-registered.json"), []byte(`{"ok":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	origDir := authDirCache.dir
	origSave := healSaveFn
	defer func() { authDirCache.dir = origDir; healSaveFn = origSave }()
	authDirCache.dir = dir

	var saved []string
	healSaveFn = func(name string, raw []byte) error {
		saved = append(saved, name)
		return nil
	}
	hosted := func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{Name: "trae-registered.json"}}, nil
	}

	got := healAuthRegistration("trae-", hosted)
	if got != 1 {
		t.Fatalf("healed=%d want 1", got)
	}
	if len(saved) != 1 || saved[0] != "trae-missing.json" {
		t.Fatalf("saved=%v want [trae-missing.json]", saved)
	}
}

func TestHealAuthRegistration_InvalidJSONNeverResaved(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "trae-torn.json"), []byte(`{"broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	origDir := authDirCache.dir
	origSave := healSaveFn
	defer func() { authDirCache.dir = origDir; healSaveFn = origSave }()
	authDirCache.dir = dir
	called := false
	healSaveFn = func(name string, raw []byte) error { called = true; return nil }
	if got := healAuthRegistration("trae-", func() ([]pluginapi.HostAuthFileEntry, error) { return nil, nil }); got != 0 {
		t.Fatalf("healed=%d want 0", got)
	}
	if called {
		t.Fatal("torn file must never be re-saved")
	}
}

func TestHealAuthRegistration_ValidJSONIntact(t *testing.T) {
	// The bytes re-saved are exactly the bytes on disk (json.Valid, not
	// re-marshal) so host.auth.save upserts identical content.
	dir := t.TempDir()
	raw := []byte("{\n  \"type\": \"trae\",\n  \"account\": {\"uid\": \"u9\"}\n}")
	if err := os.WriteFile(filepath.Join(dir, "trae-gone.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	origDir := authDirCache.dir
	origSave := healSaveFn
	defer func() { authDirCache.dir = origDir; healSaveFn = origSave }()
	authDirCache.dir = dir
	var gotRaw []byte
	healSaveFn = func(name string, rawBytes []byte) error { gotRaw = rawBytes; return nil }
	healAuthRegistration("trae-", func() ([]pluginapi.HostAuthFileEntry, error) { return nil, nil })
	if !bytes.Equal(raw, gotRaw) {
		t.Fatalf("re-saved bytes differ from disk: %q vs %q", gotRaw, raw)
	}
}
