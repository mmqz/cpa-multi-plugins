// auth_keyres.go resolves the coding-plan API key from a fresh OAuth access
// token — the step between "poll says ready" and "credential is usable for
// chat + V4 signing".
//
// The poll-ready payload carries data.{provider}.access_token, which is an
// OAUTH token, not the chat credential. Both known client implementations
// resolve a dedicated plan key through the same business chain right after
// login (TriDefender/zcode-api src/auth/resolver.ts KeyResolver, and the
// official open-source zai-org/ZCode apps/zcode-cli .../auth/coding-plan-api-key.ts
// — the two are line-for-line isomorphic, so this wire contract is an
// account-level API and carries no client fingerprint):
//
//	zai:
//	  1. POST  https://api.z.ai/api/auth/z/login            {"token": oauth}
//	     → biz token (accepts bare {access_token}, camelCase, and the
//	     {code,data:{access_token}} envelope)
//	  2. GET   https://api.z.ai/api/biz/customer/getCustomerInfo
//	     Authorization: Bearer {bizToken}
//	     → pick 默认机构 (fallback: first org) → 默认项目 (fallback: first)
//	  3. GET/POST https://api.z.ai/api/biz/v1/organization/{org}/projects/{proj}/api_keys
//	     reuse the key named "zcode-api-key" (only when its apiKey is a usable
//	     string) or create it
//	  4. GET  .../api_keys/copy/{apiKey} → {secretKey} — REQUIRED for zai
//	     (an empty secret fails the login instead of storing a credential that
//	     can never sign)
//	  final credential = "{apiKey}.{secretKey}"
//
//	bigmodel:
//	  - Authorization on every biz call is the raw OAuth token WITHOUT the
//	    Bearer prefix (both references agree), host bigmodel.cn
//	  - the copy step is best-effort: on failure or empty secret the bare
//	    "{apiKey}" is used (legacy single-segment shape)
//	  final credential = "{apiKey}.{secretKey}" or "{apiKey}"
//
// Envelope success codes: absent | null | 0 | 200 | "0" | "200" (number and
// string forms both seen upstream). Resolution failure fails the login —
// matching both references; there is no retry ladder (the user retries the
// login instead).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	// codingPlanKeyName is the dedicated plan API key both clients look for
	// (and create when missing).
	codingPlanKeyName = "zcode-api-key"
	// defaultOrgMarker / defaultProjMarker: the customer-info tree names the
	// default organization / project; both clients match by substring with a
	// first-entry fallback.
	defaultOrgMarker  = "默认机构"
	defaultProjMarker = "默认项目"
)

// Key resolution bases and the HTTP exit are vars (not consts) so tests can
// point them at an httptest server; production values mirror the bundle
// constants and the login flow's shared client.
var (
	keyResZaiBase      = "https://api.z.ai"
	keyResBigmodelBase = "https://bigmodel.cn"
	keyResHTTPDo       = func(req *http.Request) (*http.Response, error) {
		return sharedHTTPClient().Do(req)
	}
)

// -----------------------------------------------------------------------------
// Envelope handling
// -----------------------------------------------------------------------------

