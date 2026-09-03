// chat_error.go translates upstream chat rejections into actionable errors.
//
// v0.12.18: the CodeBuddy Intl gateway (codebuddy.ai) rejects models that are
// not registered on ITS catalog with HTTP 400
// {"code":11102,"msg":"model [X] service info not found"}. The executor used
// to pass the raw payload through ("upstream 400: {...}"), which told the
// user nothing about which models the account's realm actually serves — and
// the model list itself was unreliable for Intl accounts because discovery
// queried the CN endpoint (fixed in models.go the same release). The
// translator detects 11102 and rewrites the error into a bilingual, actionable
// message that names the realm and its best-known catalog.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// upstreamErrorShape mirrors the APISIX/business-JSON error envelope the
// WorkBuddy gateways return on chat rejections.
type upstreamErrorShape struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// isModelNotRegistered reports whether an upstream >=400 chat payload is the
// model-catalog rejection (code 11102 "service info not found"). The JSON
// path is authoritative; the substring path catches envelope variants where
// the payload is not the plain business JSON (e.g. wrapped in HTML or a
// different envelope) but still carries the same markers.
func isModelNotRegistered(statusCode int, payload string) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	var shape upstreamErrorShape
	if err := json.Unmarshal([]byte(payload), &shape); err == nil && shape.Code == 11102 {
		return true
	}
	low := strings.ToLower(payload)
	return strings.Contains(low, "11102") && strings.Contains(low, "service info not found")
}

// realmDisplayName maps a realm key to the human-readable name used in error
// copy so users can tell WHICH gateway rejected the model.
func realmDisplayName(realm string) string {
	switch realm {
	case "intl":
		return "CodeBuddy Intl (codebuddy.ai)"
	case "global":
		return "WorkBuddy Global (workbuddy.ai)"
	default:
		return "WorkBuddy CN (copilot.tencent.com)"
	}
}

// modelHintForRealm renders the best-known model catalog for a realm.
// Cached realm discovery (if fresh) is the truth; otherwise the realm's
// static catalog is shown (v0.12.19: per-realm — the CN list is no longer
// shown for Intl/Global accounts, which used to advertise models their
// gateway rejects with 11102). Network calls are deliberately NOT made here:
// the executor error path must stay fast, and a doomed 15s discovery call
// during error handling would only add latency.
func modelHintForRealm(realm string) string {
	ids := make([]string, 0, 16)
	if ms, ok := cachedDynamicModels(realm); ok {
		for _, m := range ms {
			if id := strings.TrimSpace(m.ID); id != "" {
				ids = append(ids, id)
			}
		}
	}
	label := "该区域缓存目录 / cached realm catalog"
	if len(ids) == 0 {
		for _, m := range staticModelsForRealm(realm) {
			ids = append(ids, m.ID)
		}
		realmTag := strings.ToUpper(strings.TrimSpace(realm))
		if realmTag == "" {
			realmTag = "CN"
		}
		label = fmt.Sprintf("静态 %s 目录（该区域已知支持；动态发现优先，也可用 models_%s 配置写死） / static %s catalog (known-good for this realm; dynamic discovery wins, or pin via models_%s)",
			realmTag, strings.ToLower(realmTag), realmTag, strings.ToLower(realmTag))
	}
	if len(ids) > 20 {
		ids = ids[:20]
	}
	return fmt.Sprintf("%s: %s", label, strings.Join(ids, ", "))
}

// translateChatUpstreamError converts one upstream chat failure into the
// plugin error. 11102 rejections get a bilingual actionable message; every
// other failure keeps the historical "upstream <status>: <payload>" shape so
// existing log parsers and client behavior stay unchanged.
func translateChatUpstreamError(statusCode int, payload string, sa *storedAuth) error {
	if isModelNotRegistered(statusCode, payload) {
		realm := "cn"
		if sa != nil {
			realm = accountRegion(sa)
		}
		return fmt.Errorf(
			"模型未被该账号区域的上游注册（code 11102 service info not found），区域=%s；请改用该区域可用模型后重试，模型列表以 /models 实际返回为准。"+
				" // Model not registered on the %s upstream; pick a model from its catalog and retry. %s | raw: %s",
			realmDisplayName(realm), realmDisplayName(realm), modelHintForRealm(realm),
			truncateRedacted(payload, 200))
	}
	return fmt.Errorf("upstream %d: %s", statusCode, truncateRedacted(payload, 200))
}
