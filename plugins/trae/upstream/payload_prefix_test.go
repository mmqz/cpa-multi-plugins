package upstream

// issue #18 回归：宿主凭据模型前缀（CPA applyModelPrefixes 在插件广告名之上
// 构造的 "prefix/" + base）随客户端 body 原样进入插件。出站 config_name 必须
// 使用宿主解析后的 ExecutorRequest.Model，而不是 body 里的原始名字——否则
// SOLO 通道每条调用都在流内被 biz_code=4001 拒绝（issue #18 的实测形态）。

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
)

func preparedModelFields(t *testing.T, out []byte) (configName, model string) {
	t.Helper()
	var peek struct {
		ConfigName string `json:"config_name"`
		Model      string `json:"model"`
	}
	if err := json.Unmarshal(out, &peek); err != nil {
		t.Fatalf("prepared body is not JSON: %v\n%s", err, out)
	}
	return peek.ConfigName, peek.Model
}

func TestPrepareBodyResolvedWinsOverHostPrefix(t *testing.T) {
	out := PrepareBodyResolved(
		[]byte(`{"model":"tr/kimi-k2.6-solo","messages":[{"role":"user","content":"hi"}]}`),
		"solo", "kimi-k2.6-solo")
	cn, m := preparedModelFields(t, out)
	if cn != "kimi-k2.6" || m != "kimi-k2.6" {
		t.Fatalf("config_name=%q model=%q, want bare kimi-k2.6", cn, m)
	}
}

func TestPrepareBodyResolvedKeepsUpstreamSlashSegment(t *testing.T) {
	// "deepseek-ai/" 是上游 config 名的合法组成部分——绝不能按 "/" 切分，
	// 只能整名采用宿主解析结果，再剥插件自己的 -solo 后缀（issue #18 提示的陷阱）。
	out := PrepareBodyResolved(
		[]byte(`{"model":"tr/deepseek-ai/deepseek-v4-pro-solo","messages":[{"role":"user","content":"hi"}]}`),
		"solo", "deepseek-ai/deepseek-v4-pro-solo")
	cn, _ := preparedModelFields(t, out)
	if cn != "deepseek-ai/deepseek-v4-pro" {
		t.Fatalf("config_name=%q, want deepseek-ai/deepseek-v4-pro", cn)
	}
}

func TestPrepareBodyResolvedEmptyFallsBackToBody(t *testing.T) {
	// resolved 为空（宿主未填 ExecutorRequest.Model）→ 完全回到 0.12.58 行为：
	// body 名原样进 SanitizeModelName（只剥 -solo/-intl 后缀，不碰任何前缀）。
	out := PrepareBodyResolved(
		[]byte(`{"model":"tr/kimi-k2.6-solo","messages":[{"role":"user","content":"hi"}]}`),
		"solo", "")
	cn, _ := preparedModelFields(t, out)
	if cn != "tr/kimi-k2.6" {
		t.Fatalf("config_name=%q, want legacy tr/kimi-k2.6", cn)
	}
}

func TestPrepareBodyResolvedWinsWhenBodyHasNoModel(t *testing.T) {
	out := PrepareBodyResolved(
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		"solo", "kimi-k2.6-solo")
	cn, _ := preparedModelFields(t, out)
	if cn != "kimi-k2.6" {
		t.Fatalf("config_name=%q, want kimi-k2.6", cn)
	}
}

func TestPrepareBodyWrapperKeepsLegacyBehavior(t *testing.T) {
	// 两参 wrapper 与 0.12.58 逐字节同行为：剥插件自己的后缀，不碰前缀。
	out := PrepareBody(
		[]byte(`{"model":"Doubao-Seed-2.1-Turbo-solo","messages":[{"role":"user","content":"hi"}]}`),
		"solo")
	cn, _ := preparedModelFields(t, out)
	if cn != "Doubao-Seed-2.1-Turbo" {
		t.Fatalf("config_name=%q, want Doubao-Seed-2.1-Turbo", cn)
	}
}

func TestNoteHostPrefixMismatchLogsOncePerName(t *testing.T) {
	hostPrefixWarned = sync.Map{} // 隔离其他用例留下的 once 键
	var buf bytes.Buffer
	log.SetOutput(&buf)
	NoteHostPrefixMismatch("tr/kimi-k2.6-solo", "kimi-k2.6-solo", "solo")
	NoteHostPrefixMismatch("tr/kimi-k2.6-solo", "kimi-k2.6-solo", "solo")
	NoteHostPrefixMismatch("tr/glm-5.2-solo", "glm-5.2-solo", "solo")
	log.SetOutput(os.Stderr)
	if n := strings.Count(buf.String(), "issue #18"); n != 2 {
		t.Fatalf("expected 2 warning lines (one per distinct body model), got %d:\n%s", n, buf.String())
	}
}
