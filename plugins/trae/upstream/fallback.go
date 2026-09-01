// fallback.go: multi-origin candidate list for ExchangeToken / GetUserInfo.
// 对齐 cockpit-tools build_api_urls 的多源重试行为：主源不可达或返回
// 404 等错误时自动切换备用源，避免单一 host 变动导致整个插件瘫痪。
package upstream

import "strings"

// cnAlternateOrigins 是 CN 平台的备用账号 API 源（cockpit-tools
// TRAE_ACCOUNT_API_ORIGIN_CN_ICUBE 等）。
var cnAlternateOrigins = []string{
	"https://api.trae.com.cn",
}

// exchangeHosts 返回去重后的候选 host 列表：primary（账号 apiHost）→
// OAuthHost → 平台备用源。空值与重复项被剔除。
func exchangeHosts(primary, oauth string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(h string) {
		h = strings.TrimRight(strings.TrimSpace(h), "/")
		if h == "" {
			return
		}
		if !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "https://") {
			h = "https://" + h
		}
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	add(primary)
	add(oauth)
	for _, h := range cnAlternateOrigins {
		add(h)
	}
	return out
}
