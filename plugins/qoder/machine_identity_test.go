// machine_identity_test.go — v0.8.44: the campaigns platform filters
// device-targeted rows (daily 100 Credits, the +1800 Pro pack) on the
// Cosy-Machine* headers. The simulated identity replays the exact shape
// the official runtime-info bridge emits (extracted from the official CN
// CLI 1.1.65 bundle, container-run verified): "P1gA"-prefixed 88-char
// base64url token (3f 58 00 header + 63 body bytes), 18-hex machineType
// with the "91" marker, 18-hex machineCode with the "00" marker — stable
// per uid, distinct across uids. These tests pin that format iron-law and
// the header wiring.
package main

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func derivedTestHeaders(t *testing.T) *machineIdentity {
	t.Helper()
	// Force the derived path on every platform: QD_NATIVE_IDENTITY=0 makes
	// runtimeInfoExePath return "" without touching the filesystem, so the
	// suite never spawns the official exe (Windows dev hosts included).
	t.Setenv("QD_NATIVE_IDENTITY", "0")
	id := machineIdentityFor("cn", "", false)
	if id.Source != "derived" {
		t.Fatalf("source = %q, want derived on a host without the official client", id.Source)
	}
	return &id
}

// TestSimulatedIdentityOfficialFormat (v0.8.44): every field of the derived
// identity must match the official bridge's observed output shape exactly —
// these are the format iron-laws the server filters on.
func TestSimulatedIdentityOfficialFormat(t *testing.T) {
	id := derivedTestHeaders(t)
	// machineToken: exactly 88 base64url chars, "P1gA" prefix (fixed
	// 3f 58 00 header), decoding back to 66 bytes starting 3f 58 00.
	if len(id.MachineToken) != 88 {
		t.Fatalf("MachineToken length = %d, want 88 (official shape)", len(id.MachineToken))
	}
	if !strings.HasPrefix(id.MachineToken, "P1gA") {
		t.Fatalf("MachineToken = %q, want P1gA prefix (3f5800 header)", id.MachineToken)
	}
	raw, err := base64.RawURLEncoding.DecodeString(id.MachineToken)
	if err != nil {
		t.Fatalf("MachineToken not base64url: %v", err)
	}
	if len(raw) != 66 || raw[0] != 0x3f || raw[1] != 0x58 || raw[2] != 0x00 {
		t.Fatalf("MachineToken body = %d bytes starting %x, want 66 bytes starting 3f5800", len(raw), raw[:3])
	}
	// machineType: 18 hex, 8hex + "91" + 8hex.
	if len(id.MachineType) != 18 || !regexp.MustCompile(`^[0-9a-f]{8}91[0-9a-f]{8}$`).MatchString(id.MachineType) {
		t.Fatalf("MachineType = %q, want 8hex+91+8hex (18 chars)", id.MachineType)
	}
	// machineCode: 18 hex, 8hex + "00" + 8hex.
	if len(id.MachineCode) != 18 || !regexp.MustCompile(`^[0-9a-f]{8}00[0-9a-f]{8}$`).MatchString(id.MachineCode) {
		t.Fatalf("MachineCode = %q, want 8hex+00+8hex (18 chars)", id.MachineCode)
	}
	if id.MachineOS == "" || id.MachineHostname == "" {
		t.Fatalf("os/hostname must never be empty: %q / %q", id.MachineOS, id.MachineHostname)
	}
}

// TestSimulatedIdentityPerUIDStableAndIsolated (v0.8.44): the same uid
// always derives the same identity (no per-call rotation); distinct uids
// never share a pseudo-device (upstream per-person dedup would otherwise
// hide the daily row from the losing accounts).
func TestSimulatedIdentityPerUIDStableAndIsolated(t *testing.T) {
	a1 := derivedMachineIdentity("u-alice")
	a2 := derivedMachineIdentity("u-alice")
	b := derivedMachineIdentity("u-bob")
	if a1 != a2 {
		t.Fatalf("derivation not stable per uid: %+v vs %+v", a1, a2)
	}
	if a1.MachineToken == b.MachineToken || a1.MachineType == b.MachineType || a1.MachineCode == b.MachineCode {
		t.Fatalf("identity collides across uids: alice=%+v bob=%+v", a1, b)
	}
	// Empty uid stays deterministic too (single-account convenience).
	e1, e2 := derivedMachineIdentity(""), derivedMachineIdentity("")
	if e1 != e2 {
		t.Fatalf("empty-uid derivation not stable")
	}
}

