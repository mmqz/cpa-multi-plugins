// machine_identity.go implements the campaigns platform's machine-identity
// layer — the missing half of the "打开客户端同步资格" story.
//
// Upstream forensics (v0.8.36, cross-verified against the qoder2api-hub
// capture, https://github.com/shuishuipingan/qoder2api-hub):
//
// The campaigns surface filters DEVICE-TARGETED campaign rows (the daily
// "100 Credits" grant and the one-shot +1800 Pro upgrade pack) on the
// Cosy-Machine* request headers — in TWO independent layers:
//
//  1. Missing desktop headers (Cosy-ClientType/Cosy-Version) → the server
//     answers HTTP 200 with an EMPTY campaign list (no error — v0.8.22
//     already fixed this layer for this plugin).
//  2. Missing or DERIVED machine identity (Cosy-Machine*) → the list comes
//     back silently SHORT: device-targeted rows are dropped for identities
//     the server does not recognize. This is exactly why a healthy account
//     can see no Pro pack row at all — and it is the real mechanism behind
//     the "open the activity once inside the official client first" hint:
//     what the official client contributes is NOT a mysterious session
//     sync, it is its REAL machine identity.
//
// The official desktop client obtains that identity by spawning its native
// risk-identity bridge before every campaigns call:
//
//      <install>/resources/umid/runtime-info.exe prod --account-stdin
//      stdin:  {"account": <uid>}
//      stdout: {"machineToken","machineType","machineCode","vmInfo":{...}}
//
// The identity is MACHINE-level (every account id on the same host returns
// the same values), rotates over time, and briefly-stale values are still
// accepted — so it is cached per region for 30 minutes (hub live-verified:
// a reused identity still answered showCampaign=true 25s+ later; the real
// cost of a forced refresh is a ~3.7s native run).
//
// When the official client is not installed (or QD_NATIVE_IDENTITY=0), the
// identity falls back to a per-uid stable derivation with the same shape —
// honest in the diagnostics about the filtering risk that fallback carries.
package main

import (
        "bytes"
        "context"
        "crypto/md5"
        "crypto/sha512"
        "encoding/base64"
        "encoding/json"
        "fmt"
        "net/http"
        "os"
        "os/exec"
        "path/filepath"
        "runtime"
        "strings"
        "sync"
        "time"
)

// machineIdentity is one machine identity in the shape the campaigns
// platform expects, plus where it came from (diagnostics) and the official
// bridge's virtualization verdict (the upstream explicitly excludes VMs
// from new-user/device-targeted campaigns).
type machineIdentity struct {
        MachineID       string
        MachineToken    string
        MachineType     string
        MachineCode     string
        MachineOS       string
        MachineHostname string
        Source          string // "runtime-info" (native bridge) | "derived"
        VMIsVM          bool
        VMBrand         string
        VMScore         int
}

// machineIdentityTTL mirrors the hub capture: identities rotate, but
// briefly-stale values are still accepted upstream, so a 30-minute cache
// keeps the native bridge out of the hot path without going stale enough
// to get rows filtered.
const machineIdentityTTL = 30 * time.Minute

// runtimeInfoTimeout bounds one native bridge run. The official binary
// takes ~3.7s in the hub capture; 25s leaves generous headroom.
const runtimeInfoTimeout = 25 * time.Second

var (
        // machineIdentityCache maps region → cache entry (the identity is
        // machine-level, so region-level caching is correct).
        machineIdentityCache sync.Map

        // machineIdentityOverride is nil in production; tests set it to inject
        // a fixed identity (e.g. a native-bridge one) without spawning any exe.
        machineIdentityOverride *machineIdentity

        // machineIdentityForceHook is nil in production; tests set it to count
        // forced identity refreshes (the showCampaign=false self-heal).
        machineIdentityForceHook func()
)

type machineIdentityCacheEntry struct {
        at time.Time
        id machineIdentity
}

// machineIdentityFor returns the identity for one region, computing and
// caching it when missing/expired or when force is set. Any failure to run
// the native bridge falls back to the per-uid derivation — the result is
// always usable, only its Source (and row visibility) differs.
func machineIdentityFor(region, uid string, force bool) machineIdentity {
        if machineIdentityOverride != nil {
                return *machineIdentityOverride
        }
        now := time.Now()
        if !force {
                if v, ok := machineIdentityCache.Load(region); ok {
                        if e, ok := v.(machineIdentityCacheEntry); ok && now.Sub(e.at) < machineIdentityTTL {
                                return e.id
                        }
                }
        }
        id := computeMachineIdentity(region, uid)
        machineIdentityCache.Store(region, machineIdentityCacheEntry{at: now, id: id})
        return id
}

