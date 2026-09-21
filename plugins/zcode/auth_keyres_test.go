// auth_keyres_test.go covers the post-OAuth coding-plan key resolution chain
// (auth_keyres.go): envelope semantics, the zai login/token exchange shapes,
// org/project selection fallbacks, find-or-create key handling, the copy
// (secret) step's provider asymmetry, and the handlePollLogin integration
// that persists the RESOLVED key.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// keyResServer is a recording httptest server with per-path handlers; every
// request is captured in order (path, method, Authorization, body) so tests
// can assert the full call sequence and auth-header shapes.
type keyResServer struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	handles map[string]http.HandlerFunc
	reqs    []seenRequest
}

type seenRequest struct {
	Path string
	Body string
	Auth string
}

func newKeyResServer(t *testing.T) *keyResServer {
	t.Helper()
	k := &keyResServer{t: t, handles: map[string]http.HandlerFunc{}}
	k.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		k.mu.Lock()
		k.reqs = append(k.reqs, seenRequest{Path: r.URL.Path, Body: string(body), Auth: r.Header.Get("Authorization")})
		h, ok := k.handles[r.URL.Path]
		k.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"msg":"no handler for ` + r.URL.Path + `"}`))
			return
		}
		h(w, r)
	}))
	t.Cleanup(k.srv.Close)
	return k
}

// handle registers a JSON response for a path (GET and POST share it).
func (k *keyResServer) handle(path string, status int, body string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.handles[path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// reqsOf returns a copy of the recorded request sequence.
func (k *keyResServer) reqsOf() []seenRequest {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]seenRequest, len(k.reqs))
	copy(out, k.reqs)
	return out
}

// pointBasesAt repoints the resolver bases (and the control-plane base when
// includeControl is set) at the test server, restoring them on cleanup.
func pointBasesAt(t *testing.T, k *keyResServer, includeControl bool) {
	t.Helper()
	oldZai, oldBig, oldDo := keyResZaiBase, keyResBigmodelBase, keyResHTTPDo
	keyResZaiBase = k.srv.URL
	keyResBigmodelBase = k.srv.URL
	keyResHTTPDo = func(req *http.Request) (*http.Response, error) {
		return k.srv.Client().Do(req)
	}
	if includeControl {
		oldCtrl := zcodeAPIBase
		zcodeAPIBase = k.srv.URL + "/api/v1"
		t.Cleanup(func() { zcodeAPIBase = oldCtrl })
	}
	t.Cleanup(func() {
		keyResZaiBase, keyResBigmodelBase, keyResHTTPDo = oldZai, oldBig, oldDo
	})
}

// -----------------------------------------------------------------------------
// envelope semantics
// -----------------------------------------------------------------------------

func TestBizCodeOK(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{``, true},         // absent → bare body
		{`null`, true},     // explicit null
		{`0`, true},        // numeric zero
		{`200`, true},      // numeric 200 (zai biz convention)
		{`"0"`, true},      // string zero
		{`"200"`, true},    // string 200
		{`1`, false},       // non-zero code
		{`"300"`, false},   // string business error
		{`3001`, false},    // parameter error
		{`1.5`, false},     // non-integer scalar
		{`true`, false},    // nonsense type
		{`{"a":1}`, false}, // object is not a code
		{`""`, false},      // empty string code is an error (both references)
	}
	for _, c := range cases {
		if got := bizCodeOK(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("bizCodeOK(%s) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestBizRequestBareBodyPassthrough(t *testing.T) {
	// A bare body (no envelope keys) must surface as the whole payload so
	// tolerant callers can read top-level fields.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiKey":"k1","name":"zcode-api-key"}`))
	}))
	defer srv.Close()
	oldDo := keyResHTTPDo
	keyResHTTPDo = func(req *http.Request) (*http.Response, error) { return srv.Client().Do(req) }
	defer func() { keyResHTTPDo = oldDo }()

	got, err := bizRequest(http.MethodGet, srv.URL+"/x", "", nil)
	if err != nil {
		t.Fatalf("bizRequest: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("payload not a bare body: %v (%s)", err, got)
	}
	if m["apiKey"] != "k1" {
		t.Fatalf("bare passthrough lost fields: %s", got)
	}
}

