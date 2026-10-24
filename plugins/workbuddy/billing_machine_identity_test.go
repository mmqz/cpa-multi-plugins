package main

import (
	"net/http"
	"regexp"
	"testing"
)

// uuidV4Shape pins the official client's device-id shape: hyphenated
// 8-4-4-4-12 with the v4 version nibble and an RFC-4122 variant nibble. A
// bare 36-hex string fails this on purpose — that shape is a gateway
// signature, not a device id (see deriveAccountDeviceID).
var uuidV4Shape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// billingHeaderFor runs billingHeaders once and returns the named header.
func billingHeaderFor(sa *storedAuth, name string) string {
	req, _ := http.NewRequest(http.MethodPost, "https://www.codebuddy.cn/billing/usage", nil)
	billingHeaders(req, sa)
	return req.Header.Get(name)
}

func TestBillingMachineIdentityStablePerAccount(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{UID: "uid-123"}}
	machine := billingHeaderFor(sa, "X-Machine-ID")
	session := billingHeaderFor(sa, "X-Session-ID")
	for i := 0; i < 5; i++ {
		if got := billingHeaderFor(sa, "X-Machine-ID"); got != machine {
			t.Fatalf("X-Machine-ID drifted for the same account: %q then %q", machine, got)
		}
		if got := billingHeaderFor(sa, "X-Session-ID"); got != session {
			t.Fatalf("X-Session-ID drifted for the same account: %q then %q", session, got)
		}
	}
}

func TestBillingMachineIdentityDiffersAcrossAccounts(t *testing.T) {
	a := billingHeaderFor(&storedAuth{Account: storedAccount{UID: "uid-a"}}, "X-Machine-ID")
	b := billingHeaderFor(&storedAuth{Account: storedAccount{UID: "uid-b"}}, "X-Machine-ID")
	if a == b {
		t.Fatalf("two accounts share one X-Machine-ID %q — a pool-wide correlation key", a)
	}
	sa := billingHeaderFor(&storedAuth{Account: storedAccount{UID: "uid-a"}}, "X-Session-ID")
	sb := billingHeaderFor(&storedAuth{Account: storedAccount{UID: "uid-b"}}, "X-Session-ID")
	if sa == sb {
		t.Fatalf("two accounts share one X-Session-ID %q", sa)
	}
}

func TestBillingMachineIdentityShape(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{UID: "uid-shape"}}
	for _, name := range []string{"X-Machine-ID", "X-Session-ID"} {
		if got := billingHeaderFor(sa, name); !uuidV4Shape.MatchString(got) {
			t.Fatalf("%s = %q: not a UUIDv4-shaped device id", name, got)
		}
	}
}

func TestBillingRequestIDFreshAndShaped(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{UID: "uid-req"}}
	first := billingHeaderFor(sa, "X-Request-ID")
	second := billingHeaderFor(sa, "X-Request-ID")
	if first == second {
		t.Fatalf("X-Request-ID repeated %q — real clients mint one per request", first)
	}
	for _, got := range []string{first, second} {
		if len(got) != 32 {
			t.Fatalf("X-Request-ID = %q: want bare 32-hex shape", got)
		}
	}
}

func TestBillingMachineIdentityAnonymousFallback(t *testing.T) {
	// A hand-built snapshot without a UID (tests, pre-persist login flows)
	// must still get stable, well-shaped ids rather than an empty header.
	sa := &storedAuth{}
	machine := billingHeaderFor(sa, "X-Machine-ID")
	if machine == "" || !uuidV4Shape.MatchString(machine) {
		t.Fatalf("empty-UID snapshot got X-Machine-ID = %q", machine)
	}
	if again := billingHeaderFor(sa, "X-Machine-ID"); again != machine {
		t.Fatalf("empty-UID machine id drifted: %q then %q", machine, again)
	}
}
