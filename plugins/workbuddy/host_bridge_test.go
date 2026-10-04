package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// v0.12.49: hosts marshal the tag-less pluginapi.HTTPResponse struct, so the
// status rides as PascalCase "StatusCode". The decoder must accept both that
// shape and the documented lowercase "status_code" — decoding only the latter
// made every bridged response read as status 0 and permanently broke dynamic
// model discovery ("models API status 0" with a healthy upstream).

func TestDecodeHostHTTPDoResultPascalCase(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"StatusCode": 200,
		"Headers":    map[string][]string{"Content-Type": {"application/json"}},
		"Body":       []byte(`{"code":0}`),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	status, headers, body, err := decodeHostHTTPDoResult(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200 (PascalCase wire must decode)", status)
	}
	if headers.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", headers.Get("Content-Type"))
	}
	if string(body) != `{"code":0}` {
		t.Fatalf("body = %q", string(body))
	}
}

func TestDecodeHostHTTPDoResultLowercase(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"status_code": 401,
		"headers":     map[string][]string{"Www-Authenticate": {"Bearer"}},
		"body":        []byte("denied"),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	status, _, body, err := decodeHostHTTPDoResult(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status != 401 {
		t.Fatalf("status = %d, want 401", status)
	}
	if string(body) != "denied" {
		t.Fatalf("body = %q", string(body))
	}
}

func TestDecodeHostHTTPDoResultZeroStatus(t *testing.T) {
	// No status field at all → 0 (caller falls back to a direct request).
	raw, err := json.Marshal(map[string]any{"Body": []byte("x")})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	status, _, _, err := decodeHostHTTPDoResult(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status != 0 {
		t.Fatalf("status = %d, want 0", status)
	}
}

func TestDecodeHostHTTPDoResultMalformed(t *testing.T) {
	if _, _, _, err := decodeHostHTTPDoResult(json.RawMessage(`{invalid`)); err == nil {
		t.Fatal("expected error for malformed result")
	}
}

// hostHTTPDoDirect must hit the real network stack and surface the real
// status — this is the path that rescues bridged status-0 responses.
func TestHostHTTPDoDirectSurfacesRealStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[]}}`))
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/console/enterprises/personal/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := hostHTTPDoDirect(req, nil)
	if err != nil {
		t.Fatalf("direct do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(resp.Body) == 0 {
		t.Fatal("body empty")
	}
}