// computeMachineIdentity tries the official native bridge first and falls
// back to the stable per-uid derivation.
func computeMachineIdentity(region, uid string) machineIdentity {
        if exe := runtimeInfoExePath(region); exe != "" {
                if id := nativeMachineIdentityFrom(exe, uid); id != nil {
                        return *id
                }
        }
        return derivedMachineIdentity(uid)
}

// nativeMachineIdentityFrom runs the official bridge once and maps its JSON
// onto machineIdentity. Returns nil when anything is missing — a partial
// identity is worse than an honest derivation.
func nativeMachineIdentityFrom(exe, uid string) *machineIdentity {
        data := runRuntimeInfo(exe, uid)
        if data == nil {
                return nil
        }
        token := strings.TrimSpace(strField(data, "machineToken"))
        mtype := strings.TrimSpace(strField(data, "machineType"))
        code := strings.TrimSpace(strField(data, "machineCode"))
        if token == "" || mtype == "" || code == "" {
                return nil
        }
        id := &machineIdentity{
                MachineToken: token,
                MachineType:  mtype,
                MachineCode:  code,
                MachineID:    derivedMachineID(uid, "machine"), // hub parity: native bridge has no machineId slot
                MachineOS:    machineOSString(),
                Source:       "runtime-info",
        }
        id.MachineHostname = machineHostname()
        if vm, ok := data["vmInfo"].(map[string]any); ok {
                id.VMIsVM, _ = vm["isVm"].(bool)
                id.VMBrand, _ = vm["brand"].(string)
                if f, ok := vm["percentage"].(float64); ok {
                        id.VMScore = int(f)
                }
        }
        return id
}

// derivedMachineIdentity builds the per-uid stable fallback (hub-parity
// scheme: salted md5/sha512 one-way derivations, so the same account always
// presents the same pseudo-device and accounts never collide).
func derivedMachineIdentity(uid string) machineIdentity {
        return machineIdentity{
                MachineID:       derivedMachineID(uid, "machine"),
                MachineToken:    derivedMachineToken(uid),
                MachineType:     derivedMachineID(uid, "machinetype")[:18],
                MachineCode:     derivedMachineID(uid, "machinecode"),
                MachineOS:       machineOSString(),
                MachineHostname: machineHostname(),
                Source:          "derived",
        }
}

func derivedMachineID(uid, salt string) string {
        sum := md5.Sum([]byte(salt + ":" + uid))
        return fmt.Sprintf("%x", sum)
}

func derivedMachineToken(uid string) string {
        sum := sha512.Sum512([]byte("machinetoken:" + uid))
        return base64.RawURLEncoding.EncodeToString(sum[:])[:43]
}

// machineOSString mirrors the official desktop's os string on Windows
// ("x86_64_win32", the only platform the official CN client ships its
// bridge for) and degrades honestly elsewhere.
func machineOSString() string {
        switch runtime.GOOS {
        case "windows":
                return "x86_64_win32"
        case "darwin":
                return "x86_64_darwin"
        default:
                return "x86_64_" + runtime.GOOS
        }
}

func machineHostname() string {
        if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
                return strings.TrimSpace(h)
        }
        return "DESKTOP-QODER" // hub's constant, last resort only
}

func strField(m map[string]any, key string) string {
        s, _ := m[key].(string)
        return s
}

// runRuntimeInfo spawns the official bridge exactly the way the desktop
// client does: `runtime-info.exe prod --account-stdin`, one JSON object on
// stdin (trailing space, hub parity), first stdout line is the answer.
func runRuntimeInfo(exe, uid string) map[string]any {
        ctx, cancel := context.WithTimeout(context.Background(), runtimeInfoTimeout)
        defer cancel()
        cmd := exec.CommandContext(ctx, exe, "prod", "--account-stdin")
        cmd.Dir = filepath.Dir(exe)
        payload, _ := json.Marshal(map[string]any{"account": uid})
        cmd.Stdin = bytes.NewReader(append(payload, ' '))
        var out bytes.Buffer
        cmd.Stdout = &out
        cmd.Stderr = nil
        if err := cmd.Run(); err != nil {
                return nil
        }
        line := strings.TrimSpace(out.String())
        if i := strings.IndexByte(line, '\n'); i >= 0 {
                line = strings.TrimSpace(line[:i])
        }
        if line == "" {
                return nil
        }
        var m map[string]any
        if json.Unmarshal([]byte(line), &m) != nil {
                return nil
        }
        return m
}

