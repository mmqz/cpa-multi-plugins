package main

// 上游把账号级配额错误（1005/4008）发成 HTTP 200 + 流内 error 帧。此前这些帧
// 只会作为普通 chunk 转发给客户端，插件还顺手 NoteSuccess，于是宿主既不会换号
// 也不会冷却，坏号一直留在池里。下面锁住"首包门"的三件事：错误在答话之前到来要
// 能变成带状态的失败；已经答过话则维持原样；只有真答过话才记成功。

import (
	"strings"
	"testing"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
)

const soloQuotaSSE = "event: error\ndata: {\"code\":1005,\"message\":\"plan limit\"}\n\n"

const soloAnswerThenErrorSSE = "event: output\ndata: {\"response\":\"你好\"}\n\n" +
	"event: error\ndata: {\"code\":4008,\"message\":\"quota exceeded\"}\n\n"

const soloAnswerSSE = "event: output\ndata: {\"response\":\"你好\"}\n\n" +
	"event: done\ndata: {\"finish_reason\":\"stop\"}\n\n"

// runHead 复刻 handleExecStream 的接线：回调先记录错误，转换器随后才吐出错误帧。
func runHead(t *testing.T, sse string) ([][]byte, *upstream.SOLOStreamError, *soloFaultTracker) {
	t.Helper()
	faults := &soloFaultTracker{}
	ch := convertSOLOStreamToOpenAI(strings.NewReader(sse), "glm-5.2", func(se *upstream.SOLOStreamError) {
		faults.record(se)
	})
	head, se := soloStreamHead(ch, faults)
	return head, se, faults
}

func TestSoloFaultStatusMapsInStreamCodes(t *testing.T) {
	cases := []struct {
		name string
		se   *upstream.SOLOStreamError
		want int
	}{
		{"plan limit 1005", &upstream.SOLOStreamError{Code: 1005, Msg: "plan limit"}, 402},
		{"plan limit 4008", &upstream.SOLOStreamError{Code: 4008, Msg: "Your requests have exceeded the quota"}, 402},
		{"input too large", &upstream.SOLOStreamError{Code: 4001, Msg: "prompt is too long for the model"}, 413},
		{"model lane mismatch", &upstream.SOLOStreamError{Code: 4001, Msg: "We're sorry, the param is invalid."}, 404},
		{"unknown", &upstream.SOLOStreamError{Code: 4023, Msg: "something went wrong"}, 502},
	}
	for _, tc := range cases {
		if got := soloFaultStatus(tc.se); got != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestSoloStreamHeadAbortsBeforeHandoffOnQuotaFrame(t *testing.T) {
	head, se, faults := runHead(t, soloQuotaSSE)
	if se == nil {
		t.Fatalf("a quota frame before any answer must be returned to the executor, head=%q", head)
	}
	if len(head) != 0 {
		t.Fatalf("nothing may be delivered once the request fails: %q", head)
	}
	if got := soloFaultStatus(se); got != 402 {
		t.Fatalf("host would not rotate: status %d, want 402", got)
	}
	if !faults.any() {
		t.Fatal("the failed call must not be recorded as a success")
	}
}

func TestSoloStreamHeadReleasesOnlyAfterAnswerContent(t *testing.T) {
	head, se, faults := runHead(t, soloAnswerSSE)
	if se != nil {
		t.Fatalf("a clean answer must not fail: %v", se)
	}
	joined := ""
	for _, chunk := range head {
		joined += string(chunk)
	}
	if !strings.Contains(joined, "你好") {
		t.Fatalf("the answer frames were not returned for delivery: %q", joined)
	}
	if faults.any() {
		t.Fatal("a clean stream must not carry a fault")
	}
}

func TestSoloStreamHeadLetsAnsweredStreamFailInBand(t *testing.T) {
	head, se, faults := runHead(t, soloAnswerThenErrorSSE)
	if se != nil {
		t.Fatalf("bytes were already on the wire, so the stream must keep going: %v", se)
	}
	if len(head) == 0 {
		t.Fatal("the answered frames must still be delivered")
	}
	if !faults.any() {
		t.Fatal("a later in-stream fault must still block NoteSuccess")
	}
}

func TestSoloFaultTrackerTakeDoesNotConsumeAny(t *testing.T) {
	faults := &soloFaultTracker{}
	if faults.any() || faults.take() != nil {
		t.Fatal("a fresh tracker must be empty")
	}
	se := &upstream.SOLOStreamError{Code: 1005, Msg: "plan limit"}
	faults.record(se)
	if got := faults.take(); got != se {
		t.Fatalf("take() = %v, want the recorded fault", got)
	}
	if faults.take() != nil {
		t.Fatal("take() must consume the fault exactly once")
	}
	if !faults.any() {
		t.Fatal("any() must keep reporting that the call failed, after take() consumed it")
	}
	later := &upstream.SOLOStreamError{Code: 4008, Msg: "later"}
	faults.record(later)
	if got := faults.take(); got != later {
		t.Fatalf("a fault recorded after the first was consumed must still be reportable, got %v", got)
	}
}
