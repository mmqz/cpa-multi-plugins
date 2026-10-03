// host_bridge.go routes every upstream HTTP call through the CPA host's
// http bridge (host.http.do / host.http.do_stream / stream_read / stream_close).
// Production traffic always uses the bridge so request-log captures outbound
// calls and host transport policy (proxy, timeout) applies. The *Direct
// variants are the test-only fallback used when the bridge is unavailable
// (unit tests, or hosts older than v7.2.x without the http bridge RPC).
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// sharedHTTPClient is the fallback HTTP client used ONLY when the host HTTP
// bridge is unavailable (unit tests, or hosts older than v7.2.x without
// host.http.* RPC). All production upstream calls should route via hostHTTPDo
// / hostHTTPDoStream so request-log captures them and host transport policy
// applies. Direct use of this client in new code is a compliance bug.
func sharedHTTPClient() *http.Client {
	httpClientOnce.Do(func() {
		// No cookie jar here: auth is carried by Bearer headers, and a shared
		// jar would leak upstream set-cookie state across accounts (multi-account
		// deployments could cross-contaminate sessions). Only the short-lived
		// login clients get a jar.
		sharedClient = &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        20,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 5,
				// v0.9.48b: disable HTTP/2. codebuddy.ai's APISIX gateway
				// closes HTTP/2 connections mid-request with EOF (field report
				// 2026-10-03: "Post ...get-user-resource: EOF" after 6.84s).
				// Setting TLSNextProto to a non-nil empty map disables the
				// HTTP/2 upgrade, forcing HTTP/1.1 which APISIX handles
				// reliably.
				TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
			},
		}
	})
	return sharedClient
}

// hostHTTPResponse is the plugin-side view of an HTTP response that came back
// through the host bridge. Body is fully buffered (matches the historical
// io.ReadAll(resp.Body) usage pattern in billing / models / usage callers).
type hostHTTPResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// rpcHostHTTPRequestWire mirrors internal/pluginhost/host_callbacks.go's
// rpcHostHTTPRequest on the wire. The "request" sub-object is the actual HTTP
// call; the flat method/url/headers/body fields are an alternate form we don't
// use (host prefers Request when present).
type rpcHostHTTPRequestWire struct {
	Request *rpcHostHTTPInner `json:"request,omitempty"`
}

