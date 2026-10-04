package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHostHTTPDoDirect_RescueCtxUsesFreshConnection pins the contract that the
// withHTTPRescue context marker switches hostHTTPDoDirect onto
// rescueHTTPClient. Observable fingerprint: DisableKeepAlives makes Go send
// `Connection: close` on the wire, so the server-side header proves which
// transport served the request.
func TestHostHTTPDoDirect_RescueCtxUsesFreshConnection(t *testing.T) {
	var sawCloseHeader atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Connection") == "close" {
			sawCloseHeader.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{}}`))
	}))
	defer srv.Close()

	build := func(ctx context.Context) *http.Request {
		req, err := http.NewRequest(http.MethodPost, srv.URL, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		return req.WithContext(ctx)
	}

	// Pooled path: no rescue marker → keep-alive request (no Connection: close).
	if _, err := hostHTTPDoDirect(build(context.Background()), nil); err != nil {
		t.Fatalf("pooled direct call: %v", err)
	}
	if sawCloseHeader.Load() {
		t.Fatal("pooled path unexpectedly sent Connection: close")
	}

	// Rescue path: marker → rescueHTTPClient → Connection: close on the wire.
	if _, err := hostHTTPDoDirect(build(withHTTPRescue(context.Background())), nil); err != nil {
		t.Fatalf("rescue direct call: %v", err)
	}
	if !sawCloseHeader.Load() {
		t.Fatal("rescue path did not use rescueHTTPClient (no Connection: close seen)")
	}
}

// TestBillingCall_RescueRetryRecoversFromEOF is the v0.9.51 field-report
// scenario: the first attempt dies with a transport EOF (server closes the
// connection without responding — the exact producer of
// `Post ".../get-user-resource": EOF`), the retry lands on the rescue
// transport and succeeds. billingCall must return the 200 payload, not the EOF.
func TestBillingCall_RescueRetryRecoversFromEOF(t *testing.T) {
	orig := billingRetryDelays
	billingRetryDelays = []time.Duration{1 * time.Millisecond}
	defer func() { billingRetryDelays = orig }()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// Abrupt close without any response bytes → client sees EOF
			// (fresh connection, so net/http does not auto-replay it).
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("handler lacks Hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"recovered":true}}`))
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	data, err := billingCall(&storedAuth{}, "/v2/billing/meter/get-user-resource", nil)
	if err != nil {
		t.Fatalf("expected rescue retry to recover, got: %v", err)
	}
	if string(data) != `{"recovered":true}` {
		t.Fatalf("unexpected data: %s", string(data))
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected 2 calls (1 EOF + 1 rescue success), got %d", got)
	}
}

// TestBillingCall_TransportExhaustionWrapped verifies the v0.9.51 exhaustion
// contract: when every attempt dies with a transport error the returned error
// keeps the original `Post "url": EOF` shape (still transient-classified for
// callers) and appends the attempts count + proxy hint.
func TestBillingCall_TransportExhaustionWrapped(t *testing.T) {
	orig := billingRetryDelays
	billingRetryDelays = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond}
	defer func() { billingRetryDelays = orig }()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		hj, _ := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	_, err := billingCall(&storedAuth{}, "/probe", nil)
	if err == nil {
		t.Fatal("expected error after transport exhaustion")
	}
	msg := err.Error()
	if !isTransientBillingErr(err) {
		t.Errorf("exhaustion error must stay transient-classified, got: %s", msg)
	}
	for _, want := range []string{"EOF", "attempts", "HTTPS_PROXY"} {
		if !strings.Contains(msg, want) {
			t.Errorf("exhaustion message missing %q: %s", want, msg)
		}
	}
	// 1 pooled + 3 rescue attempts = 4 total.
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("expected 4 attempts (1 pooled + 3 rescue), got %d", got)
	}
}