// runtimeInfoExePath locates the official bridge, or "" when this host
// cannot have one (non-Windows, disabled, client not installed).
//
// Layout (hub capture): the launcher records the real install dir in
// %LOCALAPPDATA%\<Qoder...>\<...Launcher>\state.ini (installDir=...);
// %LOCALAPPDATA%\Programs\<name> is the fallback layout.
func runtimeInfoExePath(region string) string {
        if runtime.GOOS != "windows" {
                return ""
        }
        if v := strings.ToLower(strings.TrimSpace(os.Getenv("QD_NATIVE_IDENTITY"))); v == "0" || v == "false" || v == "no" {
                return ""
        }
        base := os.Getenv("LOCALAPPDATA")
        if strings.TrimSpace(base) == "" {
                home, err := os.UserHomeDir()
                if err != nil {
                        return ""
                }
                base = filepath.Join(home, "AppData", "Local")
        }
        names := []string{"Qoder CN", "QoderCN", "Qoder"}
        if normalizeRegion(region) == regionIntl {
                names = []string{"Qoder"}
        }
        for _, name := range names {
                for _, launcher := range []string{name + " Launcher", "Launcher"} {
                        ini := filepath.Join(base, name, launcher, "state.ini")
                        if d := iniValue(ini, "installDir"); d != "" && isDir(d) {
                                return umidExe(d)
                        }
                }
        }
        for _, name := range names {
                d := filepath.Join(base, "Programs", name)
                if isDir(d) {
                        return umidExe(d)
                }
        }
        return ""
}

func umidExe(installDir string) string {
        exe := filepath.Join(installDir, "resources", "umid", "runtime-info.exe")
        if st, err := os.Stat(exe); err == nil && !st.IsDir() {
                return exe
        }
        return ""
}

func isDir(path string) bool {
        st, err := os.Stat(path)
        return err == nil && st.IsDir()
}

// iniValue reads one key from a UTF-8 or UTF-16 state.ini (launcher's
// writer is not consistent — hub capture reads both encodings).
func iniValue(path, key string) string {
        raw, err := os.ReadFile(path)
        if err != nil {
                return ""
        }
        if bytes.HasPrefix(raw, []byte{0xFF, 0xFE}) {
                raw = raw[2:]
                return iniScan(decodeUTF16LE(raw), key)
        }
        return iniScan(string(raw), key)
}

func iniScan(text, key string) string {
        for _, line := range strings.Split(text, "\n") {
                line = strings.TrimSpace(line)
                if strings.HasPrefix(strings.ToLower(line), strings.ToLower(key)+"=") {
                        return strings.TrimSpace(line[len(key)+1:])
                }
        }
        return ""
}

// decodeUTF16LE decodes little-endian UTF-16 content (state.ini's second
// possible encoding) without pulling golang.org/x/text in.
func decodeUTF16LE(b []byte) string {
        var sb strings.Builder
        for i := 0; i+1 < len(b); i += 2 {
                r := rune(b[i]) | rune(b[i+1])<<8
                if r == 0 {
                        break
                }
                sb.WriteRune(r)
        }
        return sb.String()
}

// attachMachineIdentityHeaders adds the Cosy-Machine* headers the campaigns
// platform filters device-targeted rows on. Campaigns-surface only — the
// quota/plan/legacy endpoints never gated on them.
func attachMachineIdentityHeaders(req *http.Request, mi *machineIdentity) {
        if mi.MachineID != "" {
                req.Header.Set("Cosy-MachineId", mi.MachineID)
        }
        if mi.MachineToken != "" {
                req.Header.Set("Cosy-MachineToken", mi.MachineToken)
        }
        if mi.MachineType != "" {
                req.Header.Set("Cosy-MachineType", mi.MachineType)
        }
        if mi.MachineCode != "" {
                req.Header.Set("Cosy-MachineCode", mi.MachineCode)
        }
        if mi.MachineOS != "" {
                req.Header.Set("Cosy-MachineOS", mi.MachineOS)
        }
        if mi.MachineHostname != "" {
                req.Header.Set("Cosy-MachineHostname", mi.MachineHostname)
        }
}

// machineIdentityHint renders the identity's role in the 领取Pro diagnostics:
// where it came from and what its limitations mean for device-targeted rows.
func machineIdentityHint(sa *storedAuth) string {
        mi := machineIdentityFor(authRegion(sa), sa.Account.UID, false)
        if mi.Source == "runtime-info" {
                if mi.VMIsVM {
                        brand := strings.TrimSpace(mi.VMBrand)
                        if brand == "" {
                                brand = "未知平台"
                        }
                        return fmt.Sprintf("。本机身份来源：官方 runtime-info.exe；注意官方风控判定本机为虚拟机（%s，评分 %d/100）——虚拟机不参与新人/定向活动", brand, mi.VMScore)
                }
                return "。本机身份来源：官方 runtime-info.exe（真实机器身份，定向活动可见性最优）"
        }
        return "。本机未取得官方机器身份（未找到官方客户端的 runtime-info.exe，使用派生身份）——设备定向活动（含 Pro 升级包）可能被服务端过滤；在装有官方客户端的机器上执行可取回真身份"
}