// TestAttachMachineIdentityHeadersWiring: every non-empty identity field
// lands on the request as its Cosy-Machine* header. v0.8.48: headers are
// ALWAYS attached regardless of Source (real runtime-info OR derived) —
// sending simulated headers is strictly >= sending none, because the
// server's browser path (no machine headers) ALSO filters CLAIM_BENEFIT
// rows. The live credential test on 2026-10-03 proved that real
// runtime-info headers return the full campaigns list (including the
// daily 100-Credits CLAIM_BENEFIT row), while NO headers return only
// VIEW_DETAILS rows.
func TestAttachMachineIdentityHeadersWiring(t *testing.T) {
	// Native identity — all headers MUST attach.
	native := &machineIdentity{
		MachineID:       "2c90da275cd6c2aec16a623824b5e232",
		MachineToken:    "P1gA0rfqqI_HmjFnCJXCYVgzLJSpddbvuyLWJeeA7xfMFKDgrKAvn4Kv3WohngV_c7emWuRiMrQz_EjCvrKwoxsw",
		MachineType:     "03dec3a691bfd4c412",
		MachineCode:     "076dbecc00e397e0af",
		MachineOS:       "x86_64_linux",
		MachineHostname: "qoder-host",
		Source:          identitySourceNative,
	}
	req, _ := http.NewRequest(http.MethodGet, "http://upstream/sash/api/v1/me/campaigns", nil)
	attachMachineIdentityHeaders(req, native)
	for name, want := range map[string]string{
		"Cosy-MachineId":       native.MachineID,
		"Cosy-MachineToken":    native.MachineToken,
		"Cosy-MachineType":     native.MachineType,
		"Cosy-MachineCode":     native.MachineCode,
		"Cosy-MachineOS":       native.MachineOS,
		"Cosy-MachineHostname": native.MachineHostname,
	} {
		if got := req.Header.Get(name); got != want {
			t.Fatalf("native: %s = %q, want %q", name, got, want)
		}
	}

	// Derived identity — v0.8.48: headers ALSO attach (sending simulated
	// headers is strictly >= sending none). The server may still filter
	// device-targeted rows for simulated identities, but at least we
	// don't lie about being a browser.
	derived := derivedTestHeaders(t)
	if derived.Source != "derived" {
		t.Fatalf("derived source = %q", derived.Source)
	}
	req2, _ := http.NewRequest(http.MethodGet, "http://upstream/sash/api/v1/me/campaigns", nil)
	attachMachineIdentityHeaders(req2, derived)
	for name, want := range map[string]string{
		"Cosy-MachineId":       derived.MachineID,
		"Cosy-MachineToken":    derived.MachineToken,
		"Cosy-MachineType":     derived.MachineType,
		"Cosy-MachineCode":     derived.MachineCode,
		"Cosy-MachineOS":       derived.MachineOS,
		"Cosy-MachineHostname": derived.MachineHostname,
	} {
		if got := req2.Header.Get(name); got != want {
			t.Fatalf("derived: %s = %q, want %q (v0.8.48: always attach)", name, got, want)
		}
	}
}

// TestMachineIdentityOverrideShortCircuits: the test/native seam bypasses
// cache and exe discovery entirely.
func TestMachineIdentityOverrideShortCircuits(t *testing.T) {
	native := &machineIdentity{
		MachineID: "mid", MachineToken: "mtok", MachineType: "mtype",
		MachineCode: "mcode", MachineOS: "x86_64_win32",
		MachineHostname: "DESKTOP-QODER", Source: "runtime-info",
		VMIsVM: true, VMBrand: "Hyper-V", VMScore: 88,
	}
	machineIdentityOverride = native
	t.Cleanup(func() { machineIdentityOverride = nil })
	got := machineIdentityFor("cn", "whatever-uid", false)
	if got != *native {
		t.Fatalf("override ignored: %+v", got)
	}
	hint := machineIdentityHint(&storedAuth{Auth: storedTokens{Region: "cn"}, Account: storedAccount{UID: "u"}})
	for _, want := range []string{"runtime-info", "虚拟机", "Hyper-V"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("native VM hint missing %q: %q", want, hint)
		}
	}
}

