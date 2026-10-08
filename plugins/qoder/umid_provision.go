// umid_provision.go — v0.8.60: self-provisioning of the official UMID
// identity bridge (runtime-info) for hosts without the desktop client.
//
// Why (live-proven 2026-10-08, account u673e7fcc Intl): device-targeted
// campaign rows (the daily "100 Credits" CLAIM_BENEFIT) are gated in TWO
// places on the authenticity of the Cosy-Machine* identity:
//
//  1. the campaigns LIST drops the row for derived/simulated identities —
//     the same account went invisible → visible the moment a real
//     runtime-info identity was presented;
//  2. the claim POST needs a device anchor for the same-person dedup — a
//     header-less claim 503s with SAME_PERSON_DEPENDENCY_UNAVAILABLE and a
//     derived-identity claim 503s with RISK_DEPENDENCY_UNAVAILABLE
//     (v0.8.53, live 2026-10-04 CN). Only a real bridge output passed both.
//
// Source (qoder2api-hub _install_umid.py, cross-verified): the official npm
// package @qoder-ai/qodercli embeds all five platform builds of the bridge
// as long base64 string literals inside package/bundle/
// qoder-worker-runtime.mjs. Extraction is deterministic:
//
//	registry metadata → dist.tarball (+ sha512 integrity)
//	tarball → bundle member → scan base64 literals (≥4000 chars)
//	magic-identify (ELF e_machine / Mach-O cputype / PE machine)
//	size heuristic 400KB..2MB (the 7.3MB PE blob is a different module)
//	atomic install <state>/umid/runtime-info[.exe], chmod 0755
//
// Escape hatches: QD_UMID_BIN (explicit binary) still wins over all of
// this; QD_UMID_AUTO=0/false/no disables provisioning entirely. Failures
// fall back to the derived identity and retry after a 6h cooldown.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// umidRegistryURLDefault is the official npm registry location of the
	// CLI package that embeds the identity bridge components.
	umidRegistryURLDefault = "https://registry.npmjs.org/@qoder-ai/qodercli"
	umidBundleMember       = "package/bundle/qoder-worker-runtime.mjs"
	umidBlobMinChars       = 4000
	umidBlobSizeMin        = 400 * 1024
	umidBlobSizeMax        = 2 * 1024 * 1024
	umidTarballCap         = 80 << 20
	umidBundleCap          = 64 << 20
	umidFetchTimeout       = 120 * time.Second
	umidRetryCooldown      = 6 * time.Hour
	umidProvisionerUAT     = "cpa-multi-plugins qoder umid-provision"
)

var (
	// umidRegistryURL is a var so tests can point it at a stub server.
	umidRegistryURL = umidRegistryURLDefault

	// umidProvisionRunMu serializes provisioning attempts across regions/
	// goroutines; umidProvisionStateMu guards the cached outcome below.
	umidProvisionRunMu sync.Mutex

	umidProvisionStateMu sync.Mutex
	umidProvisionPath    string    // last successful install (process cache)
	umidProvisionErr     string    // last failure text ("" when none)
	umidProvisionAt      time.Time // last attempt (cooldown anchor)

	// umidHTTPFetch is the single network touchpoint (test stub point).
	umidHTTPFetch = umidHTTPFetchProd

	// umidStateDirOverride is nil in production; tests point it at a t.TempDir.
	umidStateDirOverride func() string
)

// umidProvisionEnabled reports whether auto-provisioning is on. Default on;
// QD_UMID_AUTO=0/false/no turns it off (operators who ship their own binary
// via QD_UMID_BIN or an official client install and want zero egress).
func umidProvisionEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QD_UMID_AUTO"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// umidProvisionDir is where the plugin keeps its self-provisioned bridge.
func umidProvisionDir() string {
	if umidStateDirOverride != nil {
		return umidStateDirOverride()
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(os.TempDir(), ".cpa-multi-plugins", "umid")
	}
	return filepath.Join(home, ".cpa-multi-plugins", "umid")
}

func umidProvisionedExePath() string {
	name := "runtime-info"
	if runtime.GOOS == "windows" {
		name = "runtime-info.exe"
	}
	return filepath.Join(umidProvisionDir(), name)
}

// umidProvisionLastError returns the last provisioning failure, if any.
func umidProvisionLastError() string {
	umidProvisionStateMu.Lock()
	defer umidProvisionStateMu.Unlock()
	return umidProvisionErr
}

