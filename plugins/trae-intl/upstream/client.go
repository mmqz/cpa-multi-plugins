// Package upstream implements Trae Intl Web SOLO remote protocol.
//
// Trae Intl 走 Web SOLO remote agent 协议，与 CN 的 IDE SOLO 协议完全不同：
//
//	CN (trae-api-cn.mchost.guru): POST /api/agent/v3/llm_utils_chat + function=solo_work_lite
//	Intl (core-normal.trae.ai): POST /chat_sessions → GET /chat_sessions/{id}/events SSE
//
// Auth: Cloud-IDE-JWT (RS256, ~14d), obtained via OAuth on api.marscode.com.
//
// Based on OmniRoute/open-sse/executors/trae.ts (MIT, by diegosouzapw).
// Translated from TypeScript to Go.
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// Web SOLO remote endpoints
	EpChatSessions = "/chat_sessions"
	EpChatEvents   = "/chat_sessions/%s/events"

	// OAuth endpoints (on api.marscode.com)
	EpLoginGuidance = "/cloudide/api/v3/trae/GetLoginGuidance"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"

	// Intl v1 pay endpoints (CN uses v2)
	EpPayStatus = "/trae/api/v1/pay/ide_user_pay_status"
	EpEntUsage  = "/trae/api/v1/pay/ide_user_ent_usage"

	// Models list (GET, returns available models for selection)
	EpModels = "/models"

	// Stream timeout (5 minutes for long reasoning).
	StreamTimeout = 5 * time.Minute

	// Default base URL (Web SOLO remote API)
	DefaultBase = "https://core-normal.trae.ai/api/remote/v1"
)

// Auth holds Trae Intl credentials.
// Compatible with trae-cn Auth shape (same OAuth flow, different host).
type Auth struct {
	AccessToken  string // Cloud-IDE-JWT
	RefreshToken string
	ExpiresAt    int64 // Unix seconds
	ApiHost      string // OAuth host (api.marscode.com)
	Domain       string // "trae.ai"
	UID          string
	EnterpriseID string
	Nickname     string

	// Web SOLO remote-specific identity fields (from providerSpecificData).
	WebID         string
	BizUserID     string
	UserUniqueID  string
	UserIdentity  string // "Free" / "Pro" / etc.
	Scope         string // "marscode-us" / "marscode"
	Tenant        string // "marscode"
	Region        string // "US-East"
	AppLanguage   string // "en"
	AppVersion    string // "1.0.0.1229"
}

// Client is the Trae Intl upstream client.
type Client struct {
	HTTP       *http.Client
	BaseURL    string // https://core-normal.trae.ai/api/remote/v1
	OAuthHost  string // https://api.marscode.com
	ClientID   string // ono9krqynydwx5
}

// New creates a default client.
func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: StreamTimeout,
	}
	return &Client{
		HTTP:      &http.Client{Timeout: StreamTimeout, Transport: tr},
		BaseURL:   DefaultBase,
		OAuthHost: "https://api.marscode.com",
		ClientID:  "ono9krqynydwx5",
	}
}

// buildHeaders constructs the Web SOLO remote headers.
func buildHeaders(a *Auth) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	h.Set("Content-Type", "application/json")
	h.Set("X-Trae-Client-Type", "web")
	h.Set("X-Preferenced-Language", nonEmpty(a.AppLanguage, "en"))
	h.Set("x-user-region", nonEmpty(a.Region, "US"))
	h.Set("Referer", "https://solo.trae.ai/")
	h.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")
	return h
}

// resolveMode translates model id to SOLO remote mode/strategy/modelName.
//
//	"work" / "auto-work" / "solo-work" → mode=work, strategy=auto, modelName=""
//	"auto" / ""                          → mode=code, strategy=auto, modelName=""
//	other (e.g. "gpt-5.2")              → mode=code, strategy=manual, modelName=model
func resolveMode(model string) (mode, strategy, modelName string) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "work" || m == "auto-work" || m == "solo-work" {
		return "work", "auto", ""
	}
	if m == "" || m == "auto" {
		return "code", "auto", ""
	}
	return "code", "manual", model
}

