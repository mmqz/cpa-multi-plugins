// plugin_test.go pins the pure decision layers: OAuth URL shaping, frame
// guards, quota policy, auth-file naming, header shapes, and the executor
// body pin. The upstream error-envelope semantics here mirror the guard
// symmetry lessons from the qoder/workbuddy plugins (a 200-OK envelope with
// an error field must never fold into a silent fake success).
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// -----------------------------------------------------------------------------
// oauth.go
// -----------------------------------------------------------------------------

func TestApplyInterstitial(t *testing.T) {
	// The official client serializes the interstitial URL (which itself carries
	// an encoded zcode:// redirect) as a query param — producing the
	// double-encoded form %253A on the wire. Both layers are load-bearing.
	zai := applyInterstitial("https://accounts.zai.org/authorize?x=1", providerZai)
	if !strings.Contains(zai, "redirect_uri=") || !strings.Contains(zai, "zcode%253A%252F%252Foauth%252Fcallback") {
		t.Fatalf("zai interstitial param wrong: %s", zai)
	}
	bm := applyInterstitial("https://open.bigmodel.cn/authorize?x=1", providerBigmodel)
	if !strings.Contains(bm, "redirect=") || strings.Contains(bm, "redirect_uri=") {
		t.Fatalf("bigmodel interstitial param wrong: %s", bm)
	}
}

func TestNormalizeProviderAndPlan(t *testing.T) {
	cases := map[string]string{
		"zai": providerZai, "": providerZai, "ZAI": providerZai,
		"bigmodel": providerBigmodel, "BigModel": providerBigmodel, "zhipu": providerBigmodel,
	}
	for in, want := range cases {
		if got := normalizeProvider(in); got != want {
			t.Fatalf("normalizeProvider(%q)=%q want %q", in, got, want)
		}
	}
	if got := normalizePlan("start"); got != planStart {
		t.Fatalf("normalizePlan(start)=%q", got)
	}
	if got := normalizePlan("coding-plan"); got != planCoding {
		t.Fatalf("normalizePlan(coding-plan)=%q", got)
	}
	if got := normalizePlan("garbage"); got != planCoding {
		t.Fatalf("normalizePlan default must be coding-plan, got %q", got)
	}
}

func TestBuildStoredAuthFromPoll(t *testing.T) {
	var data cliPollData
	if err := json.Unmarshal([]byte(`{
		"status":"ready",
		"token":"plan-jwt",
		"user":{"user_id":"user-42"},
		"zai":{"access_token":"keyid.keysecret"}
	}`), &data); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	sa := buildStoredAuthFromPoll(&data, providerZai)
	if sa.Auth.AccessToken != "keyid.keysecret" {
		t.Fatalf("access token: %q", sa.Auth.AccessToken)
	}
	if sa.Auth.JWT != "plan-jwt" {
		t.Fatalf("jwt: %q", sa.Auth.JWT)
	}
	if sa.Auth.Provider != providerZai || sa.Auth.Plan != planCoding {
		t.Fatalf("provider/plan: %q/%q", sa.Auth.Provider, sa.Auth.Plan)
	}
	if sa.Account.UID != "user-42" {
		t.Fatalf("uid: %q", sa.Account.UID)
	}
	if _, err := uuid.Parse(sa.Auth.DeviceMid); err != nil {
		t.Fatalf("deviceMid must be a UUID: %q", sa.Auth.DeviceMid)
	}
	// Missing user_id → stable derived uid (not empty).
	var noUser cliPollData
	_ = json.Unmarshal([]byte(`{"status":"ready","zai":{"access_token":"a.b"}}`), &noUser)
	sa2 := buildStoredAuthFromPoll(&noUser, providerZai)
	if sa2.Account.UID == "" {
		t.Fatal("derived uid must not be empty")
	}
	if sa2.Account.UID != buildStoredAuthFromPoll(&noUser, providerZai).Account.UID {
		t.Fatal("derived uid must be stable for the same token")
	}
}

// -----------------------------------------------------------------------------
// stream.go — frame guards
// -----------------------------------------------------------------------------

