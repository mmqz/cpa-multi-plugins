package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func disc(id, name string, ctx int64, disabled bool) discoveredModel {
	m := discoveredModel{ID: id, Name: name, Disabled: disabled}
	if ctx > 0 {
		m.ContextWindow = json.RawMessage(jsonNumber(ctx))
	}
	return m
}

func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func discoveryIDs(ms []pluginapi.ModelInfo) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// TestModelsFromDiscoveryCLIBase pins the base behavior: cli agent IDs in
// payload order, disabled cli entries skipped, missing cli entries skipped,
// metadata (context window / max tokens) carried through.
func TestModelsFromDiscoveryCLIBase(t *testing.T) {
	models := []discoveredModel{
		disc("deepseek-v4-pro", "DeepSeek V4 Pro", 1000000, false),
		disc("deepseek-v4-flash", "DeepSeek V4 Flash", 1000000, false),
		disc("retired-model", "Retired", 128000, true), // disabled upstream
	}
	cli := []string{"deepseek-v4-flash", "deepseek-v4-pro", "ghost-model"}
	got := modelsFromDiscovery(models, cli)
	if got == nil {
		t.Fatal("nil result")
	}
	want := []string{"deepseek-v4-flash", "deepseek-v4-pro"} // payload order, disabled/ghost dropped
	if got2 := discoveryIDs(got); len(got2) != len(want) {
		t.Fatalf("ids=%v want %v", got2, want)
	} else {
		for i := range want {
			if got2[i] != want[i] {
				t.Fatalf("ids=%v want %v", got2, want)
			}
		}
	}
	if got[0].ContextLength != 1000000 {
		t.Errorf("ctx=%d want 1000000", got[0].ContextLength)
	}
	if got[0].Name != "DeepSeek V4 Flash" {
		t.Errorf("name=%q", got[0].Name)
	}
}

// TestModelsFromDiscoveryPromotesNewModels is the v0.9.8 regression lock:
// a model present and ENABLED in data.models but missing from the cli agent
// list (the deepseek-v4.1-flash 2026-09-10 rollout shape) must be advertised,
// not hidden behind the cli gate while the official client already shows it.
func TestModelsFromDiscoveryPromotesNewModels(t *testing.T) {
	models := []discoveredModel{
		disc("deepseek-v4-flash", "DeepSeek V4 Flash", 1000000, false),
		disc("deepseek-v4.1-flash", "DeepSeek V4.1 Flash", 1000000, false), // new, cli list lags
		disc("ide-only-beta", "", 256000, false),                           // unnamed promoted entry gets ID as name
		disc("secret-agent-model", "Secret", 64000, true),                  // disabled → never promoted
	}
	cli := []string{"deepseek-v4-flash"}
	got := modelsFromDiscovery(models, cli)
	ids := discoveryIDs(got)
	if len(ids) != 3 {
		t.Fatalf("ids=%v want [deepseek-v4-flash deepseek-v4.1-flash ide-only-beta]", ids)
	}
	if ids[0] != "deepseek-v4-flash" || ids[1] != "deepseek-v4.1-flash" {
		t.Fatalf("cli base must stay first: %v", ids)
	}
	if got[1].Name != "DeepSeek V4.1 Flash" || got[1].ContextLength != 1000000 {
		t.Errorf("promoted meta: %+v", got[1])
	}
	if got[2].Name != "ide-only-beta" {
		t.Errorf("unnamed promoted entry must fall back to ID as name: %q", got[2].Name)
	}
}

// TestModelsFromDiscoveryCLIMissing: upstream renaming/removing the cli agent
// must not zero out discovery (pre-v0.9.8 hard error → stale static fallback);
// enabled data.models alone still produce the list.
func TestModelsFromDiscoveryCLIMissing(t *testing.T) {
	models := []discoveredModel{
		disc("deepseek-v4-flash", "DeepSeek V4 Flash", 1000000, false),
		disc("glm-5.2", "GLM-5.2", 1000000, false),
	}
	if got := modelsFromDiscovery(models, nil); len(got) != 2 {
		t.Fatalf("cli-missing discovery must still serve data.models, got %v", discoveryIDs(got))
	}
	if got := modelsFromDiscovery(nil, nil); got != nil {
		t.Fatalf("empty payload must yield nil (caller errors → static fallback), got %v", got)
	}
	if got := modelsFromDiscovery([]discoveredModel{disc("x", "X", 0, true)}, nil); got != nil {
		t.Fatalf("all-disabled payload must yield nil, got %v", discoveryIDs(got))
	}
}

// TestRawJSONI64 covers number / numeric-string / null / missing shapes for
// the contextWindow/maxTokens fields (some gateways emit strings).
func TestRawJSONI64(t *testing.T) {
	cases := []struct {
		raw  string
		want int64
	}{
		{`1000000`, 1000000},
		{`"262144"`, 262144},
		{`null`, 0},
		{``, 0},
		{`{"x":1}`, 0},
	}
	for _, c := range cases {
		if got := rawJSONI64(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("rawJSONI64(%s)=%d want %d", c.raw, got, c.want)
		}
	}
}