func TestBizRequestEnvelopeErrorSurfacesMsg(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":3001,"msg":"parameter error","data":null}`))
	}))
	defer srv.Close()
	oldDo := keyResHTTPDo
	keyResHTTPDo = func(req *http.Request) (*http.Response, error) { return srv.Client().Do(req) }
	defer func() { keyResHTTPDo = oldDo }()
	_, err := bizRequest(http.MethodGet, srv.URL+"/x", "", nil)
	if err == nil || !strings.Contains(err.Error(), "parameter error") || !strings.Contains(err.Error(), "3001") {
		t.Fatalf("expected surfaced msg/code, got %v", err)
	}
}

func TestBizRequestHTTPStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`bad gateway`))
	}))
	defer srv.Close()
	oldDo := keyResHTTPDo
	keyResHTTPDo = func(req *http.Request) (*http.Response, error) { return srv.Client().Do(req) }
	defer func() { keyResHTTPDo = oldDo }()
	_, err := bizRequest(http.MethodGet, srv.URL+"/x", "", nil)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected HTTP status error, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// zai: OAuth token → business token
// -----------------------------------------------------------------------------

func TestZaiLoginTokenShapes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{"enveloped", `{"code":0,"data":{"access_token":"biz-1"},"msg":""}`, "biz-1", ""},
		{"bare_snake", `{"access_token":"biz-2"}`, "biz-2", ""},
		{"bare_camel", `{"accessToken":"biz-3"}`, "biz-3", ""},
		{"nested_data_data", `{"data":{"data":{"access_token":"biz-4"}}}`, "biz-4", ""},
		{"enveloped_camel", `{"code":0,"data":{"accessToken":"biz-5"}}`, "biz-5", ""},
		{"empty", `{"code":0,"data":{}}`, "", "unexpected shape"},
		{"biz_error", `{"code":1001,"msg":"bad token"}`, "", "bad token"},
		{"missing", `{"code":0}`, "", "unexpected shape"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/auth/z/login" {
					t.Errorf("path: %s", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "" {
					t.Errorf("z/login must not carry an Authorization header, got %q", r.Header.Get("Authorization"))
				}
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), `"token":"oauth-1"`) {
					t.Errorf("body: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			oldZai, oldDo := keyResZaiBase, keyResHTTPDo
			keyResZaiBase = srv.URL
			keyResHTTPDo = func(req *http.Request) (*http.Response, error) { return srv.Client().Do(req) }
			defer func() { keyResZaiBase, keyResHTTPDo = oldZai, oldDo }()

			got, err := zaiLoginToken("oauth-1")
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("expected error %q, got %v (token=%q)", c.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("token: %q, want %q", got, c.want)
			}
		})
	}
}

func TestZaiLoginHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`unauthorized`))
	}))
	defer srv.Close()
	oldZai, oldDo := keyResZaiBase, keyResHTTPDo
	keyResZaiBase = srv.URL
	keyResHTTPDo = func(req *http.Request) (*http.Response, error) { return srv.Client().Do(req) }
	defer func() { keyResZaiBase, keyResHTTPDo = oldZai, oldDo }()
	if _, err := zaiLoginToken("x"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401 error, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// full chain — zai
// -----------------------------------------------------------------------------

const defaultCustomerJSON = `{"code":0,"data":{"organizations":[
        {"organizationId":"org-x","organizationName":"其他机构","projects":[{"projectId":"p-x","projectName":"其他项目"}]},
        {"organizationId":"org-1","organizationName":"我的默认机构","projects":[{"projectId":"proj-1","projectName":"默认项目"}]}
]}}`

// zaiHappyPath installs the standard handler set: login → customer →
// api_keys (list GET + create POST share the path) → copy (for both key-9
// and key-77 so the reuse path can copy too).
func zaiHappyPath(t *testing.T, k *keyResServer, customer, keys, copyBody string) {
	t.Helper()
	if customer == "" {
		customer = defaultCustomerJSON
	}
	if keys == "" {
		keys = `{"code":0,"data":[]}`
	}
	base := "/api/biz/v1/organization/org-1/projects/proj-1/api_keys"
	createBody := `{"code":0,"data":{"apiKey":"key-9","name":"zcode-api-key"}}`
	k.handle("/api/auth/z/login", 200, `{"code":0,"data":{"access_token":"biz-tok"}}`)
	k.handle("/api/biz/customer/getCustomerInfo", 200, customer)
	k.mu.Lock()
	k.handles[base] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(createBody))
			return
		}
		_, _ = w.Write([]byte(keys))
	}
	k.mu.Unlock()
	k.handle(base+"/copy/key-9", 200, copyBody)
	k.handle(base+"/copy/key-77", 200, copyBody)
}

