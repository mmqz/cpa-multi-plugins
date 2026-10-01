package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBillingCall_401HTMLPageClassified (v0.9.47): an nginx/APISIX gateway
// answering the billing call with the "401 Authorization Required" HTML page
// used to surface as "parse failed: invalid character '<'" — a JSON parse
// error that hid the dead credential behind parser noise. The response must
// now be classified as a credential-level rejection (growthHTTPError, status
// 401, session-dead per the growth discipline), the message must say what
// happened, and the call must not be retried.
func TestBillingCall_401HTMLPageClassified(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("<html>\r\n<head><title>401 Authorization Required</title></head>\r\n<body>\r\n<center><h1>401 Authorization Required</h1></center><hr><center>nginx</center>\r\n</body>\r\n</html>"))
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	_, err := billingCall(&storedAuth{}, "/v2/billing/meter/get-user-resource", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if calls != 1 {
		t.Fatalf("call count = %d, want 1 (401 must not retry)", calls)
	}
	if got := growthErrStatus(err); got != http.StatusUnauthorized {
		t.Fatalf("growthErrStatus = %d, want 401", got)
	}
	if !isGrowthSessionDead(err) {
		t.Fatalf("expected session-dead classification, got: %v", err)
	}
	if isTransientBillingErr(err) {
		t.Fatalf("401 gateway page must not be transient/retryable: %v", err)
	}
	msg := err.Error()
	if strings.Contains(msg, "parse failed") {
		t.Fatalf("error still surfaces as parse failure: %q", msg)
	}
	for _, want := range []string{"http 401", "credential rejected"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error missing %q: %q", want, msg)
		}
	}
}

// TestBillingCall_403HTMLPageClassified (v0.9.47): same discipline for the
// forbidden variant of the gateway bounce.
func TestBillingCall_403HTMLPageClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><head><title>403 Forbidden</title></head></html>"))
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	_, err := billingCall(&storedAuth{}, "/v2/billing/meter/checkin-activity-status", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := growthErrStatus(err); got != http.StatusForbidden {
		t.Fatalf("growthErrStatus = %d, want 403", got)
	}
	if !strings.Contains(err.Error(), "credential rejected") {
		t.Fatalf("error not classified: %q", err.Error())
	}
}

// TestBillingCall_200HTMLKeepsParseFailedContract (v0.9.47): a 2xx response
// with a non-JSON body is a genuinely malformed payload, not a credential
// verdict — the historical "parse failed" shape must stay (callers and tests
// match on it, and it must never look session-dead).
func TestBillingCall_200HTMLKeepsParseFailedContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>maintenance</html>"))
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	_, err := billingCall(&storedAuth{}, "/test", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "parse failed") {
		t.Fatalf("2xx non-JSON must keep parse-failed shape: %q", err.Error())
	}
	if growthErrStatus(err) != 0 || isGrowthSessionDead(err) {
		t.Fatalf("2xx non-JSON must not classify as credential rejection: %v", err)
	}
}

// TestBillingCall_401JSONEnvelopeKeepsCodeShape (v0.9.47): a 4xx that DOES
// speak JSON keeps the historical code=... msg=... path — the classification
// only covers non-JSON gateway pages.
func TestBillingCall_401JSONEnvelopeKeepsCodeShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	restore := setBillingBase(srv.URL)
	defer restore()

	_, err := billingCall(&storedAuth{}, "/test", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "code=12153") {
		t.Fatalf("JSON envelope must keep code shape: %q", err.Error())
	}
}