// commonParams builds the common_params JSON string for chat_sessions.
func commonParams(a *Auth, mode string) string {
	cp := map[string]any{
		"language":         "en-us",
		"app_language":     nonEmpty(a.AppLanguage, "en"),
		"quality":          "stable",
		"app_version":      nonEmpty(a.AppVersion, "1.0.0.1229"),
		"web_id":           a.WebID,
		"user_identity":    nonEmpty(a.UserIdentity, "Free"),
		"is_freshman":      "0",
		"biz_user_id":      a.BizUserID,
		"user_unique_id":    a.UserUniqueID,
		"scope":            nonEmpty(a.Scope, "marscode-us"),
		"tenant":           nonEmpty(a.Tenant, "marscode"),
		"region":           nonEmpty(a.Region, "US-East"),
		"aiRegion":         nonEmpty(a.Region, "US-East"),
		"is_privacy_mode":  0,
		"privacy_mode":     "off",
		"solo_chat_mode":   mode,
	}
	raw, _ := json.Marshal(cp)
	return string(raw)
}

// flattenQuery converts OpenAI messages array to Trae's `query` format:
//
//	JSON.stringify([{ type: "text", data: { content: <flattened text> } }])
//
// where <flattened text> joins all messages with role prefixes:
//
//	[System]\n<content>
//	[Assistant]\n<content>
//	<user content>  (no prefix)
func flattenQuery(messages []map[string]any) string {
	var parts []string
	for _, m := range messages {
		role, _ := m["role"].(string)
		content := ""
		switch c := m["content"].(type) {
		case string:
			content = c
		case []any:
			var sb strings.Builder
			for _, p := range c {
				switch v := p.(type) {
				case string:
					sb.WriteString(v)
				case map[string]any:
					if t, ok := v["text"].(string); ok {
						sb.WriteString(t)
					}
				}
			}
			content = sb.String()
		}
		switch role {
		case "system":
			parts = append(parts, "[System]\n"+content)
		case "assistant":
			parts = append(parts, "[Assistant]\n"+content)
		default:
			parts = append(parts, content)
		}
	}
	text := strings.Join(parts, "\n\n")
	// Trae expects query as a JSON-encoded string of typed content blocks.
	blocks := []map[string]any{
		{"type": "text", "data": map[string]any{"content": text}},
	}
	raw, _ := json.Marshal(blocks)
	return string(raw)
}