// TestMachineIdentityHintDerivedDisclosesSimulatedSource (v0.8.44): the
// simulated-identity hint must name the source honestly (官方格式模拟身份),
// carry the format credentials (P1gA / 91/00 markers) and keep the real
// bridge escape hatch (QD_UMID_BIN) visible for operators who can drop
// the official binary.
func TestMachineIdentityHintDerivedDisclosesSimulatedSource(t *testing.T) {
	_ = derivedTestHeaders(t)
	hint := machineIdentityHint(&storedAuth{Auth: storedTokens{Region: "cn"}, Account: storedAccount{UID: "u"}})
	for _, want := range []string{"模拟身份", "P1gA", "QD_UMID_BIN"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("derived hint missing %q: %q", want, hint)
		}
	}
}

// runtimeInfoArgsDialectTable (v0.8.43, official-package forensics): the
// spawn dialect must byte-match the client bundle's IUt function —
// win32/darwin spawn `runtime-info prod --account-stdin` with the account
// JSON on stdin; linux spawns `runtime-info prod` with stdin ignored.
func TestRuntimeInfoArgsDialectTable(t *testing.T) {
	for goos, want := range map[string]struct {
		args      []string
		withStdin bool
	}{
		"windows": {[]string{"prod", "--account-stdin"}, true},
		"darwin":  {[]string{"prod", "--account-stdin"}, true},
		"linux":   {[]string{"prod"}, false},
	} {
		args, withStdin := runtimeInfoArgs(goos)
		if strings.Join(args, " ") != strings.Join(want.args, " ") || withStdin != want.withStdin {
			t.Fatalf("runtimeInfoArgs(%q) = %v,%v want %v,%v", goos, args, withStdin, want.args, want.withStdin)
		}
	}
}

// TestQDUmidBinNativeBridgeEndToEnd (v0.8.43): QD_UMID_BIN lets a container
// deployment point the plugin at the official bridge binary anywhere on
// disk. A fake executable reproduces the official runtime-info contract
// (stderr noise + one JSON stdout line) and the identity must come back as
// Source=runtime-info with the JSON's values, machine-level cached per
// region. Linux-only: the fake is a POSIX shell script.
func TestQDUmidBinNativeBridgeEndToEnd(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("fake bridge is a shell script; skipping on %s", runtime.GOOS)
	}
	t.Setenv("QD_NATIVE_IDENTITY", "")
	dir := t.TempDir()
	exe := filepath.Join(dir, "runtime-info")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + filepath.Join(dir, "args.txt") + "\n" +
		"echo 'open:: No such file or directory' >&2\n" +
		"echo '{\"machineToken\":\"P1gAE2ETestToken-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"machineType\":\"38de1ce79191f25e8b\",\"machineCode\":\"a2452fe300901c05e5\",\"vmInfo\":{\"isVm\":false,\"brand\":\"None\",\"percentage\":0,\"vmTypeCode\":91}}'\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bridge: %v", err)
	}
	t.Setenv("QD_UMID_BIN", exe)

	region := "umid-e2e-cn"
	id := machineIdentityFor(region, "u-e2e", false)
	if id.Source != identitySourceNative {
		t.Fatalf("source = %q, want %q (QD_UMID_BIN bridge must win)", id.Source, identitySourceNative)
	}
	if !strings.HasPrefix(id.MachineToken, "P1gAE2E") || id.MachineType != "38de1ce79191f25e8b" || id.MachineCode != "a2452fe300901c05e5" {
		t.Fatalf("identity values not mapped from bridge output: %+v", id)
	}
	// Official dialect on linux: the environment argument only — no
	// --account-stdin flag, stdin ignored (bundle IUt).
	argsRaw, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatalf("fake bridge args missing: %v", err)
	}
	if got := strings.TrimSpace(string(argsRaw)); got != "prod" {
		t.Fatalf("linux dialect args = %q, want %q", got, "prod")
	}
	// Machine-level cache: the same region returns the SAME identity
	// without re-running the bridge (30-minute TTL, native only).
	again := machineIdentityFor(region, "u-other", false)
	if again != id {
		t.Fatalf("native identity not region-cached: %+v vs %+v", again, id)
	}
	machineIdentityCache.Delete(region)
}

// TestMachineIdentityHintDerivedMentionsEscapeHatch (v0.8.44): the
// simulated-identity hint keeps the QD_UMID_BIN override and the official
// bridge artifact name visible so container operators can still opt into a
// real machine identity when one is available.
func TestMachineIdentityHintDerivedMentionsEscapeHatch(t *testing.T) {
	_ = derivedTestHeaders(t)
	hint := machineIdentityHint(&storedAuth{Auth: storedTokens{Region: "cn"}, Account: storedAccount{UID: "u"}})
	for _, want := range []string{"QD_UMID_BIN", "runtime-info"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("derived hint missing %q: %q", want, hint)
		}
	}
}

