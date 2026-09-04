package main

// variant_file_test.go — v0.12.27: per-variant credential file namespaces.
//
// The bug this pins (user-confirmed): solo and cn logins of the SAME Trae
// account both wrote `trae-<uid>.json`, so the later login silently
// overwrote the earlier one and flipped its auth.variant ("solo 变成
// init"). Solo now owns the `trae-solo-cn-<uid>.json` namespace (the legacy
// standalone plugin's naming), with a one-shot migration for files written
// by v0.12.0-0.12.26.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialFileName_PerVariantNamespaces(t *testing.T) {
	cases := []struct{ variant, uid, want string }{
		{"cn", "u1", "trae-u1.json"},
		{"", "u1", "trae-u1.json"},             // unknown variant -> cn namespace
		{"solo", "u1", "trae-solo-cn-u1.json"}, // own namespace — no cn collision
		{"SOLO", "u1", "trae-solo-cn-u1.json"}, // normalizeVariant is case-insensitive
		{"intl", "u1", "trae-intl-u1.json"},
	}
	for _, tc := range cases {
		if got := credentialFileName(tc.variant, tc.uid); got != tc.want {
			t.Fatalf("credentialFileName(%q,%q) = %q, want %q", tc.variant, tc.uid, got, tc.want)
		}
	}
	// The core regression: same uid, different variants -> different files.
	cn := credentialFileName("cn", "same-uid")
	solo := credentialFileName("solo", "same-uid")
	if cn == solo {
		t.Fatalf("cn and solo of one account must not share a file name: %q", cn)
	}
}

func TestCredentialFileName_LegacySoloPrefixMatch(t *testing.T) {
	// The solo namespace must equal the legacy trae-solo-cn plugin's
	// naming so adopt.go's legacy claim and pre-merge files line up.
	name := credentialFileName("solo", "u2")
	if !strings.HasPrefix(name, "trae-solo-cn-") {
		t.Fatalf("solo namespace %q does not carry the legacy trae-solo-cn- prefix", name)
	}
	if legacyTraeVariant(name) != variantSolo {
		t.Fatalf("legacyTraeVariant(%q) = %q, want solo", name, legacyTraeVariant(name))
	}
}

func TestMigrateSoloFileNames_MovesSharedNamespaceSolo(t *testing.T) {
	dir := t.TempDir()
	soloShared := `{"type":"trae","auth":{"accessToken":"a","variant":"solo"},"account":{"uid":"u-1"}}`
	if err := os.WriteFile(filepath.Join(dir, "trae-u-1.json"), []byte(soloShared), 0o600); err != nil {
		t.Fatal(err)
	}
	origDir, origSave := authDirCache.dir, healSaveFn
	defer func() { authDirCache.dir, healSaveFn = origDir, origSave }()
	authDirCache.dir = dir
	var saved []string
	healSaveFn = func(name string, raw []byte) error { saved = append(saved, name); return nil }

	if got := migrateSoloFileNames(); got != 1 {
		t.Fatalf("migrated=%d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "trae-u-1.json")); !os.IsNotExist(err) {
		t.Fatal("source file should have been renamed away")
	}
	moved, err := os.ReadFile(filepath.Join(dir, "trae-solo-cn-u-1.json"))
	if err != nil || string(moved) != soloShared {
		t.Fatalf("migrated file missing or altered: err=%v data=%q", err, moved)
	}
	if len(saved) != 1 || saved[0] != "trae-solo-cn-u-1.json" {
		t.Fatalf("registered=%v, want [trae-solo-cn-u-1.json]", saved)
	}
	// Idempotent: second pass is a no-op.
	if got := migrateSoloFileNames(); got != 0 {
		t.Fatalf("second pass migrated=%d, want 0", got)
	}
}

func TestMigrateSoloFileNames_LeavesEverythingElse(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		// cn credentials stay put (also the variant-less legacy shape).
		"trae-cn-keep.json": `{"type":"trae","auth":{"variant":"cn"},"account":{"uid":"c1"}}`,
		"trae-keep.json":    `{"type":"trae","auth":{},"account":{"uid":"c2"}}`,
		// Already-namespaced solo files stay put.
		"trae-solo-cn-x.json": `{"type":"trae","auth":{"variant":"solo"},"account":{"uid":"x"}}`,
		// Legacy typed files keep their names (adopt.go's domain).
		"trae-solo-cn-legacy.json": `{"type":"trae-solo-cn","auth":{},"account":{"uid":"l"}}`,
		// Torn JSON is never touched.
		"trae-torn.json": `{"type":"trae","auth":{"variant":"solo"`,
	}
	for name, raw := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	origDir, origSave := authDirCache.dir, healSaveFn
	defer func() { authDirCache.dir, healSaveFn = origDir, origSave }()
	authDirCache.dir = dir
	healSaveFn = func(name string, raw []byte) error {
		t.Fatalf("nothing should be re-registered, got %q", name)
		return nil
	}
	if got := migrateSoloFileNames(); got != 0 {
		t.Fatalf("migrated=%d, want 0", got)
	}
	for name := range files {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("file %s was disturbed: %v", name, err)
		}
	}
}

func TestMigrateSoloFileNames_TargetExistsConservativeSkip(t *testing.T) {
	dir := t.TempDir()
	// Same account solo-logged in BOTH eras: the shared-namespace file
	// (v0.12.0-0.12.26) collides with the legacy-namespaced file. The
	// migration must NOT overwrite either.
	shared := `{"type":"trae","auth":{"variant":"solo"},"account":{"uid":"dupe"}}`
	legacy := `{"type":"trae","auth":{"variant":"solo"},"account":{"uid":"dupe"}}`
	if err := os.WriteFile(filepath.Join(dir, "trae-dupe.json"), []byte(shared), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trae-solo-cn-dupe.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	origDir, origSave := authDirCache.dir, healSaveFn
	defer func() { authDirCache.dir, healSaveFn = origDir, origSave }()
	authDirCache.dir = dir
	healSaveFn = func(name string, raw []byte) error { return nil }
	if got := migrateSoloFileNames(); got != 0 {
		t.Fatalf("migrated=%d, want 0 (conservative skip)", got)
	}
	gotShared, errShared := os.ReadFile(filepath.Join(dir, "trae-dupe.json"))
	gotLegacy, errLegacy := os.ReadFile(filepath.Join(dir, "trae-solo-cn-dupe.json"))
	if errShared != nil || errLegacy != nil || string(gotShared) != shared || string(gotLegacy) != legacy {
		t.Fatal("conflicting files must both survive untouched")
	}
}

func TestMigrateSoloFileNames_DerivesUIDFromFileName(t *testing.T) {
	dir := t.TempDir()
	// v0.12.0-0.12.4 files may lack account.uid — derive it from the name.
	raw := `{"type":"trae","auth":{"variant":"solo"},"account":{}}`
	if err := os.WriteFile(filepath.Join(dir, "trae-nameless-uid.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	origDir, origSave := authDirCache.dir, healSaveFn
	defer func() { authDirCache.dir, healSaveFn = origDir, origSave }()
	authDirCache.dir = dir
	healSaveFn = func(name string, raw []byte) error { return nil }
	if got := migrateSoloFileNames(); got != 1 {
		t.Fatalf("migrated=%d, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "trae-solo-cn-nameless-uid.json")); err != nil {
		t.Fatalf("uid not derived from file name: %v", err)
	}
}
