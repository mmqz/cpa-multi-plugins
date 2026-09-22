package upstream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/auth"
)

// TestFetchModelsUserFacingFilter 锁定 v0.12.46 的目录过滤契约：
// get_detail_param 目录混有内部配置（is_invisible_to_user=true）、租户自定义
// 占位模板（display_name 为空）与已下线开关（config_switch=false），一律不得
// 透出为可选模型；可见条目须携带 context_window_tokens.dev 作为上下文窗口；
// 布尔字段缺省按可见/启用处理（防上游删字段把目录过滤空）。
func TestFetchModelsUserFacingFilter(t *testing.T) {
	catalog := `{"config_info_list":[
                {"config_name":"Doubao-Seed-2.1-Pro","config_switch":true,"is_invisible_to_user":false,
                 "context_window_tokens":{"dev":256000},"display_config":{"display_name":"Seed-2.1-Pro"}},
                {"config_name":"seed-code-pro-0430","config_switch":true,"is_invisible_to_user":true,
                 "context_window_tokens":{"dev":232768},"display_config":{"display_name":"Doubao-Seed-2.1-Pro"}},
                {"config_name":"custom_model_placeholder","config_switch":true,"is_invisible_to_user":false,
                 "context_window_tokens":{"dev":128000},"display_config":{"display_name":""}},
                {"config_name":"legacy-model","config_switch":false,"is_invisible_to_user":false,
                 "context_window_tokens":{"dev":100000},"display_config":{"display_name":"Legacy"}},
                {"config_name":"minimax-m3","config_switch":true,"is_invisible_to_user":false,
                 "context_window_tokens":{"dev":200000},"display_config":{"display_name":"MiniMax-M3"}},
                {"config_name":"no-flags-model","context_window_tokens":{"dev":64000},
                 "display_config":{"display_name":"NoFlags"}},
                {"config_name":"","display_config":{"display_name":"empty-id"}}
        ]}`
	var seenPath bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpModels) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		seenPath = true
		return jsonResp(200, catalog), nil
	})
	a := &auth.Auth{AccessToken: "at", Variant: "solo"}
	ms, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if !seenPath {
		t.Fatal("models endpoint never hit")
	}
	got := map[string]ModelInfo{}
	for _, m := range ms {
		got[m.ID] = m
	}
	for _, want := range []string{"Doubao-Seed-2.1-Pro", "minimax-m3", "no-flags-model"} {
		if _, ok := got[want]; !ok {
			t.Errorf("user-facing model %q missing from %v", want, idsOf(ms))
		}
	}
	for _, banned := range []string{"seed-code-pro-0430", "custom_model_placeholder", "legacy-model", ""} {
		if _, ok := got[banned]; ok {
			t.Errorf("non-user-facing model %q leaked into catalog", banned)
		}
	}
	if got["Doubao-Seed-2.1-Pro"].ContextWindow != 256000 {
		t.Errorf("ContextWindow=%d want 256000", got["Doubao-Seed-2.1-Pro"].ContextWindow)
	}
	if got["no-flags-model"].ContextWindow != 64000 {
		t.Errorf("missing-flags ctx=%d want 64000", got["no-flags-model"].ContextWindow)
	}
	if got["minimax-m3"].Name != "MiniMax-M3" {
		t.Errorf("Name=%q want display_name", got["minimax-m3"].Name)
	}
}