// bizEnvelope mirrors the business-API {code, data, msg} wrapper. Upstream
// also returns bare bodies (no envelope) and string-typed codes; both are
// accepted (code ?? status; 0/200/"0"/"200" all mean success).
type bizEnvelope struct {
	Code    json.RawMessage `json:"code"`
	Status  json.RawMessage `json:"status"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// rawCodeValue unwraps a JSON scalar that may arrive as number or string.
func rawCodeValue(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var num float64
	if err := json.Unmarshal(raw, &num); err == nil {
		if num == float64(int64(num)) {
			return fmt.Sprintf("%d", int64(num)), true
		}
		return fmt.Sprintf("%g", num), true
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str, true
	}
	return "", false
}

// bizCodeOK reports whether an envelope code/status means success. Absent or
// explicit-null both count as success (bare bodies); a present-but-garbage
// type is an error, matching both references' strict success sets.
func bizCodeOK(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	s, ok := rawCodeValue(raw)
	if !ok {
		return false
	}
	return s == "0" || s == "200"
}

// bizRequest performs one business-API call and unwraps the envelope. Bare
// bodies (no recognizable envelope) surface as the whole payload. Non-2xx and
// non-success codes are errors surfaced with the server msg.
func bizRequest(method, fullURL, authorization string, body []byte) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, fullURL, reader)
	if err != nil {
		return nil, err
	}
	// NOTE: authorization is passed through verbatim — zai callers pass
	// "Bearer {token}", bigmodel callers pass the raw OAuth token (no Bearer
	// prefix; both reference clients agree on this asymmetry).
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+zcodeAppVersion)

	resp, err := keyResHTTPDo(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("biz API %s failed: status=%d body=%s", fullURL, resp.StatusCode, truncateRedacted(string(raw), 200))
	}
	var env bizEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("biz API %s: invalid JSON body: %w", fullURL, err)
	}
	codePresent := len(env.Code) > 0 && string(env.Code) != "null"
	statusPresent := len(env.Status) > 0 && string(env.Status) != "null"
	if !codePresent && !statusPresent {
		// Bare body — no envelope; if a data key exists still return it,
		// otherwise the whole body so tolerant callers see top-level fields.
		if len(env.Data) > 0 && string(env.Data) != "null" {
			return env.Data, nil
		}
		return json.RawMessage(raw), nil
	}
	code := env.Code
	if !codePresent {
		code = env.Status
	}
	if !bizCodeOK(code) {
		msg := firstNonEmpty(env.Msg, env.Message)
		codeStr, _ := rawCodeValue(code)
		if msg != "" {
			return nil, fmt.Errorf("biz API %s: error %s: %s", fullURL, codeStr, msg)
		}
		return nil, fmt.Errorf("biz API %s: error %s", fullURL, codeStr)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, nil
	}
	return env.Data, nil
}

// firstNonEmpty lives in stream.go and is reused here.

// -----------------------------------------------------------------------------
// zai: OAuth token → business token
// -----------------------------------------------------------------------------

// zaiLoginData is the (possibly nested) access_token carrier: enveloped
// {code,data:{access_token}}, bare {access_token|accessToken}, or the
// data.data fallback both references tolerate.
type zaiLoginData struct {
	AccessToken    string        `json:"access_token"`
	AccessTokenCam string        `json:"accessToken"`
	Data           *zaiLoginData `json:"data"`
}

func (d *zaiLoginData) token() string {
	if d == nil {
		return ""
	}
	return firstNonEmpty(d.AccessToken, d.AccessTokenCam, d.Data.token())
}

// zaiLoginToken exchanges the OAuth access token for a zai business token.
func zaiLoginToken(oauthToken string) (string, error) {
	body, _ := json.Marshal(map[string]string{"token": oauthToken})
	req, err := http.NewRequest(http.MethodPost, keyResZaiBase+"/api/auth/z/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+zcodeAppVersion)

	resp, err := keyResHTTPDo(req)
	if err != nil {
		return "", fmt.Errorf("z/login failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("z/login failed: status=%d body=%s", resp.StatusCode, truncateRedacted(string(raw), 200))
	}
	var parsed struct {
		zaiLoginData
		Code json.RawMessage `json:"code"`
		Msg  string          `json:"msg"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("z/login: invalid JSON body: %w", err)
	}
	if len(parsed.Code) > 0 && string(parsed.Code) != "null" && !bizCodeOK(parsed.Code) {
		return "", fmt.Errorf("z/login: error: %s", parsed.Msg)
	}
	token := parsed.token()
	if token == "" {
		return "", fmt.Errorf("z/login returned unexpected shape: access_token missing or empty")
	}
	return token, nil
}

// -----------------------------------------------------------------------------
// customer info → org/project selection
// -----------------------------------------------------------------------------

// bizOrgProject is the resolved 默认机构/默认项目 location.
type bizOrgProject struct {
	OrgID  string
	ProjID string
}

