// cmd/probe — mimo cookie-lane 诊断探针（独立于插件主包的孪生实现）。
//
// 用途：在已安装官方桌面的 Windows 机器上（与桌面同一 OS 用户）一次性验证：
//  1. %APPDATA%\Xiaomi MiMo AI\Local State 的 os_crypt 密钥能被 DPAPI 解开
//  2. 分区 Cookies 库（Partitions\xiaomi-account\Network\Cookies，旧布局兜底）
//     里可收养哪些行：*.xiaomimimo.com 会话行（仅老构建有）+ 账号域引导行
//     passToken/userId/cUserId/uLocale（当前构建只落这些，且明文，§6.1 #1）
//  3. M2 换票链（serviceLogin → STS）能否用引导行换出 serviceToken —— 与
//     插件 0.2.0 cookie lane 同判据。/user/xiaomi/me 已退役：me 对有效请求
//     也 302，不能判健康（docs/MIMO_AUTH.md §6.2 坑 1）
//
// 隐私边界（与插件 policy 对齐，见 docs/MIMO_AUTH.md §6）：
//   - Cookie/票据明文只进内存：绝不打印、不落盘；账号域行只发给 passport
//     （它们的原生域），P2 刻意无 Cookie
//   - 输出仅结构信息（host/name/过期/加密前缀/HTTP 状态/Set-Cookie 名单），
//     无任何凭据值；换票 URL 含 nonce/clientSign，只报状态码
//
// 本目录是诊断工具；插件主包 cookies*.go / exchange.go 才是权威实现，算法刻意同源。
// 用法：
//
//	mimo-cookie-probe.exe                 # 全流程（需在桌面同用户的 Windows 上）
//	mimo-cookie-probe.exe --inspect-only  # 只盘点 Cookie 结构，不解密不出网
//	mimo-cookie-probe.exe --region sgp    # 换票钉死区域（默认 auto：sgp→cn）
//	mimo-cookie-probe.exe --cookies <路径> --local-state <路径>
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const webkitEpochDiff = 11644473600 // 1601-01-01 → 1970-01-01 秒差

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
	timeout := flag.Duration("timeout", 15*time.Second, "换票诊断超时")
	regionFlag := flag.String("region", "auto", "换票区域：auto（sgp→cn 序，与插件一致）/ sgp / cn")
	flag.Parse()
	switch *regionFlag {
	case "auto", "sgp", "cn":
	default:
		fail("--region 只接受 auto/sgp/cn（ru/in 的 sid 从未实测，拒绝臆测）")
	}

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
	dump("登录侧引导行（换票材料，值绝不打印）", loginSide)

	if len(lane) == 0 {
		// 当前桌面包的正常形态（docs/MIMO_AUTH.md §6.1）：可用服务票据不落盘，
		// 分区内只有账号域引导行 —— 健康与否由换票链诊断回答，不在这里判。
		fmt.Println("    （无 xiaomimimo.com 行 —— 当前桌面包的正常形态：可用服务票据不落盘，判据走换票链诊断）")
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

	// lane 行（老构建的回放对象）与登录侧行（换票材料）都可能加密；当前构建
	// 的账号域行是明文（§6.1 #1），无需解密。
	usable := 0
	decFail := 0
	for i := range rows {
		r := &rows[i]
		if !isHostFamily(r.HostKey, "xiaomimimo.com") && !isHostFamily(r.HostKey, "xiaomi.com") {
			continue
		}
		if len(r.Encrypted) >= 3 && string(r.Encrypted[:3]) == "v10" {
			plain, err := aesGCMDecrypt(key, r.Encrypted[3:])
			if err != nil {
				decFail++
				continue
			}
			r.Value = string(plain)
			usable++
			continue
		}
		if r.Value == "" {
			decFail++ // 加密行但既非 v10 也非明文可读
		}
	}
	fmt.Printf("[4] v10 解密: 成功 %d / 失败 %d（明文绝不打印；当前构建的账号域行本就是明文）\n", usable, decFail)

	bootstrap := pickBootstrapRows(loginSide)
	if len(bootstrap) == 0 {
		if usable == 0 && decFail > 0 {
			fmt.Println("\n结论: 有 Cookie 行但全部解密失败 —— 多半是不同 OS 用户/不同机器拷来的（DPAPI 绑定）。")
		} else {
			fmt.Println("\n结论: 分区里既无 xiaomimimo.com 行、也无 passToken/userId 引导行 —— 桌面未登录或分区被重建。")
			fmt.Println("      请先在桌面完成登录 → 完全退出桌面（托盘退出）→ 再测。")
		}
		os.Exit(1)
	}
	bootNames := make([]string, 0, len(bootstrap))
	for _, r := range bootstrap {
		bootNames = append(bootNames, r.Name)
	}
	fmt.Printf("[5] 换票材料: %d 条（%s；值绝不打印）\n", len(bootstrap), strings.Join(bootNames, " "))

	fmt.Println("[6] M2 换票链诊断（serviceLogin → STS；sgp→cn 序与插件 auto 同序）…")
	var winner *probeExchangeResult
	passTokenDead := false
	for _, tgt := range exchangeTargets {
		if *regionFlag != "auto" && *regionFlag != tgt.region {
			continue
		}
		res, err := probeExchange(bootstrap, tgt.sid, *timeout)
		if err == nil {
			fmt.Printf("    %-4s sid=%-8s HTTP 200 换票成功（Set-Cookie: %s；serviceToken %d 字节）\n",
				tgt.region, tgt.sid, strings.Join(res.SetCookie, "/"), res.TokenLength)
			if winner == nil {
				winner = res
			}
			continue
		}
		fmt.Printf("    %-4s sid=%-8s %v\n", tgt.region, tgt.sid, err)
		if errors.Is(err, errProbePassTokenExpired) {
			passTokenDead = true
		}
	}
	if winner != nil {
		fmt.Printf("\n总结论: cookie lane 可用（区域 %s）—— 引导材料可随时换出服务票据，\n", winner.Region)
		fmt.Println("        插件 adopt 后走同一链路即可出可用凭据，无需桌面在线。")
		os.Exit(0)
	}
	if passTokenDead {
		fmt.Println("\n总结论: 引导材料已被 passport 拒绝（passToken 已失效）—— 请在桌面重新登录后重试。")
		os.Exit(1)
	}
	fmt.Println("\n总结论: 换票未成功（网络/边缘原因）—— 稍后重试，或用 --region 钉死另一区域。")
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