// TestFetchModelsAllFilteredIsError：目录全被过滤（例如只余内部配置）时必须
// 返回错误，让上层走静态兜底而不是交出空列表。
func TestFetchModelsAllFilteredIsError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"config_info_list":[
                        {"config_name":"browser_use_subagent","config_switch":true,"is_invisible_to_user":true,
                         "display_config":{"display_name":""}},
                        {"config_name":"summary","config_switch":true,"is_invisible_to_user":false,
                         "display_config":{"display_name":""}}
                ]}`), nil
	})
	a := &auth.Auth{AccessToken: "at", Variant: "cn"}
	if _, err := c.FetchModels(a); err == nil {
		t.Fatal("expected error when no user-facing models remain, got nil")
	}
}

// TestFetchModelsTransportErrorPassthrough：非 2xx 由 doJSON 归一为 *Error，
// FetchModels 原样上抛（上层据此打静态兜底日志）。
func TestFetchModelsTransportErrorPassthrough(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(500, "boom"), nil
	})
	a := &auth.Auth{AccessToken: "at", Variant: "cn"}
	_, err := c.FetchModels(a)
	if err == nil {
		t.Fatal("expected upstream error, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %v should carry status", err)
	}
}

// TestFetchModelsSendsVariantFunction：请求体 function 必须随凭证 variant。
// v0.12.79 (issue #9): llm_utils_chat 只接受 solo_work_lite —— 全部 variant
// 都发 solo_work_lite（cn 原先发 inline_chat 是死路，所有模型必 4001）。
func TestFetchModelsSendsVariantFunction(t *testing.T) {
	var gotBody string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		return jsonResp(200, `{"config_info_list":[{"config_name":"m","display_config":{"display_name":"M"}}]}`), nil
	})
	for variant, wantFn := range map[string]string{
		"solo": "solo_work_lite", "cn": "solo_work_lite", "intl": "solo_work_lite", "": "solo_work_lite",
	} {
		a := &auth.Auth{AccessToken: "at", Variant: variant}
		if _, err := c.FetchModels(a); err != nil {
			t.Fatalf("%s: %v", variant, err)
		}
		if !strings.Contains(gotBody, `"function":"`+wantFn+`"`) {
			t.Errorf("variant=%s body=%s missing function=%s", variant, gotBody, wantFn)
		}
	}
}

// v0.12.79 (issue #9): solo_agent-only 死模型过滤 —— 精确名单里的死键在
// solo_work_lite 通道必 4001（llm_raw_chat 通道才服务它们），不得注册为
// 可选模型；大小写不敏感（目录两种写法都出现过）。v0.12.83 (issue #10)：
// 前缀匹配改精确名单，-Official 正式版条目实测可用，必须放行。
func TestFetchModelsFiltersSoloAgentOnlyModels(t *testing.T) {
	catalog := `{"config_info_list":[
                {"config_name":"agnes-agent-x","is_invisible_to_user":false,
                 "display_config":{"display_name":"Agnes X"}},
                {"config_name":"DeepSeek-V4-Flash","is_invisible_to_user":false,
                 "display_config":{"display_name":"DeepSeek-V4-Flash"}},
                {"config_name":"deepseek-v4-pro","is_invisible_to_user":false,
                 "display_config":{"display_name":"DeepSeek-V4-Pro"}},
                {"config_name":"DeepSeek-V4-Pro-Official","is_invisible_to_user":false,
                 "display_config":{"display_name":"DeepSeek-V4-Pro 正式版"}},
                {"config_name":"deepseek-v5-flash","is_invisible_to_user":false,
                 "display_config":{"display_name":"DeepSeek-V5-Flash"}},
                {"config_name":"kimi-k2.6","is_invisible_to_user":false,
                 "context_window_tokens":{"dev":200000},
                 "display_config":{"display_name":"Kimi-K2.6"}}
        ]}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, catalog), nil
	})
	a := &auth.Auth{AccessToken: "at", Variant: "cn"}
	ms, err := c.FetchModels(a)
	if err != nil {
		t.Fatal(err)
	}
	got := idsOf(ms)
	for _, dead := range []string{"agnes-agent-x", "DeepSeek-V4-Flash", "deepseek-v4-pro"} {
		for _, id := range got {
			if strings.EqualFold(id, dead) {
				t.Errorf("dead model %q leaked into catalog: %v", dead, got)
			}
		}
	}
	for _, live := range []string{"DeepSeek-V4-Pro-Official", "deepseek-v5-flash", "kimi-k2.6"} {
		found := false
		for _, id := range got {
			if id == live {
				found = true
			}
		}
		if !found {
			t.Errorf("live model %q filtered out: %v", live, got)
		}
	}
}

func idsOf(ms []ModelInfo) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}