// CreateSession calls POST /chat_sessions to start a new chat.
// Returns (sessionId, messageId, error).
func (c *Client) CreateSession(a *Auth, model string, messages []map[string]any) (string, string, error) {
	mode, strategy, modelName := resolveMode(model)
	query := flattenQuery(messages)
	body := map[string]any{
		"mode":             mode,
		"environment_id":   "default",
		"initial_message": map[string]any{
			"chat_session_id":          "",
			"content":                  []any{},
			"query":                    query,
			"model_name":               modelName,
			"agent_type":               "solo_agent_remote",
			"model_selection_strategy": strategy,
			"common_params":             commonParams(a, mode),
		},
		"env":                "remote",
		"auto_create_project": false,
		"origin":             "web",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+EpChatSessions, bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	req.Header = buildHeaders(a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("create_session upstream %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			ChatSessionID string `json:"chat_session_id"`
			MessageID     string `json:"message_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		return "", "", fmt.Errorf("create_session parse: %w", err)
	}
	if env.Code != 0 {
		return "", "", fmt.Errorf("create_session code=%d: %s", env.Code, truncate(string(respBody), 200))
	}
	return env.Data.ChatSessionID, env.Data.MessageID, nil
}

// StreamEvents calls GET /chat_sessions/{id}/events SSE.
// onEvent is called per SSE frame (event, data). Returns when onEvent returns
// true, when "done"/"error" arrives, or when the stream ends.
func (c *Client) StreamEvents(ctx context.Context, a *Auth, sessionID, messageID string, onEvent func(event string, data map[string]any) bool) error {
	u := c.BaseURL + fmt.Sprintf(EpChatEvents, sessionID) + "?reply_to_message_id=" + url.QueryEscape(messageID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header = buildHeaders(a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("events stream upstream %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	var ev string
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			ev = ""
		case strings.HasPrefix(line, "event:"):
			ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var data map[string]any
			if err := json.Unmarshal([]byte(payload), &data); err != nil {
				data = map[string]any{"_raw": payload}
			}
			if onEvent != nil && onEvent(ev, data) {
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// Execute runs a non-streaming chat completion.
// Drives the SSE to completion, accumulates `plan_item.thought` (cumulative),
// then returns a single chat.completion object.
func (c *Client) Execute(ctx context.Context, a *Auth, model string, messages []map[string]any) (map[string]any, error) {
	sessionID, messageID, err := c.CreateSession(a, model, messages)
	if err != nil {
		return nil, err
	}
	var (
		order        []string
		thoughts     = map[string]string{}
		sent         int
		usage        map[string]any
		errorEvent   map[string]any
		fullContent  strings.Builder
	)
	renderNewText := func(data map[string]any) {
		pid, _ := data["id"].(string)
		if pid == "" {
			return
		}
		if _, ok := thoughts[pid]; !ok {
			order = append(order, pid)
		}
		t, _ := data["thought"].(string)
		if len(t) >= len(thoughts[pid]) {
			thoughts[pid] = t
		}
		var sb strings.Builder
		for _, id := range order {
			sb.WriteString(thoughts[id])
		}
		full := sb.String()
		if len(full) > sent {
			fullContent.WriteString(full[sent:])
		}
		sent = len(full)
	}
	err = c.StreamEvents(ctx, a, sessionID, messageID, func(ev string, data map[string]any) bool {
		switch ev {
		case "error":
			errorEvent = data
			return true
		case "token_usage":
			usage = data
		case "plan_item":
			renderNewText(data)
		case "done":
			return true
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if errorEvent != nil {
		code, _ := errorEvent["code"]
		msg, _ := errorEvent["message"].(string)
		return nil, fmt.Errorf("trae %v: %s", code, msg)
	}
	id := fmt.Sprintf("chatcmpl-trae-%d", time.Now().UnixNano())
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": fullContent.String()},
				"finish_reason": "stop",
			},
		},
	}
	if usage != nil {
		if pt, ok := usage["prompt_tokens"].(float64); ok {
			ct := getFloat(usage, "completion_tokens")
			tt := getFloat(usage, "total_tokens")
			if tt == 0 {
				tt = pt + ct
			}
			resp["usage"] = map[string]any{
				"prompt_tokens":     int64(pt),
				"completion_tokens": int64(ct),
				"total_tokens":      int64(tt),
			}
		}
	}
	return resp, nil
}

// ExecuteStream runs a streaming chat completion.
// Returns a reader that yields OpenAI-compatible SSE chunks.
func (c *Client) ExecuteStream(ctx context.Context, a *Auth, model string, messages []map[string]any) (io.Reader, error) {
	sessionID, messageID, err := c.CreateSession(a, model, messages)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		emit := func(obj map[string]any) error {
			raw, _ := json.Marshal(obj)
			_, err := io.WriteString(pw, "data: "+string(raw)+"\n\n")
			return err
		}
		id := fmt.Sprintf("chatcmpl-trae-%d", time.Now().UnixNano())
		created := time.Now().Unix()
		// Emit role chunk first.
		if err := emit(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
		}); err != nil {
			return
		}
		var (
			order      []string
			thoughts   = map[string]string{}
			sent       int
			usage      map[string]any
			errorEvent map[string]any
		)
		renderNewText := func(data map[string]any) string {
			pid, _ := data["id"].(string)
			if pid == "" {
				return ""
			}
			if _, ok := thoughts[pid]; !ok {
				order = append(order, pid)
			}
			t, _ := data["thought"].(string)
			if len(t) >= len(thoughts[pid]) {
				thoughts[pid] = t
			}
			var sb strings.Builder
			for _, id := range order {
				sb.WriteString(thoughts[id])
			}
			full := sb.String()
			var piece string
			if len(full) > sent {
				piece = full[sent:]
			}
			sent = len(full)
			return piece
		}
		err := c.StreamEvents(ctx, a, sessionID, messageID, func(ev string, data map[string]any) bool {
			switch ev {
			case "error":
				errorEvent = data
				return true
			case "token_usage":
				usage = data
			case "plan_item":
				piece := renderNewText(data)
				if piece != "" {
					if err := emit(map[string]any{
						"id":      id,
						"object":  "chat.completion.chunk",
						"created": created,
						"model":   model,
						"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": piece}, "finish_reason": nil}},
					}); err != nil {
						return true
					}
				}
			case "done":
				return true
			}
			return false
		})
		if err != nil {
			_ = emit(map[string]any{"error": map[string]any{"message": err.Error(), "type": "api_error"}})
		} else if errorEvent != nil {
			code := errorEvent["code"]
			msg, _ := errorEvent["message"].(string)
			_ = emit(map[string]any{
				"id":      id,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []any{},
				"error":   map[string]any{"message": fmt.Sprintf("trae %v: %s", code, msg), "type": "api_error"},
			})
		} else {
			_ = emit(map[string]any{
				"id":      id,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			})
			if usage != nil {
				pt := getFloat(usage, "prompt_tokens")
				ct := getFloat(usage, "completion_tokens")
				tt := getFloat(usage, "total_tokens")
				if tt == 0 {
					tt = pt + ct
				}
				_ = emit(map[string]any{
					"id":      id,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []any{},
					"usage":   map[string]any{"prompt_tokens": int64(pt), "completion_tokens": int64(ct), "total_tokens": int64(tt)},
				})
			}
		}
		_, _ = io.WriteString(pw, "data: [DONE]\n\n")
	}()
	return pr, nil
}

// RefreshToken refreshes the access token via ExchangeToken.
func (c *Client) RefreshToken(a *Auth) error {
	if a.RefreshToken == "" {
		return fmt.Errorf("no refreshToken")
	}
	host := a.ApiHost
	if host == "" {
		host = c.OAuthHost
	}
	body := map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": a.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Trae/3.5.66")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("refresh upstream %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	var env struct {
		Result struct {
			Token                string `json:"Token"`
			TokenExpireAt        int64  `json:"TokenExpireAt"`
			TokenExpireDuration  int64  `json:"TokenExpireDuration"`
			RefreshToken         string `json:"RefreshToken"`
			RefreshExpireAt      int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("refresh parse: %w", err)
	}
	if env.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response")
	}
	a.AccessToken = env.Result.Token
	if env.Result.RefreshToken != "" {
		a.RefreshToken = env.Result.RefreshToken
	}
	if env.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(env.Result.TokenExpireAt)
	}
	return nil
}

// GetUserInfo queries the account info (UID, nickname, enterpriseID).
func (c *Client) GetUserInfo(a *Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.OAuthHost
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": "3.5.66"}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Trae/3.5.66")
	req.Header.Set("X-Cloudide-Token", a.AccessToken)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", "", fmt.Errorf("userinfo upstream %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}
	var env struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return env.Result.UserID, env.Result.ScreenName, env.Result.EnterpriseID, nil
}

// FetchModels returns the list of available models for selection.
func (c *Client) FetchModels(a *Auth) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+EpModels, nil)
	if err != nil {
		return nil, err
	}
	req.Header = buildHeaders(a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("models upstream %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var env struct {
		Code int `json:"code"`
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	out := make([]string, 0, len(env.Data))
	for _, m := range env.Data {
		if m.Name != "" {
			out = append(out, m.Name)
		}
	}
	return out, nil
}

// Helpers

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) != "" {
		return s
	}
	return fallback
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

func getFloat(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return 0
}