// provisionedRuntimeInfo returns the plugin-managed official bridge,
// provisioning it on first use, or "" when unavailable (derived fallback
// stays in charge). Cached on disk forever and in-process for the lifetime;
// failed attempts retry after umidRetryCooldown.
func provisionedRuntimeInfo() string {
	if !umidProvisionEnabled() {
		return ""
	}
	umidProvisionStateMu.Lock()
	p := umidProvisionPath
	umidProvisionStateMu.Unlock()
	if p != "" {
		return p
	}
	// fast path: a previous run of this binary already provisioned it
	disk := umidProvisionedExePath()
	if st, err := os.Stat(disk); err == nil && !st.IsDir() && st.Size() > umidBlobSizeMin {
		umidProvisionStateMu.Lock()
		umidProvisionPath = disk
		umidProvisionStateMu.Unlock()
		return disk
	}

	umidProvisionRunMu.Lock()
	defer umidProvisionRunMu.Unlock()
	// re-check under the run lock: a concurrent attempt may have finished
	if st, err := os.Stat(disk); err == nil && !st.IsDir() && st.Size() > umidBlobSizeMin {
		umidProvisionStateMu.Lock()
		umidProvisionPath = disk
		umidProvisionErr = ""
		umidProvisionStateMu.Unlock()
		return disk
	}
	umidProvisionStateMu.Lock()
	if !umidProvisionAt.IsZero() && time.Since(umidProvisionAt) < umidRetryCooldown {
		umidProvisionStateMu.Unlock()
		return ""
	}
	umidProvisionStateMu.Unlock()

	exe, err := provisionUmidBridge()
	umidProvisionStateMu.Lock()
	umidProvisionAt = time.Now()
	if err != nil {
		umidProvisionErr = err.Error()
	} else {
		umidProvisionErr = ""
		umidProvisionPath = exe
	}
	umidProvisionStateMu.Unlock()
	if err != nil {
		return ""
	}
	return exe
}

// umidRegistryMeta is the slice of npm registry metadata we need.
type umidRegistryMeta struct {
	Version   string
	Tarball   string
	Integrity string
}

