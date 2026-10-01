// machine_identity_test.go — v0.8.36: the campaigns platform filters
// device-targeted rows (daily 100 Credits, the +1800 Pro pack) on the
// Cosy-Machine* headers, and the real values come from the official
// client's native bridge runtime-info.exe (qoder2api-hub capture). These
// tests pin the derived fallback's exact hub-parity scheme (stable per-uid,
// no random values that would trip risk control) and the header wiring.
package main

import (
        "crypto/md5"
        "crypto/sha512"
        "encoding/base64"
        "fmt"
        "net/http"
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

// TestDerivedIdentityHubParityValues: the derivation must stay byte-identical
// with the qoder2api-hub capture's scheme (salted md5 / sha512-b64url) —
// same account, same pseudo-device, forever.
func TestDerivedIdentityHubParityValues(t *testing.T) {
        id := derivedTestHeaders(t)
        wantID := fmt.Sprintf("%x", md5.Sum([]byte("machine:")))
        if id.MachineID != wantID {
                t.Fatalf("MachineID = %q, want %q (hub md5(salt:uid))", id.MachineID, wantID)
        }
        wantType := fmt.Sprintf("%x", md5.Sum([]byte("machinetype:")))[:18]
        if id.MachineType != wantType {
                t.Fatalf("MachineType = %q, want %q (hub machinetype[:18])", id.MachineType, wantType)
        }
        wantCode := fmt.Sprintf("%x", md5.Sum([]byte("machinecode:")))
        if id.MachineCode != wantCode {
                t.Fatalf("MachineCode = %q, want %q", id.MachineCode, wantCode)
        }
        sum := sha512.Sum512([]byte("machinetoken:"))
        wantToken := base64.RawURLEncoding.EncodeToString(sum[:])[:43]
        if id.MachineToken != wantToken {
                t.Fatalf("MachineToken = %q, want %q (hub sha512-b64url[:43])", id.MachineToken, wantToken)
        }
        if id.MachineOS == "" || id.MachineHostname == "" {
                t.Fatalf("os/hostname must never be empty: %q / %q", id.MachineOS, id.MachineHostname)
        }
        // Stability: the same uid derives the same identity (no randomness).
        again := derivedMachineIdentity("")
        if again != *id {
                t.Fatalf("derivation is not stable: %+v vs %+v", again, *id)
        }
}

// TestAttachMachineIdentityHeadersWiring: every non-empty identity field
// lands on the request as its Cosy-Machine* header.
func TestAttachMachineIdentityHeadersWiring(t *testing.T) {
        id := derivedTestHeaders(t)
        req, _ := http.NewRequest(http.MethodGet, "http://upstream/sash/api/v1/me/campaigns", nil)
        attachMachineIdentityHeaders(req, id)
        for name, want := range map[string]string{
                "Cosy-MachineId":       id.MachineID,
                "Cosy-MachineToken":    id.MachineToken,
                "Cosy-MachineType":     id.MachineType,
                "Cosy-MachineCode":     id.MachineCode,
                "Cosy-MachineOS":       id.MachineOS,
                "Cosy-MachineHostname": id.MachineHostname,
        } {
                if got := req.Header.Get(name); got != want {
                        t.Fatalf("%s = %q, want %q", name, got, want)
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

// TestMachineIdentityHintDerivedDisclosesFilteringRisk: the derived hint
// must say the official bridge was not found and what that costs.
func TestMachineIdentityHintDerivedDisclosesFilteringRisk(t *testing.T) {
        _ = derivedTestHeaders(t)
        hint := machineIdentityHint(&storedAuth{Auth: storedTokens{Region: "cn"}, Account: storedAccount{UID: "u"}})
        for _, want := range []string{"runtime-info.exe", "设备定向活动", "过滤"} {
                if !strings.Contains(hint, want) {
                        t.Fatalf("derived hint missing %q: %q", want, hint)
                }
        }
}