func TestResolveZaiKeyFullChain(t *testing.T) {
	k := newKeyResServer(t)
	pointBasesAt(t, k, false)
	// Empty list → create → copy.
	zaiHappyPath(t, k, "", `{"code":0,"data":[]}`, `{"code":0,"data":{"secretKey":"sec-9"}}`)

	got, err := resolveCodingPlanKey("oauth-abc", providerZai)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "key-9.sec-9" {
		t.Fatalf("final key: %q", got)
	}

	// Request sequence: login → customer → list(GET) → create(POST) → copy.
	want := []string{
		"/api/auth/z/login",
		"/api/biz/customer/getCustomerInfo",
		"/api/biz/v1/organization/org-1/projects/proj-1/api_keys",
		"/api/biz/v1/organization/org-1/projects/proj-1/api_keys",
		"/api/biz/v1/organization/org-1/projects/proj-1/api_keys/copy/key-9",
	}
	reqs := k.reqsOf()
	if len(reqs) != len(want) {
		t.Fatalf("request count: %d, want %d (%v)", len(reqs), len(want), paths(reqs))
	}
	for i, p := range want {
		if reqs[i].Path != p {
			t.Errorf("req[%d].path = %s, want %s", i, reqs[i].Path, p)
		}
	}
	// Create call must POST the key name.
	if !strings.Contains(reqs[3].Body, `"name":"zcode-api-key"`) {
		t.Fatalf("create body: %s", reqs[3].Body)
	}
	// Biz calls ride "Bearer {bizToken}"; the login call carries none.
	for _, r := range reqs[1:] {
		if r.Auth != "Bearer biz-tok" {
			t.Errorf("biz call %s Authorization = %q, want Bearer biz-tok", r.Path, r.Auth)
		}
	}
	if reqs[0].Auth != "" {
		t.Errorf("z/login Authorization = %q, want none", reqs[0].Auth)
	}
}

func paths(reqs []seenRequest) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.Path
	}
	return out
}

func TestResolveZaiKeyExistingKeyReused(t *testing.T) {
	// A well-formed existing "zcode-api-key" entry is reused (no create POST).
	k := newKeyResServer(t)
	pointBasesAt(t, k, false)
	zaiHappyPath(t, k, "", `{"code":0,"data":[{"apiKey":"","name":"zcode-api-key"},{"apiKey":"key-77","name":"zcode-api-key"}]}`, `{"code":0,"data":{"secretKey":"sec-77"}}`)
	got, err := resolveCodingPlanKey("oauth-abc", providerZai)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "key-77.sec-77" {
		t.Fatalf("final key: %q", got)
	}
	// Reuse path: exactly one api_keys hit (GET), no create POST.
	reqs := k.reqsOf()
	apiKeysHits := 0
	for _, r := range reqs {
		if strings.HasSuffix(r.Path, "/api_keys") {
			apiKeysHits++
		}
	}
	if apiKeysHits != 1 {
		t.Fatalf("existing key must not trigger create; api_keys hits = %d", apiKeysHits)
	}
}

func TestResolveZaiKeySecretRequired(t *testing.T) {
	// Empty secret on zai = login failure (a credential that can never sign).
	k := newKeyResServer(t)
	pointBasesAt(t, k, false)
	zaiHappyPath(t, k, "", "", `{"code":0,"data":{"secretKey":""}}`)
	if _, err := resolveCodingPlanKey("oauth-abc", providerZai); err == nil ||
		!strings.Contains(err.Error(), "missing secretKey") {
		t.Fatalf("expected missing secretKey error, got %v", err)
	}
	// Copy endpoint hard-fails → same.
	k2 := newKeyResServer(t)
	pointBasesAt(t, k2, false)
	zaiHappyPath(t, k2, "", "", `{"code":5001,"msg":"copy denied"}`)
	if _, err := resolveCodingPlanKey("oauth-abc", providerZai); err == nil ||
		!strings.Contains(err.Error(), "copy failed") {
		t.Fatalf("expected copy failed error, got %v", err)
	}
}

