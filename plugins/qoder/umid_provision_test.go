// umid_provision_test.go — v0.8.60: hermetic coverage for the self-
// provisioning of the official UMID identity bridge (umid_provision.go).
//
// Live context (2026-10-08, u673e7fcc Intl): the same account that could
// never see the daily CLAIM_BENEFIT row under a derived identity saw it and
// claimed +100 credits the moment a real runtime-info identity was
// presented. These tests pin the extraction pipeline (registry → tarball →
// bundle → base64 scan → magic identify → size heuristic → atomic install)
// so the live path keeps working without ever touching the network in CI:
// TestMain forces QD_UMID_AUTO=0 package-wide and provisioning tests opt
// back in with t.Setenv against a stub registry.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"
)

// TestMain keeps the whole test binary hermetic: no npm egress, no bridge
// spawns from provisioning. Tests that exercise provisioning opt in via
// t.Setenv("QD_UMID_AUTO", "1") and stub servers only. Live runs
// (-tags live + QD_TOKEN) skip the guard so they exercise the REAL
// provisioning path end to end.
func TestMain(m *testing.M) {
	if os.Getenv("QD_TOKEN") == "" {
		os.Setenv("QD_UMID_AUTO", "0")
	}
	os.Exit(m.Run())
}

// umidFakeBlob builds a blob with the real magic/header shape of one label.
func umidFakeBlob(label string, size int) []byte {
	b := make([]byte, size)
	switch label {
	case "elf-x86_64", "elf-aarch64":
		if len(b) < 4 {
			return b
		}
		b[0], b[1], b[2], b[3] = 0x7f, 'E', 'L', 'F'
		if len(b) < 20 {
			return b // deliberately truncated: identify must answer "unknown"
		}
		b[4], b[5] = 2, 1 // 64-bit little-endian
		if label == "elf-x86_64" {
			b[18], b[19] = 0x3e, 0
		} else {
			b[18], b[19] = 0xb7, 0
		}
	case "macho-x86_64", "macho-arm64":
		if len(b) < 8 {
			return b
		}
		b[0], b[1], b[2], b[3] = 0xcf, 0xfa, 0xed, 0xfe
		cpu := uint32(0x01000007)
		if label == "macho-arm64" {
			cpu = 0x0100000C
		}
		b[4] = byte(cpu)
		b[5] = byte(cpu >> 8)
		b[6] = byte(cpu >> 16)
		b[7] = byte(cpu >> 24)
	case "pe-x86_64", "pe-aarch64":
		if len(b) < 0x46 {
			return b
		}
		b[0], b[1] = 'M', 'Z'
		b[0x3C] = 0x40 // e_lfanew
		copy(b[0x40:0x44], []byte{'P', 'E', 0, 0})
		machine := uint16(0x8664)
		if label == "pe-aarch64" {
			machine = 0xAA64
		}
		b[0x44] = byte(machine)
		b[0x45] = byte(machine >> 8)
	case "wasm":
		if len(b) < 4 {
			return b
		}
		b[0], b[1], b[2], b[3] = 0x00, 'a', 's', 'm'
	}
	return b
}

func TestUmidBlobLabelFor(t *testing.T) {
	cases := []struct {
		goos, arch, want string
	}{
		{"linux", "amd64", "elf-x86_64"},
		{"linux", "arm64", "elf-aarch64"},
		{"darwin", "amd64", "macho-x86_64"},
		{"darwin", "arm64", "macho-arm64"},
		{"windows", "amd64", "pe-x86_64"},
		{"windows", "arm64", "pe-aarch64"},
		{"linux", "386", ""},   // never shipped
		{"js", "wasm", ""},     // never shipped
		{"sunos", "amd64", ""}, // nonsense
	}
	for _, c := range cases {
		if got := umidBlobLabelFor(c.goos, c.arch); got != c.want {
			t.Errorf("umidBlobLabelFor(%s/%s) = %q, want %q", c.goos, c.arch, got, c.want)
		}
	}
	if got := umidBlobLabelFor(runtime.GOOS, runtime.GOARCH); got == "" {
		t.Errorf("current platform %s/%s has no bridge component — provisioner would be dead code here", runtime.GOOS, runtime.GOARCH)
	}
}

func TestIdentifyUmidBlob(t *testing.T) {
	cases := []struct {
		label string
		size  int
		want  string
	}{
		{"elf-x86_64", 4096, "elf-x86_64"},
		{"elf-aarch64", 4096, "elf-aarch64"},
		{"macho-x86_64", 4096, "macho-x86_64"},
		{"macho-arm64", 4096, "macho-arm64"},
		{"pe-x86_64", 4096, "pe-x86_64"},
		{"pe-aarch64", 4096, "pe-aarch64"},
		{"wasm", 4096, "wasm"},
		{"none", 4096, "unknown"},
		{"elf-x86_64", 8, "unknown"}, // too short for the full header read
	}
	for _, c := range cases {
		if got := identifyUmidBlob(umidFakeBlob(c.label, c.size)); got != c.want {
			t.Errorf("identifyUmidBlob(%s/%dB) = %q, want %q", c.label, c.size, got, c.want)
		}
	}
}