// parseUmidRegistryMeta picks dist-tags.latest out of the full registry doc.
func parseUmidRegistryMeta(body []byte) (umidRegistryMeta, error) {
	var doc struct {
		DistTags map[string]string `json:"dist-tags"`
		Versions map[string]struct {
			Dist struct {
				Tarball   string `json:"tarball"`
				Integrity string `json:"integrity"`
			} `json:"dist"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return umidRegistryMeta{}, fmt.Errorf("registry metadata not JSON: %w", err)
	}
	latest := strings.TrimSpace(doc.DistTags["latest"])
	entry, ok := doc.Versions[latest]
	if latest == "" || !ok || strings.TrimSpace(entry.Dist.Tarball) == "" {
		return umidRegistryMeta{}, errors.New("registry metadata missing dist-tags.latest tarball")
	}
	return umidRegistryMeta{
		Version:   latest,
		Tarball:   entry.Dist.Tarball,
		Integrity: entry.Dist.Integrity,
	}, nil
}

// umidHTTPFetchProd is the production fetcher: one bounded GET, size-capped.
func umidHTTPFetchProd(url string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", umidProvisionerUAT)
	req.Header.Set("Accept", "*/*")
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d for %s", resp.StatusCode, url)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, umidTarballCap+1))
	if err != nil {
		return nil, err
	}
	if len(body) > umidTarballCap {
		return nil, errors.New("download exceeds size cap")
	}
	return body, nil
}

// umidVerifyIntegrity checks the npm sha512-<base64> integrity line.
// Missing/unsupported integrity returns nil (skip — same as the hub tool).
func umidVerifyIntegrity(data []byte, integrity string) error {
	algo, b64, ok := strings.Cut(strings.TrimSpace(integrity), "-")
	if !ok || algo != "sha512" || b64 == "" {
		return nil
	}
	want, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil // malformed integrity line — skip rather than block
	}
	got := sha512.Sum512(data)
	if !bytes.Equal(got[:], want) {
		return errors.New("npm tarball sha512 integrity mismatch")
	}
	return nil
}

// umidExtractBundle pulls the worker-runtime bundle text out of the tarball.
func umidExtractBundle(tgz []byte) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return "", fmt.Errorf("tarball not gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("tarball has no %s", umidBundleMember)
		}
		if err != nil {
			return "", err
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name != umidBundleMember {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, umidBundleCap+1))
		if err != nil {
			return "", err
		}
		if len(raw) > umidBundleCap {
			return "", errors.New("bundle exceeds size cap")
		}
		return string(raw), nil
	}
}

// Go's regexp caps repeat counts at 1000, so the pattern below matches
// runs of 1000+ base64 chars and scanUmidBlobs filters to the real
// umidBlobMinChars threshold (the hub tool's 4000) explicitly. Native
// bridge components are ~0.5-1MB encoded, far above either bound.
var umidBlobRe = regexp.MustCompile(`['"]([A-Za-z0-9+/=]{1000,})['"]`)

// scanUmidBlobs decodes every long base64 literal in the bundle text.
func scanUmidBlobs(text string) [][]byte {
	matches := umidBlobRe.FindAllStringSubmatch(text, -1)
	out := make([][]byte, 0, len(matches))
	for _, m := range matches {
		if len(m[1]) < umidBlobMinChars {
			continue
		}
		if b, err := base64.StdEncoding.DecodeString(m[1]); err == nil {
			out = append(out, b)
			continue
		}
		if b, err := base64.RawStdEncoding.DecodeString(m[1]); err == nil {
			out = append(out, b)
		}
	}
	return out
}

// umidBlobLabelFor maps GOOS/GOARCH onto the embedded component label.
// Unsupported combinations return "" (provisioning stays off for them).
func umidBlobLabelFor(goos, arch string) string {
	switch arch {
	case "amd64":
		switch goos {
		case "linux":
			return "elf-x86_64"
		case "darwin":
			return "macho-x86_64"
		case "windows":
			return "pe-x86_64"
		}
	case "arm64":
		switch goos {
		case "linux":
			return "elf-aarch64"
		case "darwin":
			return "macho-arm64"
		case "windows":
			return "pe-aarch64"
		}
	}
	return ""
}

// identifyUmidBlob magic-sniffs one decoded blob.
func identifyUmidBlob(data []byte) string {
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0x00, 'a', 's', 'm'}) {
		return "wasm"
	}
	if len(data) >= 20 && bytes.Equal(data[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		if data[4] != 2 || data[5] != 1 { // 64-bit little-endian only
			return "unknown"
		}
		switch uint16(data[18]) | uint16(data[19])<<8 {
		case 0x3E:
			return "elf-x86_64"
		case 0xB7:
			return "elf-aarch64"
		}
		return "unknown"
	}
	if len(data) >= 8 && bytes.Equal(data[:4], []byte{0xcf, 0xfa, 0xed, 0xfe}) {
		cpu := uint32(data[4]) | uint32(data[5])<<8 | uint32(data[6])<<16 | uint32(data[7])<<24
		switch cpu {
		case 0x01000007:
			return "macho-x86_64"
		case 0x0100000C:
			return "macho-arm64"
		}
		return "unknown"
	}
	if len(data) >= 0x40 && data[0] == 'M' && data[1] == 'Z' {
		off := uint32(data[0x3C]) | uint32(data[0x3D])<<8 | uint32(data[0x3E])<<16 | uint32(data[0x3F])<<24
		if int(off)+6 <= len(data) && bytes.Equal(data[off:off+4], []byte{'P', 'E', 0, 0}) {
			machine := uint16(data[off+4]) | uint16(data[off+5])<<8
			switch machine {
			case 0x8664:
				return "pe-x86_64"
			case 0xAA64:
				return "pe-aarch64"
			}
		}
	}
	return "unknown"
}

// selectUmidBlob picks the component matching want: magic first, then the
// 400KB..2MB heuristic (real bridges live there; the 7.3MB PE blob is some
// other native module), smallest wins.
func selectUmidBlob(blobs [][]byte, want string) []byte {
	if want == "" {
		return nil
	}
	var best []byte
	for _, b := range blobs {
		if identifyUmidBlob(b) != want {
			continue
		}
		if best == nil {
			best = b
			continue
		}
		inRange := func(b []byte) bool { return len(b) >= umidBlobSizeMin && len(b) <= umidBlobSizeMax }
		switch {
		case inRange(b) && !inRange(best):
			best = b
		case inRange(b) == inRange(best) && len(b) < len(best):
			best = b
		}
	}
	return best
}

// installUmidBridge atomically writes the component and marks it executable.
func installUmidBridge(data []byte) (string, error) {
	dir := umidProvisionDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	target := umidProvisionedExePath()
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return target, nil
}

// provisionUmidBridge runs the full extraction pipeline once.
func provisionUmidBridge() (string, error) {
	want := umidBlobLabelFor(runtime.GOOS, runtime.GOARCH)
	if want == "" {
		return "", fmt.Errorf("no official identity bridge component for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	metaBody, err := umidHTTPFetch(umidRegistryURL, umidFetchTimeout)
	if err != nil {
		return "", fmt.Errorf("npm registry: %w", err)
	}
	meta, err := parseUmidRegistryMeta(metaBody)
	if err != nil {
		return "", err
	}
	tgz, err := umidHTTPFetch(meta.Tarball, umidFetchTimeout)
	if err != nil {
		return "", fmt.Errorf("npm tarball: %w", err)
	}
	if err := umidVerifyIntegrity(tgz, meta.Integrity); err != nil {
		return "", err
	}
	text, err := umidExtractBundle(tgz)
	if err != nil {
		return "", err
	}
	blob := selectUmidBlob(scanUmidBlobs(text), want)
	if blob == nil {
		return "", fmt.Errorf("official bundle carries no %s component", want)
	}
	return installUmidBridge(blob)
}