// TestOfficialBridgeGoldenSamples (v0.8.44): the format iron-laws are not
// invented — they were read off nine live runs of the official bridge
// (extracted from the official CN CLI 1.1.65 bundle; sha256 e30b307e...,
// byte-identical to the IDE deb's runtime-info). The samples below are the
// exact observed machineToken/machineType/machineCode values with their
// random bodies kept verbatim: this test re-pins the simulator's shape
// against the official reality whenever the format test above changes.
func TestOfficialBridgeGoldenSamples(t *testing.T) {
	type sample struct{ token, mtype, code string }
	samples := []sample{
		{"P1gAyx0-JzEkqouxxfjI9eXMW906sx9fDiFDdvMsL02lD07MjoaafK-QhBp5yaRUaARDuvFs0XLo8xTmMejXj-WE", "c5233f7091a5566047", "ac2b4cba0028649952"},
		{"P1gAiQvKVNlSwvoHGJsIeFULqT6GYj-7mdb2AtwKNiu8liWYAUr01eTmCgikqbcceg0tYdAM-ndEOI8aTGdKYhGr", "189d27fb915b08d163", "af8825110054463a72"},
		{"P1gA-ZdWWaXf-10_9bQ6m7EC-FEHoRerwzj183jZEFZYgwLg-T3E6Hj60uPA3X4_hZO_6Thauwpd8I-s9r3RXNmC", "cb71c2be915cdf6939", "3444cffd0070dd58b4"},
		{"P1gASXhSGOrYR_xEAXhMA4v0K_MOu95-jBM2N6ZGJEIILyP0Y_jKGLDR7r2vYlYytmYzZjajf7ItK435kiLYlvb5", "55f2c19b913284ab54", "2189ac6000c41c4316"},
		{"P1gADW8JamdjLtbF3gghQvB526Xo5itY2_5hOc5Mknrcrg9285MicRHzUVbiSMKAhwUGbiJaqffdbob2KvewwRU1", "f3aba522915256b72e", "8c091c81003c80b0bb"},
		{"P1gA8SH5Ph0YaUcMqK_me2ZrKfWf-ykDPp_w94B5bXpi6bCokJUO7NYLKSAmwhu3FuFLITW32YKpTnL7VsN-OXRW", "87d2c61491e98d6686", "a2870d1600d7bdfb07"},
		{"P1gAx55X-hXeqGY_J3tqKcvC_eDZf6Y_dj3_IUZqA3Ezge8gO9mcrHIkYKl1Wz9GvsQUQT6HZgRSMVBT8ectgpeB", "eaa927a291988d10c2", "1f7541a5000b51c0eb"},
		{"P1gA2WPbVevTTyS8ThOFao6B6Uc1cgv6kjjhHTv6kEAnkTgwtDAF-AHBaYViBCG2-vovw-BOKqV0z0W51kHvsJEc", "2d894e019189ca8910", "0818377300b0dd913b"},
		{"P1gAbKUEUrpMTSMQEgfixiI-FTwbveDcmDY7PowA2WrpA4xD4UqLTjqgcT_kP4q2c4IRgjr72AedOhLFx2DcN8c1", "12c9a38591cfb03c19", "8ea2e9ce0047c18a94"},
	}
	for i, s := range samples {
		if len(s.token) != 88 || !strings.HasPrefix(s.token, "P1gA") {
			t.Fatalf("sample %d token shape drift: %q", i, s.token)
		}
		if len(s.mtype) != 18 || s.mtype[8:10] != "91" {
			t.Fatalf("sample %d machineType shape drift: %q", i, s.mtype)
		}
		if len(s.code) != 18 || s.code[8:10] != "00" {
			t.Fatalf("sample %d machineCode shape drift: %q", i, s.code)
		}
	}
	// The simulated values must be shape-identical to every golden sample.
	sim := derivedMachineIdentity("golden-uid")
	if len(sim.MachineToken) != 88 || !strings.HasPrefix(sim.MachineToken, "P1gA") ||
		len(sim.MachineType) != 18 || sim.MachineType[8:10] != "91" ||
		len(sim.MachineCode) != 18 || sim.MachineCode[8:10] != "00" {
		t.Fatalf("simulated identity shape drifts from official samples: %+v", sim)
	}
}