func TestZcodeUnwrapFrame(t *testing.T) {
	// Plain OpenAI chunk passes through.
	body, meaningful, err := zcodeUnwrapFrame(`data: {"choices":[{"delta":{"content":"hi"}}]}`)
	if err != nil || !meaningful || !strings.Contains(body, "delta") {
		t.Fatalf("plain chunk: %q %v %v", body, meaningful, err)
	}
	// [DONE] recognized.
	body, meaningful, err = zcodeUnwrapFrame("data: [DONE]")
	if err != nil || meaningful || body != "[DONE]" {
		t.Fatalf("done frame: %q %v %v", body, meaningful, err)
	}
	// error field in 200 envelope → error (never a fake success).
	if _, _, err = zcodeUnwrapFrame(`data: {"error":{"message":"quota exceeded"}}`); err == nil {
		t.Fatal("error envelope must surface as error")
	}
	// event:error line → error.
	if _, _, err = zcodeUnwrapFrame("event:error"); err == nil {
		t.Fatal("event:error must surface as error")
	}
	// Non-data lines ignored.
	if body, meaningful, err = zcodeUnwrapFrame(": keep-alive"); err != nil || meaningful || body != "" {
		t.Fatalf("comment line: %q %v %v", body, meaningful, err)
	}
}

func TestCleanChunkJSON(t *testing.T) {
	// Empty tool_calls array stripped.
	out := cleanChunkJSON(`{"choices":[{"delta":{"role":"assistant","tool_calls":[]}}]}`)
	if strings.Contains(out, "tool_calls") {
		t.Fatalf("empty tool_calls must be stripped: %s", out)
	}
	// Real tool call content preserved.
	keep := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{}"}}]}}]}`
	if got := cleanChunkJSON(keep); got != keep {
		t.Fatalf("real tool call must be preserved: %s", got)
	}
	// Fully empty delta without finish_reason dropped.
	if got := cleanChunkJSON(`{"choices":[{"delta":{}}]}`); got != "" {
		t.Fatalf("empty delta must be dropped: %q", got)
	}
	// Role-only chunk survives.
	if got := cleanChunkJSON(`{"choices":[{"delta":{"role":"assistant"}}]}`); got == "" {
		t.Fatal("role-only chunk must survive")
	}
}

func TestDecodeNonStreamCompletion(t *testing.T) {
	ok := []byte(`{"id":"c1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"yo"}}],"usage":{"total_tokens":9}}`)
	got, err := decodeNonStreamCompletion(ok, "glm-5.3")
	if err != nil || !strings.Contains(string(got), "chat.completion") {
		t.Fatalf("valid completion: %v %s", err, got)
	}
	if _, err = decodeNonStreamCompletion([]byte(`{"error":{"message":"no quota"}}`), "glm-5.3"); err == nil {
		t.Fatal("200 error envelope must surface")
	}
	if _, err = decodeNonStreamCompletion([]byte(`not json`), "glm-5.3"); err == nil {
		t.Fatal("non-JSON body must surface")
	}
	if _, err = decodeNonStreamCompletion([]byte(`{"choices":[]}`), "glm-5.3"); err == nil {
		t.Fatal("empty choices must surface")
	}
	if _, err = decodeNonStreamCompletion(nil, "glm-5.3"); err == nil {
		t.Fatal("empty body must surface as empty_stream")
	}
}

// -----------------------------------------------------------------------------
// executor.go
// -----------------------------------------------------------------------------

func TestBuildChatBody(t *testing.T) {
	payload := []byte(`{"model":"zcode/glm-5.3","messages":[{"role":"user","content":"hi"}],"stream":false,"temperature":0.7}`)
	body, err := buildChatBody(payload, "glm-5.3", true)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var obj map[string]any
	if json.Unmarshal([]byte(body), &obj) != nil {
		t.Fatalf("body not json: %s", body)
	}
	if obj["model"] != "glm-5.3" {
		t.Fatalf("model pin failed: %v", obj["model"])
	}
	if obj["stream"] != true {
		t.Fatalf("stream pin failed: %v", obj["stream"])
	}
	if _, has := obj["temperature"]; !has {
		t.Fatal("passthrough fields must survive")
	}
	if _, has := obj["messages"]; !has {
		t.Fatal("messages must survive")
	}
}