// TestFetchDynamicModelsRecordsSourceState is the v0.9.9 regression lock for
// the diagnostics trail: a discovery failure must leave a visible reason
// (previously the SILENT fallback that left realms stuck on a thin static
// catalog with no trace of why), and a later successful discovery must
// replace it. Flat storage shape {"accessToken","region"} keeps the realm cn.
func TestFetchDynamicModelsRecordsSourceState(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()
	orig := discoverModelsFn
	defer func() { discoverModelsFn = orig }()
	storage := []byte(`{"accessToken":"tok","region":"cn"}`)

	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return nil, errors.New("models API status 403")
	}
	got := fetchDynamicModelsFromStorage(storage)
	if len(got) != len(wbModels()) {
		t.Fatalf("fallback must serve the CN static catalog: got %d want %d", len(got), len(wbModels()))
	}
	st := realmModelStateFor("cn")
	if st == nil {
		t.Fatal("discovery failure must record realm state")
	}
	if st.Source != "static (discovery failed)" || st.Count != len(wbModels()) {
		t.Errorf("state source/count = %q/%d", st.Source, st.Count)
	}
	if !strings.Contains(st.LastError, "403") {
		t.Errorf("state last_error must carry the reason, got %q", st.LastError)
	}
	if st.LastErrorA == "" {
		t.Error("state last_error_at must be set")
	}

	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		return realmTestModels("deepseek-v4.1-flash", "glm-5.2"), nil
	}
	got = fetchDynamicModelsFromStorage(storage)
	// Discovery's doctored list [deepseek-v4.1-flash glm-5.2] plus the free-model
	// guarantee merges deepseek-v4-flash and hy4-preview (absent upstream) → 4
	// models. The paid entry leading the list must stay intact.
	if len(got) != 4 || got[0].ID != "deepseek-v4.1-flash" {
		t.Fatalf("successful discovery must be served (with free-model merge): %+v", discoveryIDs(got))
	}
	st = realmModelStateFor("cn")
	if st == nil || st.Source != "discovery" || st.Count != 4 {
		t.Fatalf("discovery state = %+v, want source=discovery count=4", st)
	}
	if st.LastError != "" {
		t.Errorf("successful discovery must clear last_error, got %q", st.LastError)
	}
	if st.FetchedAt == "" || st.AgeSeconds < 0 {
		t.Errorf("fetched_at/age must be recorded: %+v", st)
	}
}

// TestFetchDynamicModelsPinRecordsState: a models_cn pin replaces discovery
// for the realm entirely — the recorded state must say so (this is how a
// stale pin gets caught when "the new model never shows up").
func TestFetchDynamicModelsPinRecordsState(t *testing.T) {
	resetDynamicModelsCache()
	defer resetDynamicModelsCache()
	orig := discoverModelsFn
	defer func() { discoverModelsFn = orig }()
	pinnedModelsMu.Lock()
	origPins := map[string][]string{"cn": append([]string(nil), pinnedModels["cn"]...)}
	pinnedModels["cn"] = []string{"deepseek-v4.1-flash"}
	pinnedModelsMu.Unlock()
	defer func() {
		pinnedModelsMu.Lock()
		pinnedModels["cn"] = origPins["cn"]
		pinnedModelsMu.Unlock()
	}()
	called := false
	discoverModelsFn = func(accessToken, realm, uid string) ([]pluginapi.ModelInfo, error) {
		called = true
		return realmTestModels("x"), nil
	}
	got := fetchDynamicModelsFromStorage([]byte(`{"accessToken":"tok","region":"cn"}`))
	if called {
		t.Fatal("pin must skip discovery entirely")
	}
	if len(got) != 1 || got[0].ID != "deepseek-v4.1-flash" {
		t.Fatalf("pin must be served verbatim: %+v", discoveryIDs(got))
	}
	st := realmModelStateFor("cn")
	if st == nil || !strings.HasPrefix(st.Source, "pin") || st.Count != 1 {
		t.Fatalf("pin state = %+v, want source=pin count=1", st)
	}
}

// TestModelsFromDiscoveryModalityFlags is the v0.9.11 lock for image-input
// advertisement: supportsImages && !disabledMultimodal (Tencent's own
// registration-table statement) maps to SupportedInputModalities
// ["text","image"]; everything else stays un-declared so modality-aware
// clients don't offer attachments a model would reject.
func TestModelsFromDiscoveryModalityFlags(t *testing.T) {
	vision := disc("glm-5v-turbo", "GLM-5V Turbo", 200000, false)
	vision.SupportsImages = true
	textOnly := disc("deepseek-v4-flash", "DeepSeek V4 Flash", 1000000, false)
	textOnly.SupportsImages = true
	textOnly.DisabledMultimodal = true // account-level multimodal switch off
	plain := disc("hy4-preview", "Hy4 Preview", 1000000, false)
	got := modelsFromDiscovery([]discoveredModel{vision, textOnly, plain}, nil)
	if len(got) != 3 {
		t.Fatalf("ids=%v", discoveryIDs(got))
	}
	byID := map[string]pluginapi.ModelInfo{}
	for _, m := range got {
		byID[m.ID] = m
	}
	if m := byID["glm-5v-turbo"]; len(m.SupportedInputModalities) != 2 {
		t.Fatalf("vision modalities = %v, want [text image]", m.SupportedInputModalities)
	}
	if m := byID["deepseek-v4-flash"]; len(m.SupportedInputModalities) != 0 {
		t.Fatalf("disabledMultimodal must stay un-declared, got %v", m.SupportedInputModalities)
	}
	if m := byID["hy4-preview"]; len(m.SupportedInputModalities) != 0 {
		t.Fatalf("no upstream flag must stay un-declared, got %v", m.SupportedInputModalities)
	}
}