func TestResolveZaiKeyOrgProjectFallbacks(t *testing.T) {
	cases := []struct {
		name     string
		customer string
		wantErr  string
	}{
		{"orgs_alias", `{"code":0,"data":{"orgs":[{"id":"org-1","name":"x默认机构x","projects":[{"id":"proj-1","name":"我的默认项目"}]}]}}`, ""},
		{"first_fallback", `{"code":0,"data":{"organizations":[{"organizationId":"org-1","projects":[{"projectId":"proj-1"}]}]}}`, ""},
		{"no_orgs", `{"code":0,"data":{"organizations":[]}}`, "no organizations"},
		{"no_projects", `{"code":0,"data":{"organizations":[{"organizationId":"org-1","projects":[]}]}}`, "no projects"},
		{"empty_body", `{"code":0,"data":{}}`, "no organizations"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newKeyResServer(t)
			pointBasesAt(t, k, false)
			zaiHappyPath(t, k, c.customer, "", `{"code":0,"data":{"secretKey":"s"}}`)
			got, err := resolveCodingPlanKey("oauth-abc", providerZai)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("expected error %q, got %v (key=%q)", c.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != "key-9.s" {
				t.Fatalf("key: %q", got)
			}
		})
	}
}

func TestResolveKeyEmptyOAuthToken(t *testing.T) {
	if _, err := resolveCodingPlanKey("  ", providerZai); err == nil {
		t.Fatal("empty oauth token must fail fast")
	}
}

// -----------------------------------------------------------------------------
// full chain — bigmodel
// -----------------------------------------------------------------------------

