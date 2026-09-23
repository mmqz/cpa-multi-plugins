// cmd/probe — mimo cookie-lane 诊断探针（独立于插件主包的孪生实现）。
//
// 用途：在已安装官方桌面的 Windows 机器上（与桌面同一 OS 用户）一次性验证：
//  1. %APPDATA%\Xiaomi MiMo AI\Local State 的 os_crypt 密钥能被 DPAPI 解开
//  2. 分区 Cookies 库（Partitions\xiaomi-account\Network\Cookies，旧布局兜底）
//     里存在 *.xiaomimimo.com 会话 Cookie 且 v10 AES-256-GCM 解密成功
//  3. 官方 me 端点（{区域基址}/user/xiaomi/me）是否认可该会话（桌面同款探针）
//
// 隐私边界（与插件 policy 对齐，见 docs/MIMO_AUTH.md §6）：
//   - Cookie 明文只进内存：绝不打印、不落盘、不发往 *.xiaomimimo.com 之外的主机
//   - 输出仅结构信息（host/name/过期时间/HTTP 状态码/code/区域），无凭据值
//
// 本目录是诊断工具；插件主包 cookies*.go 才是权威实现，算法刻意同源。
// 用法：
//
//	mimo-cookie-probe.exe                 # 全流程（需在桌面同用户的 Windows 上）
//	mimo-cookie-probe.exe --inspect-only  # 只盘点 Cookie 结构，不解密不出网
//	mimo-cookie-probe.exe --cookies <路径> --local-state <路径>
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const webkitEpochDiff = 11644473600 // 1601-01-01 → 1970-01-01 秒差

var regions = []struct {
	name string
	base string
}{
	{"cn", "https://mimo-server-cn.xiaomimimo.com/api"},
	{"sgp", "https://mimo-server-sgp.xiaomimimo.com/api"},
	{"ru", "https://mimo-server-ru.xiaomimimo.com/api"},
	{"in", "https://mimo-server-in.xiaomimimo.com/api"},
}

type cookieRow struct {
	Name       string
	Value      string // 明文 value 列（新版 Chromium 通常为空）
	Encrypted  []byte
	HostKey    string
	Path       string
	Secure     bool
	HTTPOnly   bool
	ExpiresUTC int64 // WebKit 微秒；0 = 会话 Cookie
}

func wkToUTC(us int64) string {
	if us <= 0 {
		return "<session>"
	}
	return time.Unix(us/1_000_000-webkitEpochDiff, 0).UTC().Format("2006-01-02 15:04:05Z")
}

func encPrefix(b []byte) string {
	switch {
	case len(b) >= 3 && string(b[:3]) == "v10":
		return "v10"
	case len(b) >= 3 && string(b[:3]) == "v11":
		return "v11"
	case len(b) >= 3 && string(b[:3]) == "v20":
		return "v20"
	case len(b) >= 4 && string(b[:4]) == "\x01\x00\x00\x00":
		return "dpapi-legacy"
	case len(b) == 0:
		return "-"
	default:
		return fmt.Sprintf("%x", b[:1])
	}
}

