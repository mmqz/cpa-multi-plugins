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
//
// v0.8.43 (official-package forensics, both packages sha256 on file): until
// now the native bridge was Windows-only — runtimeInfoExePath returned ""
// for any other GOOS, so every Linux/macOS deployment silently lived on the
// derived fallback even though BOTH official packages ship the bridge for
// it:
//
//   - Qoder-linux-amd64.deb ships resources/umid/runtime-info (ELF x86-64,
//     manifest qoderCliVersion 1.1.64, sha256 e30b307e...f7d7478) and the
//     binary runs standalone on stock Linux (container-verified).
//   - The client bundle (out/main/index.js, identical in CN v0.4.3 exe and
//     the intl deb) spawns it per platform (function IUt):
//       win32/darwin: runtime-info <env> --account-stdin, stdin {"account":...}
//       linux:        runtime-info <env>                (stdin ignored)
//     env is the release channel ("prod"), stdout's FIRST line is one JSON
//     object {machineToken, machineType, machineCode} (vmInfo parsed then
//     dropped by the official EUt mapper).
//   - The output is cached per active account for 1h ± 5min and any spawn
//     failure just sends the request without the machine headers — identity
//     is best-effort by design upstream.
//
// This release extends the bridge discovery to Linux (the deb's official
// install root /opt/Qoder) and macOS (Electron default layout), and adds a
// QD_UMID_BIN env override so container deployments can drop the official
// binary anywhere and point the plugin at it. The per-platform spawn dialect
// is byte-parity with the client.
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

// runtimeInfoArgs returns the official spawn dialect for one GOOS (client
// bundle, function IUt): win32/darwin get [env, --account-stdin] plus the
// account JSON on stdin; linux gets [env] and stdin is ignored. The bool
// reports whether stdin carries the account payload.
func runtimeInfoArgs(goos string) ([]string, bool) {
        if goos == "windows" || goos == "darwin" {
                return []string{"prod", "--account-stdin"}, true
        }
        return []string{"prod"}, false
}

// identitySourceNative is the Source value the official bridge produces.
// The cache split (v0.8.39) keys on it: native identities are machine-level
// and region-cacheable; derived ones are per-uid and never cached.
const identitySourceNative = "runtime-info"

var (
        // machineIdentityCache maps region → cache entry. Only NATIVE
        // identities are stored: they are machine-level, so region-level
        // caching is correct for them and for nothing else.
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
//
// v0.8.39 cache split (field report u673e7fcc + hub doctrine): the 30-minute
// region cache is only valid for the NATIVE bridge's identity — that one is
// machine-level, so every account on the host shares it by design. The
// DERIVED fallback is per-uid (hub: "多账号之间天然隔离，阻断跨账号关联风控");
// caching it per region collapsed every account in a deployment onto whichever
// pseudo-device was computed first, upstream answered with per-person dedup
// (SAME_PERSON_ALREADY_CLAIMED) and hid the daily row from the losing
// accounts — the mechanism behind the "当前没有可领取的活动" field report.
// Derived identities are pure-CPU derivations, so computing them per call
// costs nothing; only native identities enter the cache.
func machineIdentityFor(region, uid string, force bool) machineIdentity {
        if machineIdentityOverride != nil {
                return *machineIdentityOverride
        }
        now := time.Now()
        if !force {
                if v, ok := machineIdentityCache.Load(region); ok {
                        if e, ok := v.(machineIdentityCacheEntry); ok && now.Sub(e.at) < machineIdentityTTL && e.id.Source == identitySourceNative {
                                return e.id
                        }
                }
        }
        id := computeMachineIdentity(region, uid)
        if id.Source == identitySourceNative {
                machineIdentityCache.Store(region, machineIdentityCacheEntry{at: now, id: id})
        }
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

// runRuntimeInfo spawns the official bridge with the client's per-platform
// dialect (IUt): win32/darwin `runtime-info[.exe] prod --account-stdin` with
// one JSON object on stdin, linux `runtime-info prod` with stdin closed —
// first stdout line is the answer either way.
func runRuntimeInfo(exe, uid string) map[string]any {
        ctx, cancel := context.WithTimeout(context.Background(), runtimeInfoTimeout)
        defer cancel()
        args, withStdin := runtimeInfoArgs(runtime.GOOS)
        cmd := exec.CommandContext(ctx, exe, args...)
        cmd.Dir = filepath.Dir(exe)
        if withStdin {
                payload, _ := json.Marshal(map[string]any{"account": uid})
                cmd.Stdin = bytes.NewReader(append(payload, ' '))
        } else {
                cmd.Stdin = nil // official linux dialect: stdio ignore
        }
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
// cannot have one (disabled, client not installed).
//
// Windows layout (hub capture): the launcher records the real install dir
// in %LOCALAPPDATA%\<Qoder...>\<...Launcher>\state.ini (installDir=...);
// %LOCALAPPDATA%\Programs\<name> is the fallback layout.
//
// Linux layout (v0.8.43, official deb extracted): the package installs to
// /opt/Qoder with the bridge at /opt/Qoder/resources/umid/runtime-info.
// macOS layout: Electron default bundle path (best-effort — QD_UMID_BIN is
// the authoritative override there; no official dmg was dissected).
//
// QD_UMID_BIN (any platform) wins over all discovery: container
// deployments drop the official binary anywhere and point the plugin at
// it. QD_NATIVE_IDENTITY=0/false/no disables the native layer entirely.
func runtimeInfoExePath(region string) string {
        if v := strings.ToLower(strings.TrimSpace(os.Getenv("QD_NATIVE_IDENTITY"))); v == "0" || v == "false" || v == "no" {
                return ""
        }
        if p := strings.TrimSpace(os.Getenv("QD_UMID_BIN")); p != "" {
                if st, err := os.Stat(p); err == nil && !st.IsDir() {
                        return p
                }
                return ""
        }
        switch runtime.GOOS {
        case "windows":
                return runtimeInfoExePathWindows(region)
        case "darwin":
                for _, app := range []string{"/Applications/Qoder.app", "/Applications/Qoder CN.app", "/Applications/QoderCN.app"} {
                        if exe := umidExe(app + "/Contents"); exe != "" {
                                return exe
                        }
                }
                return ""
        default:
                // Official deb root (sha256-verified extraction).
                return umidExe("/opt/Qoder")
        }
}

func runtimeInfoExePathWindows(region string) string {
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
        name := "runtime-info"
        if runtime.GOOS == "windows" {
                name = "runtime-info.exe"
        }
        exe := filepath.Join(installDir, "resources", "umid", name)
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
        return "。本机未取得官方机器身份（未找到官方客户端的 runtime-info.exe/runtime-info，使用派生身份）——设备定向活动（含 Pro 升级包）可能被服务端过滤；在装有官方客户端的机器上执行可取回真身份，Linux/容器部署也可将官方安装包内 resources/umid/runtime-info 放到 /opt/Qoder 下，或以 QD_UMID_BIN 环境变量指定其路径"
}