func TestResolveBigmodelKeyRawAuthAndBestEffortCopy(t *testing.T) {
	cases := []struct {
		name    string
		copy    string
		want    string
		wantErr string
	}{
		{"copy_ok", `{"code":0,"data":{"secretKey":"sec-bm"}}`, "key-bm.sec-bm", ""},
		{"copy_empty", `{"code":0,"data":{"secretKey":""}}`, "key-bm", ""},
		{"copy_biz_error", `{"code":5001,"msg":"denied"}`, "key-bm", ""},
		{"copy_malformed", `not-json`, "key-bm", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newKeyResServer(t)
			pointBasesAt(t, k, false)
			const customer = `{"code":0,"data":{"organizations":[{"organizationId":"org-bm","organizationName":"默认机构","projects":[{"projectId":"proj-bm","projectName":"默认项目"}]}]}}`
			base := "/api/biz/v1/organization/org-bm/projects/proj-bm/api_keys"
			k.handle("/api/biz/customer/getCustomerInfo", 200, customer)
			k.handle(base, 200, `{"code":0,"data":[{"apiKey":"key-bm","name":"zcode-api-key"}]}`)
			k.handle(base+"/copy/key-bm", 200, c.copy)

			got, err := resolveCodingPlanKey("oauth-bm-1", providerBigmodel)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("expected error %q, got %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("key: %q, want %q", got, c.want)
			}
			// Every bigmodel biz call must carry the RAW OAuth token — no Bearer.
			reqs := k.reqsOf()
			if len(reqs) < 2 {
				t.Fatalf("expected ≥2 biz calls, got %v", paths(reqs))
			}
			for _, r := range reqs {
				if r.Path == "/api/auth/z/login" {
					t.Fatalf("bigmodel must not call z/login")
				}
				if r.Auth != "oauth-bm-1" {
					t.Errorf("bigmodel biz call %s Authorization = %q, want raw oauth token", r.Path, r.Auth)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------------
// handlePollLogin integration — the persisted credential is the RESOLVED key
// -----------------------------------------------------------------------------

func TestPollLoginPersistsResolvedKey(t *testing.T) {
	k := newKeyResServer(t)
	pointBasesAt(t, k, true) // control plane (oauth poll) + resolver bases
	const customer = `{"code":0,"data":{"organizations":[{"organizationId":"org-1","organizationName":"默认机构","projects":[{"projectId":"proj-1","projectName":"默认项目"}]}]}}`
	base := "/api/biz/v1/organization/org-1/projects/proj-1/api_keys"
	k.handle("/api/v1/oauth/cli/poll/flow-1", 200, `{"code":0,"data":{"status":"ready","token":"plan-jwt","user":{"user_id":"user-42"},"zai":{"access_token":"oauth-tok-1"}}}`)
	k.handle("/api/auth/z/login", 200, `{"code":0,"data":{"access_token":"biz-tok"}}`)
	k.handle("/api/biz/customer/getCustomerInfo", 200, customer)
	k.handle(base, 200, `{"code":0,"data":[{"apiKey":"key-9","name":"zcode-api-key"}]}`)
	k.handle(base+"/copy/key-9", 200, `{"code":0,"data":{"secretKey":"sec-9"}}`)

	loginStates.Store("zc-test", &loginCtx{
		flowID:    "flow-1",
		pollToken: "poll-tok",
		provider:  providerZai,
		expires:   time.Now().Add(time.Minute),
		startedAt: time.Now().UnixNano(),
	})
	t.Cleanup(func() { loginStates.Delete("zc-test") })

	reqBody, _ := json.Marshal(map[string]string{"state": "zc-test"})
	raw, err := handlePollLogin(reqBody)
	if err != nil {
		t.Fatalf("handlePollLogin: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope: %v (%s)", err, raw)
	}
	// AuthLoginPollResponse / AuthData carry no json tags → Go field names.
	var resp struct {
		Status string `json:"Status"`
		Auth   struct {
			StorageJSON []byte `json:"StorageJSON"`
		} `json:"Auth"`
	}
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("result: %v (%s)", err, env.Result)
	}
	if resp.Status != string(pluginapi.AuthLoginStatusSuccess) {
		t.Fatalf("status: %q", resp.Status)
	}
	var stored struct {
		Auth zcodeTokens `json:"auth"`
	}
	if err := json.Unmarshal(resp.Auth.StorageJSON, &stored); err != nil {
		t.Fatalf("storage: %v", err)
	}
	if stored.Auth.AccessToken != "key-9.sec-9" {
		t.Fatalf("persisted AccessToken = %q, want the RESOLVED key", stored.Auth.AccessToken)
	}
	if stored.Auth.OAuthToken != "oauth-tok-1" {
		t.Fatalf("persisted OAuthToken = %q, want the raw login token", stored.Auth.OAuthToken)
	}
	if stored.Auth.JWT != "plan-jwt" {
		t.Fatalf("jwt: %q", stored.Auth.JWT)
	}
}

func TestPollLoginResolutionFailureFailsLogin(t *testing.T) {
	k := newKeyResServer(t)
	pointBasesAt(t, k, true)
	const customer = `{"code":0,"data":{"organizations":[{"organizationId":"org-1","organizationName":"默认机构","projects":[{"projectId":"proj-1","projectName":"默认项目"}]}]}}`
	base := "/api/biz/v1/organization/org-1/projects/proj-1/api_keys"
	k.handle("/api/v1/oauth/cli/poll/flow-2", 200, `{"code":0,"data":{"status":"ready","token":"plan-jwt","user":{"user_id":"user-7"},"zai":{"access_token":"oauth-tok-2"}}}`)
	k.handle("/api/auth/z/login", 200, `{"code":0,"data":{"access_token":"biz-tok-2"}}`)
	k.handle("/api/biz/customer/getCustomerInfo", 200, customer)
	// List returns nothing; create (POST) is a hard failure → resolution
	// fails → the login fails with a clear error (no bogus credential
	// persisted).
	k.mu.Lock()
	k.handles[base] = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":503,"msg":"key service down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":[]}`))
	}
	k.mu.Unlock()

	loginStates.Store("zc-test-2", &loginCtx{
		flowID:    "flow-2",
		pollToken: "poll-tok",
		provider:  providerZai,
		expires:   time.Now().Add(time.Minute),
		startedAt: time.Now().UnixNano(),
	})
	t.Cleanup(func() { loginStates.Delete("zc-test-2") })

	reqBody, _ := json.Marshal(map[string]string{"state": "zc-test-2"})
	_, err := handlePollLogin(reqBody)
	if err == nil || !strings.Contains(err.Error(), "resolve coding-plan key") {
		t.Fatalf("expected resolution failure to fail the login, got %v", err)
	}
}