// bizProject / bizOrg accept the field spellings both references read:
// organizationId | id | orgId, organizationName | name; projectId | id,
// projectName | name; the org list may arrive as organizations or orgs.
type bizProject struct {
	ProjectID   string `json:"projectId"`
	ID          string `json:"id"`
	ProjectName string `json:"projectName"`
	Name        string `json:"name"`
}

type bizOrg struct {
	OrganizationID   string       `json:"organizationId"`
	ID               string       `json:"id"`
	OrgIDAlt         string       `json:"orgId"`
	OrganizationName string       `json:"organizationName"`
	Name             string       `json:"name"`
	Projects         []bizProject `json:"projects"`
}

type bizCustomerInfo struct {
	Organizations []bizOrg `json:"organizations"`
	Orgs          []bizOrg `json:"orgs"`
}

func (o bizOrg) id() string   { return firstNonEmpty(o.OrganizationID, o.ID, o.OrgIDAlt) }
func (o bizOrg) name() string { return firstNonEmpty(o.OrganizationName, o.Name) }
func (p bizProject) id() string {
	return firstNonEmpty(p.ProjectID, p.ID)
}
func (p bizProject) name() string { return firstNonEmpty(p.ProjectName, p.Name) }

// pickOrgProject selects the default organization/project with first-entry
// fallback, mirroring pickOrgAndProject in both references.
func pickOrgProject(info *bizCustomerInfo) (bizOrgProject, error) {
	orgs := info.Organizations
	if len(orgs) == 0 {
		orgs = info.Orgs
	}
	if len(orgs) == 0 {
		return bizOrgProject{}, fmt.Errorf("no organizations found")
	}
	org := orgs[0]
	for _, o := range orgs {
		if strings.Contains(o.name(), defaultOrgMarker) {
			org = o
			break
		}
	}
	if org.id() == "" {
		return bizOrgProject{}, fmt.Errorf("default organization has no id")
	}
	if len(org.Projects) == 0 {
		return bizOrgProject{}, fmt.Errorf("no projects found in default organization")
	}
	proj := org.Projects[0]
	for _, p := range org.Projects {
		if strings.Contains(p.name(), defaultProjMarker) {
			proj = p
			break
		}
	}
	if proj.id() == "" {
		return bizOrgProject{}, fmt.Errorf("default project has no id")
	}
	return bizOrgProject{OrgID: org.id(), ProjID: proj.id()}, nil
}

// resolveCustomerInfo fetches and reduces the customer-info tree.
func resolveCustomerInfo(base, authorization string) (bizOrgProject, error) {
	raw, err := bizRequest(http.MethodGet, base+"/api/biz/customer/getCustomerInfo", authorization, nil)
	if err != nil {
		return bizOrgProject{}, err
	}
	if len(raw) == 0 {
		return bizOrgProject{}, fmt.Errorf("customer info response is empty")
	}
	var info bizCustomerInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return bizOrgProject{}, fmt.Errorf("customer info: invalid shape: %w", err)
	}
	return pickOrgProject(&info)
}

// -----------------------------------------------------------------------------
// api_keys: find-or-create + copy secret
// -----------------------------------------------------------------------------

// bizApiKeySummary is one entry of the api_keys list / create response.
type bizApiKeySummary struct {
	APIKey string `json:"apiKey"`
	Name   string `json:"name"`
}

// apiKeysURL builds the org/project api_keys collection URL.
func apiKeysURL(base string, loc bizOrgProject) string {
	return base + "/api/biz/v1/organization/" + url.PathEscape(loc.OrgID) +
		"/projects/" + url.PathEscape(loc.ProjID) + "/api_keys"
}

