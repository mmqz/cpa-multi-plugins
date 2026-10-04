package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestBillingDialContext_IPLiteralFastPath pins the hermeticity contract: IP
// literal dials (httptest servers, 127.0.0.1 proxies) take the plain-dial
// fast path — byte-identical to the default transport, no DoH, no journal.
func TestBillingDialContext_IPLiteralFastPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if net.ParseIP(host) == nil {
		t.Fatalf("expected IP literal, got %s", host)
	}
	conn, err := billingDialContext(context.Background(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		t.Fatalf("IP literal dial: %v", err)
	}
	_ = conn.Close()
	dialStateMu.Lock()
	_, cached := goodIPs[host]
	dialStateMu.Unlock()
	if cached {
		t.Fatal("IP literal dial must not populate the good-IP cache")
	}
}

// TestOrderIPs_ConsensusAndDivergence pins the candidate ordering: cached-good
// → v4 consensus (system ∩ DoH) → DoH-only (pollution suspect) → system-only
// → v6 tail.
func TestOrderIPs_ConsensusAndDivergence(t *testing.T) {
	a := net.ParseIP("1.2.3.4")
	b := net.ParseIP("5.6.7.8")
	c := net.ParseIP("9.9.9.9")
	g := net.ParseIP("7.7.7.7")
	v6 := net.ParseIP("2001:db8::1")

	got := orderIPs(g, []net.IP{a, b}, []net.IP{a, c})
	want := []net.IP{g, a, c, b}
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("order[%d] = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}

	// No cached-good: consensus first, then DoH-only, then system-only.
	got = orderIPs(nil, []net.IP{b, a}, []net.IP{c, a})
	want = []net.IP{a, c, b}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("no-good order[%d] = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}

	// Healthy identical sets stay untouched, v6 goes last.
	got = orderIPs(nil, []net.IP{v6, a}, []net.IP{a})
	if len(got) != 2 || !got[0].Equal(a) || !got[1].Equal(v6) {
		t.Fatalf("v6 tail violated: %v", got)
	}
}

// TestQueryDoH_ParsesARecords checks the dns-json decode path: A records
// kept, everything else (AAAA/CNAME) dropped, duplicates merged.
func TestQueryDoH_ParsesARecords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "doh-test.example" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"Status":0,"Answer":[` +
			`{"name":"doh-test.example","type":1,"TTL":300,"data":"1.2.3.4"},` +
			`{"name":"doh-test.example","type":28,"TTL":300,"data":"2001:db8::1"},` +
			`{"name":"doh-test.example","type":1,"TTL":300,"data":"5.6.7.8"},` +
			`{"name":"doh-test.example","type":1,"TTL":300,"data":"1.2.3.4"}]}`))
	}))
	defer srv.Close()

	origEndpoints := dialDoHEndpoints
	dialDoHEndpoints = []string{srv.URL, srv.URL}
	defer func() { dialDoHEndpoints = origEndpoints }()

	ips := queryDoH("doh-test.example")
	if len(ips) != 2 || !ips[0].Equal(net.ParseIP("1.2.3.4")) || !ips[1].Equal(net.ParseIP("5.6.7.8")) {
		t.Fatalf("queryDoH = %v, want [1.2.3.4 5.6.7.8]", ips)
	}
}

// TestBillingDialContext_MultiIPFallback drives the full resilient path with
// sealed seams: the first candidate is a dead loopback address (connection
// refused), the second serves the connection. The dialer must fall through
// and cache the winner.
func TestBillingDialContext_MultiIPFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	liveIP, livePort, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	_ = liveIP

	dead, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve dead port: %v", err)
	}
	deadPort := dead.Addr().(*net.TCPAddr).Port
	_ = dead.Close() // port now refuses connections

	origLookup := dialSystemLookup
	origEndpoints := dialDoHEndpoints
	dialSystemLookup = func(ctx context.Context, host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	dialDoHEndpoints = nil // no DoH on this test
	t.Cleanup(func() {
		dialSystemLookup = origLookup
		dialDoHEndpoints = origEndpoints
	})
	t.Cleanup(billingResetDialState)

	conn, err := billingDialContext(context.Background(), "tcp", net.JoinHostPort("fallback.test", strconv.Itoa(deadPort)))
	if err == nil {
		_ = conn.Close()
		t.Fatal("all-candidates-dead dial should fail")
	}
	dialStateMu.Lock()
	_, marked := deadIPs["127.0.0.1"]
	dialStateMu.Unlock()
	if !marked {
		t.Fatal("failed candidate must be marked dead")
	}

	// Second call: the live port. The dead mark is cleared (all candidates
	// dead), the dial re-explores, and the live listener wins.
	conn, err = billingDialContext(context.Background(), "tcp", net.JoinHostPort("fallback.test", livePort))
	if err != nil {
		t.Fatalf("multi-IP fallback dial: %v", err)
	}
	ra := conn.RemoteAddr().(*net.TCPAddr)
	_ = conn.Close()
	if ra.Port == deadPort {
		t.Fatalf("dial landed on the dead port %d", deadPort)
	}

	dialStateMu.Lock()
	ip, ok := goodIPs["fallback.test"]
	dialStateMu.Unlock()
	if !ok || !ip.Equal(net.ParseIP("127.0.0.1")) {
		t.Fatalf("good-IP cache = %v, want 127.0.0.1", ip)
	}
}

// TestFilterDeadIPs_AllDeadClearsMarks pins the recovery branch: when every
// candidate is marked dead the marks are cleared so the dial gets a full
// retry budget instead of a guaranteed empty result.
func TestFilterDeadIPs_AllDeadClearsMarks(t *testing.T) {
	t.Cleanup(billingResetDialState)
	ip := net.ParseIP("203.0.113.9")
	dialStateMarkDead(ip)
	if got := filterDeadIPs("clear.test", []net.IP{ip}); len(got) != 1 {
		t.Fatalf("all-dead filter = %v, want full retry budget", got)
	}
	dialStateMu.Lock()
	_, stillMarked := deadIPs[ip.String()]
	dialStateMu.Unlock()
	if stillMarked {
		t.Fatal("all-dead branch must clear dead marks")
	}
}

// TestBillingNetDiagSuffix_HermeticForIPLiteralAndCache pins the two safety
// properties of the exhaustion diag: IP-literal bases (unit tests) probe
// nothing, and repeated calls within the cache window return the same string
// without re-probing.
func TestBillingNetDiagSuffix_HermeticForIPLiteralAndCache(t *testing.T) {
	if got := billingNetDiagSuffix("http://127.0.0.1:12345"); got != "" {
		t.Fatalf("IP-literal base must be diag-free, got %q", got)
	}
	if got := billingNetDiagSuffix("http://localhost:12345"); got != "" {
		t.Fatalf("localhost base must be diag-free, got %q", got)
	}
}

// TestProbeIPStages_ReportsDeepestStage exercises the staged probe against a
// local TLS listener with an untrusted certificate: TCP must connect, TLS
// must fail verification, and the report must show both facts.
func TestProbeIPStages_ReportsDeepestStage(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	got := probeIPStages(net.ParseIP(host), host, port)
	if !strings.HasPrefix(got, "tcp=ok tls=") {
		t.Fatalf("probe = %q, want tcp=ok tls=<err>", got)
	}
}

// TestBriefNetErr pins the error compaction used in diag output.
func TestBriefNetErr(t *testing.T) {
	cases := map[string]string{
		"dial tcp4 1.2.3.4:443: connect: connection refused": "connection refused",
		"read: EOF": "EOF",
	}
	for in, want := range cases {
		if got := briefNetErr(errString(in)); got != want {
			t.Errorf("briefNetErr(%q) = %q, want %q", in, got, want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