func TestChatEndpointFor(t *testing.T) {
	zai := &storedAuth{Auth: zcodeTokens{Provider: providerZai}}
	if got := chatEndpointFor(zai); got != openAIBaseZai+"/chat/completions" {
		t.Fatalf("zai endpoint: %s", got)
	}
	bm := &storedAuth{Auth: zcodeTokens{Provider: providerBigmodel}}
	if got := chatEndpointFor(bm); got != openAIBaseBigmodel+"/chat/completions" {
		t.Fatalf("bigmodel endpoint: %s", got)
	}
}

// -----------------------------------------------------------------------------
// identity.go
// -----------------------------------------------------------------------------

func TestIdentityHeaderPlanes(t *testing.T) {
	id := zcodeIdentity()
	llm := buildLlmIdentityHeaders(id)
	if llm["X-ZCode-Agent"] != "glm" {
		t.Fatalf("LLM plane must carry X-ZCode-Agent: %v", llm["X-ZCode-Agent"])
	}
	if _, has := llm["X-Device-Mid"]; has {
		t.Fatal("LLM plane must never carry X-Device-Mid")
	}
	if _, has := llm["X-Client-Language"]; !has {
		t.Fatal("LLM plane always carries language (unknown fallback)")
	}
	if llm["X-Os-Category"] == "" {
		t.Fatal("LLM plane X-Os-Category unconditional")
	}
	ctx1 := buildContextIdentityHeaders(id, "uuid-1")
	if _, has := ctx1["X-ZCode-Agent"]; has {
		t.Fatal("context plane must NOT carry X-ZCode-Agent")
	}
	if ctx1["X-Device-Mid"] != "uuid-1" {
		t.Fatalf("context plane device mid: %v", ctx1["X-Device-Mid"])
	}
	if got := buildContextIdentityHeaders(id, ""); got["X-Device-Mid"] != "" {
		t.Fatal("empty deviceMid must be omitted")
	}
}

func TestNormalizePrintableHeaderValue(t *testing.T) {
	if normalizePrintableHeaderValue(" 3.14.0 ") != "3.14.0" {
		t.Fatal("trim+keep printable")
	}
	if normalizePrintableHeaderValue("bad\nvalue") != "" {
		t.Fatal("control chars dropped")
	}
	if normalizePrintableHeaderValue("") != "" {
		t.Fatal("empty dropped")
	}
}

// -----------------------------------------------------------------------------
// policy.go
// -----------------------------------------------------------------------------

func TestQuotaErrorClassification(t *testing.T) {
	if !isHardQuotaError(402, "") {
		t.Fatal("402 is hard")
	}
	if !isHardQuotaError(429, `{"error":{"message":"insufficient quota"}}`) {
		t.Fatal("quota marker is hard")
	}
	if isSoftRateLimit(429, `{"error":{"message":"insufficient quota"}}`) {
		t.Fatal("hard quota is never soft")
	}
	if !isSoftRateLimit(429, `{"error":{"message":"rate limit"}}`) {
		t.Fatal("429 with rate limit is soft")
	}
	if !isHardQuotaError(400, "余额不足") {
		t.Fatal("chinese marker is hard")
	}
}

func TestNoteSegmentPreservation(t *testing.T) {
	sa := &storedAuth{Auth: zcodeTokens{Provider: providerZai}, Account: zcodeAccount{UID: "u1"}}
	prev := "ZAI · 余100 已用50 池150"
	// Fresh data wins.
	got := displayNoteWithPrev(sa, &creditsSummary{TotalRemain: 90, TotalUsed: 60, TotalSize: 150}, false, prev)
	if !strings.Contains(got, "余90") {
		t.Fatalf("fresh credits must win: %s", got)
	}
	// nil credits → previous segment preserved (never regress to 积分未知).
	got = displayNoteWithPrev(sa, nil, false, prev)
	if !strings.Contains(got, "余100") {
		t.Fatalf("prev segment must survive: %s", got)
	}
	// No prev, no data → placeholder.
	got = displayNoteWithPrev(sa, nil, false, "")
	if !strings.Contains(got, "积分未知") {
		t.Fatalf("placeholder expected: %s", got)
	}
	// creditSegmentFromNote strips provider/disabled heads.
	if seg := creditSegmentFromNote("ZAI · 已禁用 · 余5 已用1"); seg != "余5 已用1" {
		t.Fatalf("segment extraction: %q", seg)
	}
	if seg := creditSegmentFromNote("积分未知"); seg != "" {
		t.Fatalf("placeholder must not resurrect: %q", seg)
	}
}