// findOrCreateAPIKey reuses the "zcode-api-key" entry when it carries a
// usable apiKey string; otherwise creates it. A failed list call falls
// through to creation (the proxy client's resilience deviation; the official
// client propagates the error instead).
func findOrCreateAPIKey(base, authorization string, loc bizOrgProject) (string, error) {
	listURL := apiKeysURL(base, loc)
	if raw, err := bizRequest(http.MethodGet, listURL, authorization, nil); err == nil && len(raw) > 0 {
		var keys []bizApiKeySummary
		if err := json.Unmarshal(raw, &keys); err == nil {
			for _, k := range keys {
				if k.Name == codingPlanKeyName && strings.TrimSpace(k.APIKey) != "" {
					return strings.TrimSpace(k.APIKey), nil
				}
			}
		}
	}
	body, _ := json.Marshal(map[string]string{"name": codingPlanKeyName})
	raw, err := bizRequest(http.MethodPost, listURL, authorization, body)
	if err != nil {
		return "", fmt.Errorf("api key creation failed: %w", err)
	}
	var created bizApiKeySummary
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &created)
	}
	if strings.TrimSpace(created.APIKey) == "" {
		return "", fmt.Errorf("api key creation returned unexpected shape: apiKey missing or empty")
	}
	return strings.TrimSpace(created.APIKey), nil
}

// copyAPIKeySecret reads the secret via the copy endpoint. Both spellings
// (secretKey / secret_key) are accepted; empty means "no secret available"
// and the caller decides whether that is fatal.
func copyAPIKeySecret(base, authorization string, loc bizOrgProject, apiKey string) (string, error) {
	copyURL := apiKeysURL(base, loc) + "/copy/" + url.PathEscape(apiKey)
	raw, err := bizRequest(http.MethodGet, copyURL, authorization, nil)
	if err != nil {
		return "", err
	}
	if len(raw) == 0 {
		return "", nil
	}
	var secret struct {
		SecretKey    string `json:"secretKey"`
		SecretKeyAlt string `json:"secret_key"`
	}
	if err := json.Unmarshal(raw, &secret); err != nil {
		return "", nil // tolerate malformed copy bodies; empty is not secret
	}
	return firstNonEmpty(secret.SecretKey, secret.SecretKeyAlt), nil
}

// -----------------------------------------------------------------------------
// full chain
// -----------------------------------------------------------------------------

// resolveCodingPlanKey runs the full post-OAuth resolution chain and returns
// the final chat credential:
//
//	zai:      "{apiKey}.{secretKey}"  (empty secret = login failure)
//	bigmodel: "{apiKey}.{secretKey}" or "{apiKey}" (copy is best-effort)
//
// The oauthToken itself is NOT a chat credential; the returned key is what
// rides Authorization/x-api-key on chat calls and feeds the V4 signer's
// {apiKeyId}.{apiKeySecret} split.
func resolveCodingPlanKey(oauthToken, provider string) (string, error) {
	oauthToken = strings.TrimSpace(oauthToken)
	if oauthToken == "" {
		return "", fmt.Errorf("resolve coding-plan key: empty oauth token")
	}
	var base, authorization string
	switch provider {
	case providerBigmodel:
		// BigModel biz calls authenticate with the raw OAuth token (no
		// Bearer prefix — both references agree).
		base = keyResBigmodelBase
		authorization = oauthToken
	default:
		bizToken, err := zaiLoginToken(oauthToken)
		if err != nil {
			return "", err
		}
		base = keyResZaiBase
		authorization = "Bearer " + bizToken
	}
	loc, err := resolveCustomerInfo(base, authorization)
	if err != nil {
		return "", fmt.Errorf("resolve organization/project: %w", err)
	}
	apiKey, err := findOrCreateAPIKey(base, authorization, loc)
	if err != nil {
		return "", err
	}
	secret, copyErr := copyAPIKeySecret(base, authorization, loc, apiKey)
	if provider == providerZai {
		// zai requires the two-part shape: a missing secret would store a
		// credential that can never sign (both references hard-fail here).
		if copyErr != nil {
			return "", fmt.Errorf("zai api key copy failed: %w", copyErr)
		}
		if secret == "" {
			return "", fmt.Errorf("zai api key copy response is missing secretKey")
		}
		return apiKey + "." + secret, nil
	}
	// bigmodel: best-effort copy — fall back to the bare key.
	if copyErr != nil || secret == "" {
		return apiKey, nil
	}
	return apiKey + "." + secret, nil
}