func TestScanAndSelectUmidBlob(t *testing.T) {
	big1 := umidFakeBlob("elf-x86_64", 420*1024)  // in range
	big2 := umidFakeBlob("elf-x86_64", 700*1024)  // in range, larger
	small := umidFakeBlob("elf-x86_64", 300*1024) // below the heuristic floor
	wasm := umidFakeBlob("wasm", 64*1024)

	var buf bytes.Buffer
	buf.WriteString("const x=1;const a=\"")
	buf.WriteString(base64.StdEncoding.EncodeToString(big1))
	buf.WriteString("\";const b='")
	buf.WriteString(base64.StdEncoding.EncodeToString(small))
	buf.WriteString("';const c=\"")
	buf.WriteString(base64.StdEncoding.EncodeToString(wasm))
	buf.WriteString("\";const d=\"")
	buf.WriteString(base64.StdEncoding.EncodeToString(big2))
	buf.WriteString("\";const e=\"not-base64-!!-skipped\";")

	blobs := scanUmidBlobs(buf.String())
	if len(blobs) != 4 {
		t.Fatalf("scanUmidBlobs decoded %d blobs, want 4 (invalid literal skipped)", len(blobs))
	}
	got := selectUmidBlob(blobs, "elf-x86_64")
	if got == nil || len(got) != len(big1) {
		t.Fatalf("selectUmidBlob picked %v bytes, want the in-range smallest (%d)", len(got), len(big1))
	}
	if got := selectUmidBlob(blobs, "pe-x86_64"); got != nil {
		t.Fatalf("selectUmidBlob returned %d bytes for a label that is not in the bundle", len(got))
	}
	if got := selectUmidBlob(blobs, ""); got != nil {
		t.Fatalf("selectUmidBlob with empty label must return nil, got %d bytes", len(got))
	}
}

func TestParseUmidRegistryMeta(t *testing.T) {
	good := map[string]any{
		"dist-tags": map[string]string{"latest": "1.1.65"},
		"versions": map[string]any{
			"1.1.65": map[string]any{
				"dist": map[string]any{
					"tarball":   "https://registry.npmjs.org/x/-/x-1.1.65.tgz",
					"integrity": "sha512-abc==",
				},
			},
		},
	}
	raw, _ := json.Marshal(good)
	meta, err := parseUmidRegistryMeta(raw)
	if err != nil || meta.Version != "1.1.65" || meta.Tarball == "" {
		t.Fatalf("parseUmidRegistryMeta good doc: meta=%+v err=%v", meta, err)
	}
	if _, err := parseUmidRegistryMeta([]byte(`{"dist-tags":{},"versions":{}}`)); err == nil {
		t.Fatal("missing latest must error")
	}
	if _, err := parseUmidRegistryMeta([]byte("not json")); err == nil {
		t.Fatal("non-JSON registry body must error")
	}
}

func TestUmidExtractBundle(t *testing.T) {
	var tgz bytes.Buffer
	zw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(zw)
	addFile := func(name, content string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
		tw.Write([]byte(content))
	}
	addFile("package/package.json", "{\"name\":\"@qoder-ai/qodercli\"}")
	addFile("package/bundle/qoder-worker-runtime.mjs", "export default 1")
	addFile("package/other.txt", "ignore me")
	tw.Close()
	zw.Close()

	text, err := umidExtractBundle(tgz.Bytes())
	if err != nil {
		t.Fatalf("umidExtractBundle: %v", err)
	}
	if text != "export default 1" {
		t.Fatalf("bundle text = %q", text)
	}
	if _, err := umidExtractBundle([]byte("not gzip")); err == nil {
		t.Fatal("non-gzip tarball must error")
	}
}

func TestUmidVerifyIntegrity(t *testing.T) {
	data := []byte("qoder umid payload")
	// sha512 of data, base64 — compute via the same helper chain crypto uses
	if err := umidVerifyIntegrity(data, ""); err != nil {
		t.Fatalf("missing integrity must be skipped, got %v", err)
	}
	if err := umidVerifyIntegrity(data, "md5-junk"); err != nil {
		t.Fatalf("unsupported algorithm must be skipped, got %v", err)
	}
	// correct sha512 line
	sum := sha512Sum64(data)
	if err := umidVerifyIntegrity(data, "sha512-"+base64.StdEncoding.EncodeToString(sum)); err != nil {
		t.Fatalf("correct integrity rejected: %v", err)
	}
	bad := append([]byte{}, sum...)
	bad[0] ^= 0xff
	if err := umidVerifyIntegrity(data, "sha512-"+base64.StdEncoding.EncodeToString(bad)); err == nil {
		t.Fatal("corrupted tarball must fail integrity")
	}
}