// -----------------------------------------------------------------------------
// authfile.go
// -----------------------------------------------------------------------------

func TestAuthFileNaming(t *testing.T) {
	sa := &storedAuth{Auth: zcodeTokens{Provider: providerZai}, Account: zcodeAccount{UID: "user/42:x"}}
	if got := authFileNameFor(sa); got != "zcode-zai-user_42_x.json" {
		t.Fatalf("naming: %s", got)
	}
	bm := &storedAuth{Auth: zcodeTokens{Provider: providerBigmodel}, Account: zcodeAccount{UID: "u"}}
	if got := authFileNameFor(bm); got != "zcode-bigmodel-u.json" {
		t.Fatalf("bigmodel naming: %s", got)
	}
	if got := authFileNameFor(nil); got != authFileName {
		t.Fatalf("nil fallback: %s", got)
	}
	if sanitizeUIDForFileName("") != "" {
		t.Fatal("empty uid rejected")
	}
	if sanitizeUIDForFileName("../..") != "_" {
		t.Fatal("pure-traversal uid collapses to underscores")
	}
	// Traversal material never survives: ".." sanitizes to "_" (harmless),
	// and slash-bearing uids lose the separators entirely.
	if got := sanitizeUIDForFileName("../../etc"); strings.Contains(got, "/") || strings.Contains(got, "..") {
		t.Fatalf("traversal survived: %q", got)
	}
}

