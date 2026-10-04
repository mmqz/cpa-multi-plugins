package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// --- v0.8.31: gateway-failure pages and the plan-gate (code 112) family ---
// User-visible behavior pinned here: ALB HTML 504 pages collapse to one
// actionable retry line instead of markup soup, and the plan-gate envelope
// renders one greppable plan_gate line whose marker drives pair-scoped
// cooldown without ever reading as account-wide credit trouble.

func TestChatUpstreamErrorCollapsesALBGatewayTimeout(t *testing.T) {
	const alb504 = "<html>\r\n<head><title>504 Gateway Time-out</title></head>\r\n<body bgcolor=\"white\">\r\n<center><h1>504 Gateway Time-out</h1></center>\r\n<hr><center>alb</center>\r\n</body>\r\n</html>\r\n"
	msg := chatUpstreamError(504, alb504).Error()
	for _, want := range []string{"upstream 504", "网关超时", "重试"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q in: %s", want, msg)
		}
	}
	for _, banned := range []string{"<html", "bgcolor", "gateway time-out"} {
		if strings.Contains(strings.ToLower(msg), banned) {
			t.Fatalf("raw page leaked into: %s", msg)
		}
	}
}

func TestChatUpstreamErrorGenericHTMLFallback(t *testing.T) {
	msg := chatUpstreamError(502, "<html><head><title>502 Bad Gateway</title></head><body><center>nginx</center></body></html>").Error()
	if !strings.Contains(msg, "upstream 502") || !strings.Contains(msg, "HTML 错误页") {
		t.Fatalf("generic gateway copy missing: %s", msg)
	}
	if strings.Contains(msg, "<html") {
		t.Fatalf("raw HTML leaked: %s", msg)
	}
	// Non-HTML bodies keep the historic shape.
	if got := chatUpstreamError(500, "boom").Error(); !strings.HasPrefix(got, "upstream 500: boom") {
		t.Fatalf("plain shape changed: %s", got)
	}
	// 4xx HTML is not collapsed (login pages etc. keep their body).
	if got := chatUpstreamError(403, "<html>forbidden</html>").Error(); !strings.Contains(got, "<html") {
		t.Fatalf("4xx HTML must pass through: %s", got)
	}
}

func TestDescribeQoderEnvelopeRejectionPlanGate112(t *testing.T) {
	const url = "https://qoder.com.cn/pricing?client=qoder"
	// Real wire shape (panel log 2026-09-26): the message field is a JSON
	// string quoting an object that carries pricingUrl.
	payload := `{"code":"112","message":"{\"pricingUrl\":\"` + url + `\"}"}`
	line := describeQoderEnvelopeRejection(payload, nil, 200)
	for _, want := range []string{"plan_gate", "code=112", "套餐额度", url} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in: %s", want, line)
		}
	}
	// Numeric spelling of the same code renders the same family.
	if line := describeQoderEnvelopeRejection(`{"code":112,"message":"{}"}`, nil, 200); !strings.Contains(line, "plan_gate") {
		t.Fatalf("numeric 112 must render plan_gate: %s", line)
	}
	// The same family through the full frame unwrap: error field carries the
	// code-112 object behind a 200-OK envelope.
	inner, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    "112",
			"message": `{"pricingUrl":"` + url + `"}`,
		},
	})
	frame, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": string(inner)})
	_, _, err := qoderUnwrapFrame("data:" + string(frame))
	if err == nil {
		t.Fatal("plan-gate envelope must surface")
	}
	for _, want := range []string{"plan_gate", url} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("frame error missing %q: %v", want, err)
		}
	}
}

func TestPricingURLFromMessage(t *testing.T) {
	const want = "https://qoder.com.cn/pricing?client=qoder"
	// Direct object.
	if got := pricingURLFromMessage(`{"pricingUrl":"` + want + `"}`); got != want {
		t.Fatalf("object shape: %q", got)
	}
	// JSON string quoting the object.
	if got := pricingURLFromMessage(`"{\"pricingUrl\":\"` + want + `\"}"`); got != want {
		t.Fatalf("quoted shape: %q", got)
	}
	// Substring sniff with a trailing junk terminator.
	if got := pricingURLFromMessage(`升级套餐 pricingUrl=` + want + `}end`); got != want {
		t.Fatalf("sniff shape: %q", got)
	}
	if got := pricingURLFromMessage("no url here"); got != "" {
		t.Fatalf("empty expected, got %q", got)
	}
}

func TestRecordUpstreamFailurePlanGateCoolsPair(t *testing.T) {
	base := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	restore := withCooldownNow(t, base)
	defer restore()

	recordUpstreamFailure("pg1", "dfmodel", 0, "qoder upstream error: plan_gate: code=112 套餐额度或模型范围限制（同账号其他模型通常不受影响）— 升级套餐或换模型后重试: https://qoder.com.cn/pricing?client=qoder")
	if !modelIsCooling("pg1", "dfmodel") {
		t.Fatal("plan_gate must cool the pair")
	}
	if d := modelCoolingUntil("pg1", "dfmodel").Sub(base); d != modelCooldownRateLimit {
		t.Fatalf("plan_gate cooldown = %v, want %v", d, modelCooldownRateLimit)
	}
	if modelIsCooling("pg1", "gmodel") {
		t.Fatal("other models must stay live")
	}
	// The copy must never read as hard-credit (that would skip pair cooling
	// and read account-wide downstream), nor as a soft rate limit.
	if isHardCreditError(0, "plan_gate: code=112 套餐额度或模型范围限制") {
		t.Fatal("plan_gate copy must not classify as hard credit")
	}
	if isSoftRateLimit(0, "plan_gate: code=112 套餐额度或模型范围限制") {
		t.Fatal("plan_gate copy must not classify as soft rate limit")
	}
}