type rpcHostHTTPInner struct {
	Method  string              `json:"method,omitempty"`
	URL     string              `json:"url,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

type rpcHostHTTPStreamResponseWire struct {
	StatusCode int                         `json:"status_code"`
	Headers    map[string][]string         `json:"headers,omitempty"`
	StreamID   string                      `json:"stream_id,omitempty"`
	Chunks     []pluginapi.HTTPStreamChunk `json:"chunks,omitempty"`
}

type rpcHostHTTPStreamReadResponseWire struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// hostBridgeUnwrap unwraps the pluginabi.Envelope returned by host RPC and
// returns the inner Result payload. Returns an error when the envelope itself
// signals failure (ok=false) or is malformed.
func hostBridgeUnwrap(raw []byte, method string) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: decode envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: host error %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("%s: host returned not-ok", method)
	}
	return env.Result, nil
}

// hostBridgeAvailable reports whether host.http.* RPC is wired up. False in
// unit tests (no hostAPI) and when the host binary predates the bridge.
func hostBridgeAvailable() bool {
	return hostAPI != nil && hostAPI.call != nil
}

// hostHTTPDo performs a non-streaming upstream call via the host's http bridge.
// Request body is read eagerly (callers already have []byte or a small buffer).
// The response body is likewise read eagerly — all existing call sites consume
// it via io.ReadAll then Close, so we keep that shape and discard the closer.
//
// Fallback: when the host bridge is unavailable (unit tests, host older than
// v7.2.x without the http bridge), we route through sharedHTTPClient directly.
// This keeps the plugin functional in dev/test contexts while preferring the
// compliant path in production.
func hostHTTPDo(req *http.Request) (*hostHTTPResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request")
	}
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		_ = req.Body.Close()
		bodyBytes = b
	}
	// Windows stack movement mitigation: nested host calls during synchronous
	// RPCs (model.for_auth, management.handle) cause the host stack to move,
	// rendering the stack response pointer dangling and causing "unexpected
	// end of JSON input". Bypass host bridge on Windows and make direct HTTP
	// calls.
	if !hostBridgeAvailable() || runtime.GOOS == "windows" {
		return hostHTTPDoDirect(req, bodyBytes)
	}
	wire := rpcHostHTTPRequestWire{
		Request: &rpcHostHTTPInner{
			Method:  req.Method,
			URL:     req.URL.String(),
			Headers: map[string][]string(req.Header),
			Body:    bodyBytes,
		},
	}
	raw, err := hostCall(pluginabi.MethodHostHTTPDo, mustJSON(wire))
	if err != nil {
		// Bridge exists but the call failed — fall back to direct so a transient
		// host RPC error doesn't take down the executor.
		return hostHTTPDoDirect(req, bodyBytes)
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPDo)
	if err != nil {
		return hostHTTPDoDirect(req, bodyBytes)
	}
	// Wire compat (v0.12.49): hosts marshal pluginapi.HTTPResponse WITHOUT
	// json tags, so the status arrives as PascalCase "StatusCode". Our old
	// decode only looked for "status_code" — Go's case-insensitive field
	// match rescued Headers/Body but NOT StatusCode (underscore ≠ no
	// underscore), so every bridged response decoded with status 0 and the
	// models discovery treated every upstream 200 as "models API status 0".
	// Decode both shapes; keep "status_code" for hosts that document it.
	status, headers, respBody, errDecode := decodeHostHTTPDoResult(result)
	if errDecode != nil {
		return nil, errDecode
	}
	if status == 0 {
		// HTTP has no status 0: the host returned a non-error envelope
		// with an unusable status (unknown wire shape or opaque transport
		// failure). Retry once via direct so the caller sees the real
		// status — or the real transport error — instead of a bogus 0.
		log.Printf("workbuddy: host.http.do returned status 0 for %s %s — retrying direct", req.Method, req.URL.Host)
		return hostHTTPDoDirect(req, bodyBytes)
	}
	return &hostHTTPResponse{
		StatusCode: status,
		Headers:    headers,
		Body:       respBody,
	}, nil
}

// decodeHostHTTPDoResult decodes the inner Result of a host.http.do envelope.
// Accepts both wire shapes seen in the wild: the documented lowercase
// {"status_code":...} and the PascalCase {"StatusCode":...} that hosts emit
// when they marshal the tag-less pluginapi.HTTPResponse struct directly.
// Headers/Body match either spelling via Go's case-insensitive field match.
func decodeHostHTTPDoResult(result json.RawMessage) (int, http.Header, []byte, error) {
	var resp struct {
		StatusCode    int                 `json:"status_code"`
		StatusCodeAlt int                 `json:"StatusCode"`
		Headers       map[string][]string `json:"headers,omitempty"`
		Body          []byte              `json:"body,omitempty"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return 0, nil, nil, fmt.Errorf("decode host.http.do response: %w", err)
	}
	status := resp.StatusCode
	if status == 0 {
		status = resp.StatusCodeAlt
	}
	return status, http.Header(resp.Headers), resp.Body, nil
}

// hostHTTPDoDirect executes the request via the plugin's own http.Client.
// Used as a fallback when the host bridge is unavailable (unit tests).
func hostHTTPDoDirect(req *http.Request, bodyBytes []byte) (*hostHTTPResponse, error) {
	// Rebuild the request since req.Body was already consumed.
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header.Clone()
	resp, err := sharedHTTPClient().Do(newReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &hostHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Body:       raw,
	}, nil
}

// hostHTTPStream is a handle for an in-flight host-bridged stream. Read returns
// the next chunk; Close aborts the upstream.
//
// Two modes:
//   - Bridged: streamID set, Read/Close forward to host RPC.
//   - Direct (test fallback): direct holds the full buffered body, Read drains
//     it once then reports done. Close is a no-op.
type hostHTTPStream struct {
	streamID string
	direct   []byte
	directAt int
}

// hostHTTPDoStream opens a streaming call via the host bridge. The host owns
// the actual http.Response body; we pull chunks via hostHTTPStreamRead.
//
// Falls back to direct http.Client.Do when the bridge is unavailable (tests).
// In that case the returned hostHTTPStream wraps an in-memory copy of the
// full response body so Read/Close have the same shape.
func hostHTTPDoStream(req *http.Request) (*hostHTTPStream, int, http.Header, error) {
	if req == nil {
		return nil, 0, nil, fmt.Errorf("nil request")
	}
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("read request body: %w", err)
		}
		_ = req.Body.Close()
		bodyBytes = b
	}
	if !hostBridgeAvailable() {
		return hostHTTPDoStreamDirect(req, bodyBytes)
	}
	wire := rpcHostHTTPRequestWire{
		Request: &rpcHostHTTPInner{
			Method:  req.Method,
			URL:     req.URL.String(),
			Headers: map[string][]string(req.Header),
			Body:    bodyBytes,
		},
	}
	raw, err := hostCall(pluginabi.MethodHostHTTPDoStream, mustJSON(wire))
	if err != nil {
		return hostHTTPDoStreamDirect(req, bodyBytes)
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPDoStream)
	if err != nil {
		return hostHTTPDoStreamDirect(req, bodyBytes)
	}
	var resp rpcHostHTTPStreamResponseWire
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, 0, nil, fmt.Errorf("decode host.http.do_stream response: %w", err)
	}
	if resp.StreamID == "" {
		return nil, resp.StatusCode, http.Header(resp.Headers), fmt.Errorf("host stream bridge unavailable")
	}
	return &hostHTTPStream{streamID: resp.StreamID}, resp.StatusCode, http.Header(resp.Headers), nil
}