func TestParseStoredShapes(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"a.b","jwt":"j","provider":"bigmodel"},"account":{"uid":"u"}}`)
	sa, err := parseStored(nested)
	if err != nil || sa.Auth.Provider != providerBigmodel || sa.Auth.JWT != "j" {
		t.Fatalf("nested: %v %+v", err, sa)
	}
	flat := []byte(`{"accessToken":"k","uid":"u2","nickname":"nick","provider":"zai"}`)
	sa2, err := parseStored(flat)
	if err != nil || sa2.Auth.AccessToken != "k" || sa2.Account.Nickname != "nick" {
		t.Fatalf("flat: %v %+v", err, sa2)
	}
	if _, err := parseStored([]byte(`{"auth":{}}`)); err == nil {
		t.Fatal("missing accessToken must fail")
	}
	// Legacy file without provider → zai default.
	var legacy struct {
		Auth zcodeTokens `json:"auth"`
	}
	_ = json.Unmarshal([]byte(`{"auth":{"accessToken":"x"}}`), &legacy)
	if normalizeProvider("") != providerZai {
		t.Fatal("empty provider defaults to zai")
	}
}

// -----------------------------------------------------------------------------
// models.go
// -----------------------------------------------------------------------------

func TestModelCatalogPinned(t *testing.T) {
	models := zcodeModels()
	want := map[string]bool{
		"glm-4.5-air": false, "glm-4.6": false, "glm-4.6v": false, "glm-4.7": false,
		"glm-5": false, "glm-5-turbo": false, "glm-5v-turbo": false, "glm-5.1": false,
		"glm-5.2": false, "glm-5.3": false, "glm-5.3-flash": false,
	}
	for _, m := range models {
		if _, ok := want[m.ID]; !ok {
			t.Fatalf("unexpected model %s", m.ID)
		}
		want[m.ID] = true
		if m.OwnedBy != providerName {
			t.Fatalf("ownedBy: %s", m.OwnedBy)
		}
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("model %s missing from catalog", id)
		}
	}
}

func TestFilterExcludedModelsFreshSlice(t *testing.T) {
	models := zcodeModels()
	filtered := filterExcludedModels(models, hostCfgWithExcluded([]string{"glm-4.6v"}))
	for _, m := range filtered {
		if m.ID == "glm-4.6v" {
			t.Fatal("excluded model leaked")
		}
	}
	if len(models) != 11 {
		t.Fatalf("input catalog must be untouched: %d", len(models))
	}
}

// hostCfgWithExcluded builds a pluginapi.HostConfigSummary with the excluded
// list keyed by this provider.
func hostCfgWithExcluded(list []string) pluginapi.HostConfigSummary {
	return pluginapi.HostConfigSummary{ExcludedModels: map[string][]string{providerName: list}}
}

// -----------------------------------------------------------------------------
// cooldown.go
// -----------------------------------------------------------------------------

func TestCooldownLifecycle(t *testing.T) {
	oldNow := cooldownNowFn
	cooldownNowFn = func() time.Time { return time.Unix(1000000, 0) }
	defer func() { cooldownNowFn = oldNow }()

	markModelCooldown("acct", "glm-5.3", cooldownReasonRateLimit)
	if !modelIsCooling("acct", "glm-5.3") {
		t.Fatal("rate-limit cooldown must be active")
	}
	snap := cooldownSnapshotFor("acct")
	if len(snap) != 1 || snap[0]["model"] != "glm-5.3" {
		t.Fatalf("snapshot: %v", snap)
	}
	// Empty model never freezes the account.
	markModelCooldown("acct", "", cooldownReasonRateLimit)
	if len(cooldownSnapshotFor("acct")) != 1 {
		t.Fatal("empty-model mark must be ignored")
	}
	if n := clearModelCooldown("acct", ""); n != 1 {
		t.Fatalf("clear account: %d", n)
	}
	if modelIsCooling("acct", "glm-5.3") {
		t.Fatal("cleared pair must not cool")
	}
}

func TestRecordUpstreamFailureRouting(t *testing.T) {
	oldNow := cooldownNowFn
	cooldownNowFn = func() time.Time { return time.Unix(1000000, 0) }
	defer func() { cooldownNowFn = oldNow }()

	recordUpstreamFailure("acct2", "glm-5", 429, "too many requests")
	if !modelIsCooling("acct2", "glm-5") {
		t.Fatal("429 must cool the pair")
	}
	clearModelCooldown("acct2", "")
	recordUpstreamFailure("acct2", "glm-5", 402, "insufficient quota")
	if modelIsCooling("acct2", "glm-5") {
		t.Fatal("hard quota must NOT cool the model (account lifecycle handles it)")
	}
	clearModelCooldown("acct2", "")
	recordUpstreamFailure("acct2", "glm-5", 0, "empty_stream: upstream closed")
	if !modelIsCooling("acct2", "glm-5") {
		t.Fatal("empty stream must cool the pair")
	}
}

// -----------------------------------------------------------------------------
// http semantics shared with quota
// -----------------------------------------------------------------------------

func TestQuotaSummarizeBalances(t *testing.T) {
	rows := []zcodeBalance{
		{ShowName: "coding", RemainingUnits: 90, TotalUnits: 100, UsedUnits: 10, UnitType: "tokens"},
		{ShowName: "trial", RemainingAlt: 5, UsedAlt: 1, TotalAlt: 6, UnitTypeAlt: "tokens"},
	}
	sum := summarizeBalances(rows)
	if sum.TotalRemain != 95 || sum.TotalUsed != 11 || sum.TotalSize != 106 {
		t.Fatalf("totals: %+v", sum)
	}
	if sum.PackCount != 2 || len(sum.Packages) != 2 {
		t.Fatalf("packages: %d", sum.PackCount)
	}
	if sum.Packages[0].Name != "coding (tokens)" {
		t.Fatalf("package name: %q", sum.Packages[0].Name)
	}
}

func TestIsCreditsExhausted(t *testing.T) {
	if isCreditsExhausted(nil) {
		t.Fatal("nil is unknown, not exhausted")
	}
	if isCreditsExhausted(&creditsSummary{}) {
		t.Fatal("empty is unknown")
	}
	if !isCreditsExhausted(&creditsSummary{TotalUsed: 10}) {
		t.Fatal("used>0 remain=0 is exhausted")
	}
	if isCreditsExhausted(&creditsSummary{TotalRemain: 5}) {
		t.Fatal("remain>0 not exhausted")
	}
}

// httpMethodGet guards the const spelling used across send paths.
var _ = http.MethodGet