func main() {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		home, _ := os.UserHomeDir()
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	base := filepath.Join(appData, "Xiaomi MiMo AI")
	legacyBase := filepath.Join(appData, "Xiaomi MiMo")

	lsFlag := flag.String("local-state", "", "Local State 路径（默认自动定位）")
	dbFlag := flag.String("cookies", "", "Cookies 库路径（默认自动定位，新旧布局都试）")
	inspect := flag.Bool("inspect-only", false, "只盘点结构，不 DPAPI 解密、不出网")
	timeout := flag.Duration("timeout", 15*time.Second, "me 探测超时")
	flag.Parse()

	localState := *lsFlag
	if localState == "" {
		for _, b := range []string{base, legacyBase} {
			p := filepath.Join(b, "Local State")
			if fileExists(p) {
				localState = p
				break
			}
		}
	}
	dbPath := *dbFlag
	if dbPath == "" {
		for _, b := range []string{base, legacyBase} {
			for _, rel := range []string{
				filepath.Join("Partitions", "xiaomi-account", "Network", "Cookies"),
				filepath.Join("Partitions", "xiaomi-account", "Cookies"),
			} {
				p := filepath.Join(b, rel)
				if fileExists(p) {
					dbPath = p
					break
				}
			}
			if dbPath != "" {
				break
			}
		}
	}
	fmt.Printf("[1] Cookies 库: %s\n", dbPath)
	fmt.Printf("    Local State: %s\n", localState)
	if dbPath == "" {
		fail("未找到 Cookies 库：确认桌面已安装且分区目录存在（或用 --cookies 指定）")
	}

	rows, err := queryCookieRows(copyToTemp(dbPath))
	if err != nil {
		fail("读取 Cookies 库失败: %v", err)
	}

	var lane, loginSide, other []cookieRow
	for _, r := range rows {
		switch {
		case isHostFamily(r.HostKey, "xiaomimimo.com"):
			lane = append(lane, r)
		case isHostFamily(r.HostKey, "xiaomi.com"):
			loginSide = append(loginSide, r)
		default:
			other = append(other, r)
		}
	}
	fmt.Printf("[2] Cookie 盘点: xiaomimimo.com 载荷 %d 条 / xiaomi.com 登录侧 %d 条 / 其他 %d 条\n",
		len(lane), len(loginSide), len(other))
	dump := func(title string, rs []cookieRow) {
		if len(rs) == 0 {
			return
		}
		fmt.Printf("    -- %s --\n", title)
		for _, r := range rs {
			fmt.Printf("       %-34s %-26s path=%-8s exp=%s enc=%s\n",
				r.HostKey, r.Name, r.Path, wkToUTC(r.ExpiresUTC), encPrefix(r.Encrypted))
		}
	}
	dump("Cookie lane 载荷（回放对象）", lane)
	dump("登录侧（仅计数，绝不回放）", loginSide)

	if len(lane) == 0 {
		fmt.Println("\n结论: 分区里没有任何 xiaomimimo.com Cookie —— 桌面未登录、登录前复制的副本，或分区被重建。")
		fmt.Println("      请先在桌面完成登录 → 完全退出桌面（托盘退出）→ 重新复制 Network\\Cookies 再测。")
		os.Exit(1)
	}
	if *inspect {
		fmt.Println("\n--inspect-only: 到此为止（未解密、未出网）。")
		return
	}
	if localState == "" {
		fail("未找到 Local State（解密密钥容器）；用 --local-state 指定")
	}

	fmt.Println("[3] DPAPI 解开 os_crypt 密钥 …")
	key, err := loadOsCryptKey(localState)
	if err != nil {
		fail("Local State 密钥不可用: %v", err)
	}

	usable := 0
	decFail := 0
	for i := range lane {
		r := &lane[i]
		if len(r.Encrypted) < 3 || string(r.Encrypted[:3]) != "v10" {
			decFail++
			continue
		}
		plain, err := aesGCMDecrypt(key, r.Encrypted[3:])
		if err != nil {
			decFail++
			continue
		}
		r.Value = string(plain)
		usable++
	}
	fmt.Printf("[4] v10 解密: 成功 %d / 失败 %d（明文绝不打印）\n", usable, decFail)
	if usable == 0 {
		fmt.Println("\n结论: 有 Cookie 行但全部解密失败 —— 多半是不同 OS 用户/不同机器拷来的（DPAPI 绑定）。")
		os.Exit(1)
	}

	fmt.Println("[5] 官方 me 端点探测（桌面同款会话探针，仅带 Cookie，无遥测）…")
	ok := false
	for _, rg := range regions {
		code, regionName, status, redirHost, err := probeMe(rg.base, lane, *timeout)
		switch {
		case err != nil:
			fmt.Printf("    %-4s %s: 网络错误 %v\n", rg.name, rg.base, err)
		case redirHost != "":
			fmt.Printf("    %-4s HTTP %d → 302 跳转 %s（该区域认为未登录）\n", rg.name, status, redirHost)
		default:
			verdict := "未认可"
			if code == 0 {
				verdict = "✅ 登录态有效"
				ok = true
			}
			fmt.Printf("    %-4s HTTP %d code=%d region=%s %s\n", rg.name, status, code, regionName, verdict)
		}
	}
	if ok {
		fmt.Println("\n总结论: Cookie lane 凭据可用 —— 插件 adopt 流程在此机器可收养该会话。")
		os.Exit(0)
	}
	fmt.Println("\n总结论: Cookie 已解密但 me 端点未认可 —— 会话可能已过期/被服务端拒绝，重启桌面刷新后再试。")
	os.Exit(1)
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+f+"\n", a...)
	os.Exit(2)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// copyToTemp 把 Cookies 库（含 journal/wal 伴生）拷到私有临时目录，绕开运行中
// 桌面的锁与半写状态。
func copyToTemp(dbPath string) string {
	tmp, err := os.MkdirTemp("", "mimo-probe-")
	if err != nil {
		fail("临时目录: %v", err)
	}
	dst := filepath.Join(tmp, "Cookies")
	for _, suffix := range []string{"", "-journal", "-wal"} {
		in, err := os.ReadFile(dbPath + suffix)
		if err != nil {
			continue
		}
		_ = os.WriteFile(dst+suffix, in, 0o600)
	}
	return dst
}

func queryCookieRows(dbPath string) ([]cookieRow, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rs, err := db.Query(`SELECT name, value, encrypted_value, host_key, path, is_secure, is_httponly, expires_utc FROM cookies`)
	if err != nil {
		return nil, fmt.Errorf("query cookies: %w", err)
	}
	defer rs.Close()
	out := make([]cookieRow, 0, 32)
	for rs.Next() {
		var r cookieRow
		var v, enc []byte
		if err := rs.Scan(&r.Name, &v, &enc, &r.HostKey, &r.Path, &r.Secure, &r.HTTPOnly, &r.ExpiresUTC); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		r.Value = string(v)
		r.Encrypted = enc
		out = append(out, r)
	}
	return out, rs.Err()
}