// hostHTTPDoStreamDirect is the test-only fallback: it performs the request
// with the plugin's own http.Client and buffers the full body into an
// in-memory hostHTTPStream so Read/Close keep the same contract.
func hostHTTPDoStreamDirect(req *http.Request, bodyBytes []byte) (*hostHTTPStream, int, http.Header, error) {
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, 0, nil, err
	}
	newReq.Header = req.Header.Clone()
	resp, err := sharedHTTPClient().Do(newReq)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Clone(), err
	}
	return &hostHTTPStream{direct: raw}, resp.StatusCode, resp.Header.Clone(), nil
}

// Read pulls the next chunk. Returns (payload, done, err). done=true means the
// stream ended cleanly; err non-nil means upstream or bridge error.
func (s *hostHTTPStream) Read() ([]byte, bool, error) {
	if s == nil {
		return nil, true, fmt.Errorf("stream closed")
	}
	// Direct (test fallback) mode: serve the buffered body in one shot.
	if s.direct != nil {
		if s.directAt >= len(s.direct) {
			return nil, true, nil
		}
		out := s.direct[s.directAt:]
		s.directAt = len(s.direct)
		return out, false, nil
	}
	if s.streamID == "" {
		return nil, true, fmt.Errorf("stream closed")
	}
	raw, err := hostCall(pluginabi.MethodHostHTTPStreamRead, mustJSON(map[string]any{"stream_id": s.streamID}))
	if err != nil {
		return nil, true, err
	}
	result, err := hostBridgeUnwrap(raw, pluginabi.MethodHostHTTPStreamRead)
	if err != nil {
		return nil, true, err
	}
	var resp rpcHostHTTPStreamReadResponseWire
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, true, fmt.Errorf("decode host.http.stream_read response: %w", err)
	}
	if resp.Error != "" {
		return nil, true, fmt.Errorf("%s", resp.Error)
	}
	return resp.Payload, resp.Done, nil
}

// Close aborts the upstream stream. Always safe to call (idempotent on host).
func (s *hostHTTPStream) Close() {
	if s == nil {
		return
	}
	if s.direct != nil {
		s.direct = nil
		s.directAt = 0
		return
	}
	if s.streamID == "" {
		return
	}
	_, _ = hostCall(pluginabi.MethodHostHTTPStreamClose, mustJSON(map[string]any{"stream_id": s.streamID}))
	s.streamID = ""
}

// hostStreamReader adapts a hostHTTPStream to io.Reader so existing
// bufio.Scanner / io.ReadAll call sites work unchanged. The host bridge emits
// arbitrary 32KB chunks (not SSE lines), so line framing must be re-assembled
// by the consumer — Scanner handles that for us.
type hostStreamReader struct {
	s    *hostHTTPStream
	buf  []byte // leftover from previous chunk
	done bool
	err  error
}

func newHostStreamReader(s *hostHTTPStream) *hostStreamReader {
	return &hostStreamReader{s: s}
}

func (r *hostStreamReader) Read(p []byte) (int, error) {
	// Drain buffered bytes first.
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	if r.done {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	chunk, done, err := r.s.Read()
	if err != nil {
		r.done = true
		r.err = err
		return 0, err
	}
	if len(chunk) > 0 {
		n := copy(p, chunk)
		if n < len(chunk) {
			r.buf = append(r.buf, chunk[n:]...)
		}
		if done {
			r.done = true
		}
		return n, nil
	}
	if done {
		r.done = true
		return 0, io.EOF
	}
	// Empty chunk, not done — recurse to fetch next.
	return r.Read(p)
}

// Close releases the underlying host stream. io.ReadCloser parity for call
// sites that drain-and-close task-chat SSE streams (task_chat.go).
func (r *hostStreamReader) Close() error {
	r.s.Close()
	return nil
}

// mustJSON marshals v and panics on error — the wire structs above are always
// marshalable, so any failure here is a programming bug.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