func TestProvisionEndToEnd(t *testing.T) {
	t.Setenv("QD_UMID_AUTO", "1")
	want := umidBlobLabelFor(runtime.GOOS, runtime.GOARCH)
	if want == "" {
		t.Skipf("no bridge component for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	blob := umidFakeBlob(want, 420*1024)

	bundle := "const bridge=\"" + base64.StdEncoding.EncodeToString(blob) + "\";"
	var tgz bytes.Buffer
	zw := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "package/bundle/qoder-worker-runtime.mjs", Mode: 0o644, Size: int64(len(bundle))})
	tw.Write([]byte(bundle))
	tw.Close()
	zw.Close()

	metaRaw, _ := json.Marshal(map[string]any{
		"dist-tags": map[string]string{"latest": "9.9.9"},
		"versions": map[string]any{
			"9.9.9": map[string]any{"dist": map[string]any{
				"tarball":   "%TARBALL%",
				"integrity": "sha512-" + base64.StdEncoding.EncodeToString(sha512Sum64(tgz.Bytes())),
			}},
		},
	})

	// tarballURL is filled in after the server starts; the handler only
	// reads it while serving, so plain closure capture is safe.
	var tarballURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/registry":
			w.Write(bytes.ReplaceAll(metaRaw, []byte("%TARBALL%"), []byte(tarballURL)))
		case "/tarball":
			w.Write(tgz.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tarballURL = srv.URL + "/tarball"
	prevURL := umidRegistryURL
	umidRegistryURL = srv.URL + "/registry"
	defer func() { umidRegistryURL = prevURL }()
	resetUmidProvisionState(t)

	exe, err := provisionUmidBridge()
	if err != nil {
		t.Fatalf("provisionUmidBridge: %v", err)
	}
	st, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("provisioned bridge missing: %v", err)
	}
	if st.Mode()&0o111 == 0 {
		t.Fatalf("provisioned bridge not executable: %v", st.Mode())
	}
	onDisk, err := os.ReadFile(exe)
	if err != nil || len(onDisk) != len(blob) {
		t.Fatalf("provisioned bridge bytes mismatch: err=%v len=%d want %d", err, len(onDisk), len(blob))
	}
	// second call must come from the disk cache, not a re-download
	if exe2 := provisionedRuntimeInfo(); exe2 != exe {
		t.Fatalf("provisionedRuntimeInfo cache miss: %q != %q", exe2, exe)
	}
}

func TestProvisionedRuntimeInfoCooldownAndDisable(t *testing.T) {
	t.Setenv("QD_UMID_AUTO", "1")
	fetches := 0
	prevFetch := umidHTTPFetch
	umidHTTPFetch = func(url string, timeout time.Duration) ([]byte, error) {
		fetches++
		return nil, errProvisionStub{msg: "stub registry down"}
	}
	defer func() { umidHTTPFetch = prevFetch }()
	resetUmidProvisionState(t)

	if exe := provisionedRuntimeInfo(); exe != "" {
		t.Fatalf("failed provisioning must fall back to derived, got %q", exe)
	}
	if exe := provisionedRuntimeInfo(); exe != "" {
		t.Fatalf("cooldown retry returned %q", exe)
	}
	if fetches != 1 {
		t.Fatalf("cooldown not honored: %d fetch rounds, want 1 (registry+tarball share one stub)", fetches)
	}
	if msg := umidProvisionLastError(); msg == "" || !bytes.Contains([]byte(msg), []byte("stub registry down")) {
		t.Fatalf("provision error not surfaced for diagnostics: %q", msg)
	}

	// disabled flag: no egress at all
	t.Setenv("QD_UMID_AUTO", "0")
	resetUmidProvisionState(t)
	fetches = 0
	if exe := provisionedRuntimeInfo(); exe != "" {
		t.Fatalf("disabled provisioning must return empty, got %q", exe)
	}
	if fetches != 0 {
		t.Fatalf("disabled provisioning hit the network %d times", fetches)
	}
}

// errProvisionStub is a test error type with a stable message.
type errProvisionStub struct{ msg string }

func (e errProvisionStub) Error() string { return e.msg }

// resetUmidProvisionState points the provisioner at a fresh temp dir and
// clears the in-process outcome cache; restored via t.Cleanup.
func resetUmidProvisionState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prevOverride := umidStateDirOverride
	umidStateDirOverride = func() string { return dir }
	t.Cleanup(func() {
		umidStateDirOverride = prevOverride
		umidProvisionStateMu.Lock()
		umidProvisionPath = ""
		umidProvisionErr = ""
		umidProvisionAt = time.Time{}
		umidProvisionStateMu.Unlock()
	})
	umidProvisionStateMu.Lock()
	umidProvisionPath = ""
	umidProvisionErr = ""
	umidProvisionAt = time.Time{}
	umidProvisionStateMu.Unlock()
}

// sha512Sum64 is the test-side digest helper for npm-style integrity lines.
func sha512Sum64(data []byte) []byte {
	sum := sha512.Sum512(data)
	return sum[:]
}