// isHostFamily 与插件 isMimoUpstreamHost 同语义：域 Cookie 去点后缀匹配，
// 宿主 Cookie 精确匹配。
func isHostFamily(hostKey, family string) bool {
	h := strings.TrimPrefix(hostKey, ".")
	return h == family || strings.HasSuffix(h, "."+family)
}

// aesGCMDecrypt 复刻 Chromium os_crypt v10：12 字节 nonce 在前，16 字节 tag 在尾。
func aesGCMDecrypt(key, payload []byte) ([]byte, error) {
	if len(payload) < 12+16 {
		return nil, fmt.Errorf("v10 payload too short (%d)", len(payload))
	}
	return gcmOpen(key, payload[:12], payload[12:len(payload)-16], payload[len(payload)-16:])
}

// gcmOpen AES-256-GCM 解密（密文与 tag 分离传入）。
func gcmOpen(key, nonce, ciphertext, tag []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, append(append([]byte{}, ciphertext...), tag...), nil)
}

// loadOsCryptKey 读 Local State → os_crypt.encrypted_key（base64，"DPAPI" 前缀）
// → DPAPI 解出 32 字节 AES 密钥。与插件 localStateCryptKey 同源。
func loadOsCryptKey(localState string) ([]byte, error) {
	raw, err := os.ReadFile(localState)
	if err != nil {
		return nil, err
	}
	var ls struct {
		OsCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("parse Local State: %w", err)
	}
	if ls.OsCrypt.EncryptedKey == "" {
		return nil, fmt.Errorf("no os_crypt.encrypted_key")
	}
	blob, err := base64.StdEncoding.DecodeString(ls.OsCrypt.EncryptedKey)
	if err != nil {
		return nil, err
	}
	if len(blob) < 5 || string(blob[:5]) != "DPAPI" {
		return nil, fmt.Errorf("encrypted_key missing DPAPI prefix")
	}
	key, err := dpapiUnprotect(blob[5:])
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("os_crypt key length %d (want 32)", len(key))
	}
	return key, nil
}

// probeMe 用收养的 Cookie 打官方 me 端点；不跟随重定向（未登录时服务端
// 302 去 account.xiaomi.com SSO 页 —— 只报告跳转主机，不打印含 nonce 的 URL）。
// 返回 (code, data.region, httpStatus, redirectHost, err)。
func probeMe(base string, cookies []cookieRow, timeout time.Duration) (int, string, int, string, error) {
	u, err := url.Parse(base + "/user/xiaomi/me")
	if err != nil {
		return 0, "", 0, "", err
	}
	header := buildCookieHeader(cookies, u)
	if header == "" {
		return 0, "", 0, "", fmt.Errorf("没有可匹配 %s 的 Cookie", u.Host)
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, "", 0, "", err
	}
	req.Header.Set("Cookie", header)
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		host := ""
		if lu, perr := url.Parse(loc); perr == nil {
			host = lu.Host
		}
		return 0, "", resp.StatusCode, host, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var parsed struct {
		Code int `json:"code"`
		Data struct {
			Region string `json:"region"`
			UserID any    `json:"userId"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &parsed)
	return parsed.Code, parsed.Data.Region, resp.StatusCode, "", nil
}

// buildCookieHeader 按 RFC6265 域/路径匹配挑 Cookie（与插件 jar 匹配同语义，
// 长路径优先），返回 "k=v; k2=v2" 或空串。
func buildCookieHeader(cookies []cookieRow, u *url.URL) string {
	host := u.Host
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	var picked []cookieRow
	for _, c := range cookies {
		if c.Value == "" || !hostMatchesCookie(c.HostKey, host) || !pathMatchesCookie(c.Path, path) {
			continue
		}
		picked = append(picked, c)
	}
	if len(picked) == 0 {
		return ""
	}
	// 长路径优先（RFC6265 §5.4 排序简化版）
	for i := 1; i < len(picked); i++ {
		for j := i; j > 0 && len(picked[j].Path) > len(picked[j-1].Path); j-- {
			picked[j], picked[j-1] = picked[j-1], picked[j]
		}
	}
	parts := make([]string, len(picked))
	for i, c := range picked {
		parts[i] = c.Name + "=" + c.Value
	}
	return strings.Join(parts, "; ")
}

func hostMatchesCookie(cookieHost, reqHost string) bool {
	if strings.HasPrefix(cookieHost, ".") {
		h := strings.TrimPrefix(cookieHost, ".")
		return reqHost == h || strings.HasSuffix(reqHost, "."+h)
	}
	return reqHost == cookieHost
}

func pathMatchesCookie(cookiePath, reqPath string) bool {
	cp := cookiePath
	if cp == "" {
		cp = "/"
	}
	if !strings.HasPrefix(reqPath, cp) {
		return false
	}
	if len(reqPath) > len(cp) && cp[len(cp)-1] != '/' && reqPath[len(cp)] != '/' {
		return false
	}
	return true
}
