// net_dial.go — resilient dialing for the direct billing transports (v0.9.53).
//
// Field data from v0.8.52→v0.8.55 shows every transport variant (pooled
// h1.1, rescue fresh-conn tcp4, host bridge) failing simultaneously with EOF
// on deployments where direct egress to codebuddy.ai/workbuddy.ai is broken,
// while the same transports verify 200 from healthy networks. Three
// machine-level causes produce that exact all-paths-EOF signature:
//
//  1. local DNS pollution / stale resolver data handing out an address that
//     accepts TCP but never completes the exchange (EOF);
//  2. a dead CDN edge inside the resolved set (same EOF);
//  3. an MTU blackhole on the path — the TCP handshake (small packets)
//     survives, then the server's certificate flight (large packets) is
//     silently dropped, the TLS handshake stalls, and the connection dies
//     as EOF.
//
// billingDialContext attacks all three: multi-source resolution (system DNS
// cross-checked against DoH via doh.pub and cloudflare-dns.com), candidate
// fallback with short-lived dead-IP memory, and a Linux TCP_MAXSEG clamp so
// the server's certificate flight fits under broken PMTUD paths. A staged
// probe (dns → tcp → tls → http per IP) is appended to billing exhaustion
// errors so the next field report pinpoints the failing stage instead of
// surfacing yet another bare `Post "url": EOF`.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	dialIPTimeout     = 4 * time.Second  // per-candidate TCP budget
	dohQueryTimeout   = 3 * time.Second  // per-DoH-endpoint budget
	dohCacheTTL       = 5 * time.Minute  // DoH answer cache
	goodIPTTL         = 10 * time.Minute // winning-IP stickiness
	deadIPTTL         = 2 * time.Minute  // skip freshly-dead IPs
	maxDialCandidates = 4                // candidates tried per dial call
	diagCacheTTL      = 30 * time.Second // exhaustion probe de-dup window
)

// billingDialer is the shared dialer used by every direct transport. The
// Control hook clamps the advertised MSS on Linux (mss_linux.go) — a no-op
// elsewhere — so a server behind a broken return path can still deliver its
// certificate flight within the working packet size.
var billingDialer = &net.Dialer{
	Timeout:   15 * time.Second,
	KeepAlive: 30 * time.Second,
	Control:   dialControl,
}

// Test seams: overridden in net_dial_test.go for hermetic coverage.
var (
	dialSystemLookup = func(ctx context.Context, host string) ([]net.IP, error) {
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		ips := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
		return ips, nil
	}
	dialDoHEndpoints = []string{"https://doh.pub/dns-query", "https://cloudflare-dns.com/dns-query"}
	dialDoHClient    = &http.Client{Timeout: dohQueryTimeout}
)

// Dial-state caches. One mutex guards all of them — contention is irrelevant
// at billing call rates.
var (
	dialStateMu sync.Mutex
	goodIPs     = map[string]net.IP{}    // host → last TCP-winning IP
	goodIPAt    = map[string]time.Time{} // host → when the win was recorded
	deadIPs     = map[string]time.Time{} // "ip" → skip-until timestamp
	dohCache    = map[string]dohEntry{}  // host → DoH answers
)

type dohEntry struct {
	ips []net.IP
	at  time.Time
}

// billingDialContext is the DialContext for sharedHTTPClient and
// rescueHTTPClient. Behaviour by address shape:
//
//   - IP literal / localhost: plain dial (keeps httptest-based unit tests and
//     proxy dials to 127.0.0.1 byte-identical to the default transport);
//   - hostname: multi-candidate dial — cached-good IP first, then v4
//     consensus (system ∩ DoH), DoH-only v4 (suspected pollution when the
//     sets diverge), system-only v4, v6 tail; per-IP failures are remembered
//     for deadIPTTL and the first TCP win is cached for goodIPTTL;
//   - zero candidates (both resolvers empty): plain system dial so exotic
//     internal names keep their default behaviour.
func billingDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "443"
	}
	if net.ParseIP(host) != nil || isLocalHostName(host) {
		return billingDialer.DialContext(ctx, network, addr)
	}
	if network != "tcp" && network != "tcp4" {
		// Explicit tcp6 (or exotic) dials: plain, MSS clamp still applies.
		return billingDialer.DialContext(ctx, network, addr)
	}

	cands := dialCandidates(host)
	if len(cands) == 0 {
		// Both resolvers came up empty — behave exactly like the default
		// transport (which would surface the same resolver error).
		return billingDialer.DialContext(ctx, network, addr)
	}

	var firstErr error
	tried := 0
	for _, ip := range cands {
		if tried >= maxDialCandidates || ctx.Err() != nil {
			break
		}
		family := "tcp4"
		if ip.To4() == nil {
			family = "tcp6"
		}
		ipCtx := ctx
		cancel := context.CancelFunc(nil)
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > dialIPTimeout {
			ipCtx, cancel = context.WithTimeout(ctx, dialIPTimeout)
		}
		conn, err := billingDialer.DialContext(ipCtx, family, net.JoinHostPort(ip.String(), port))
		if cancel != nil {
			cancel()
		}
		if err == nil {
			dialStateMarkGood(host, ip)
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		dialStateMarkDead(ip)
		tried++
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no dial candidates for %s", host)
	}
	return nil, firstErr
}

// dialCandidates builds the ordered candidate list for host.
func dialCandidates(host string) []net.IP {
	dialStateMu.Lock()
	var good net.IP
	if ip, ok := goodIPs[host]; ok && time.Since(goodIPAt[host]) < goodIPTTL {
		good = ip
	}
	dialStateMu.Unlock()

	sysCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sys, _ := dialSystemLookup(sysCtx, host)
	doh := dohLookup(host)

	return filterDeadIPs(host, orderIPs(good, sys, doh))
}

// orderIPs returns dial candidates in trust order: cached-good → v4
// consensus (system ∩ DoH) → DoH-only v4 (the pollution suspect when the
// sets diverge — doh.pub/cloudflare are encrypted and cross-checked) →
// system-only v4 → v6 tail. Deduplicated; caller caps the count.
func orderIPs(good net.IP, sys, doh []net.IP) []net.IP {
	key := func(ip net.IP) string { return ip.String() }
	seen := map[string]bool{}
	var out []net.IP
	add := func(ips ...net.IP) {
		for _, ip := range ips {
			if ip == nil {
				continue
			}
			k := key(ip)
			if !seen[k] {
				seen[k] = true
				out = append(out, ip)
			}
		}
	}
	add(good)

	var sys4, sys6, doh4 []net.IP
	for _, ip := range sys {
		if ip.To4() != nil {
			sys4 = append(sys4, ip)
		} else {
			sys6 = append(sys6, ip)
		}
	}
	for _, ip := range doh {
		if ip.To4() != nil {
			doh4 = append(doh4, ip)
		}
	}
	sysSet := map[string]bool{}
	for _, ip := range sys4 {
		sysSet[key(ip)] = true
	}
	dohSet := map[string]bool{}
	for _, ip := range doh4 {
		dohSet[key(ip)] = true
	}
	var consensus, dohOnly, sysOnly []net.IP
	for _, ip := range doh4 {
		if sysSet[key(ip)] {
			consensus = append(consensus, ip)
		} else {
			dohOnly = append(dohOnly, ip)
		}
	}
	for _, ip := range sys4 {
		if !dohSet[key(ip)] {
			sysOnly = append(sysOnly, ip)
		}
	}
	add(consensus...)
	add(dohOnly...)
	add(sysOnly...)
	add(sys6...)
	return out
}

// filterDeadIPs drops IPs marked dead within deadIPTTL. If every candidate
// is dead (the path may have recovered), the marks are cleared so dialing
// gets a full retry budget instead of a guaranteed empty result.
func filterDeadIPs(host string, cands []net.IP) []net.IP {
	dialStateMu.Lock()
	defer dialStateMu.Unlock()
	now := time.Now()
	live := make([]net.IP, 0, len(cands))
	for _, ip := range cands {
		if until, ok := deadIPs[ip.String()]; ok && now.Before(until) {
			continue
		}
		live = append(live, ip)
	}
	if len(live) == 0 && len(cands) > 0 {
		for _, ip := range cands {
			delete(deadIPs, ip.String())
		}
		return cands
	}
	return live
}

func dialStateMarkGood(host string, ip net.IP) {
	dialStateMu.Lock()
	defer dialStateMu.Unlock()
	goodIPs[host] = ip
	goodIPAt[host] = time.Now()
	delete(deadIPs, ip.String())
}

func dialStateMarkDead(ip net.IP) {
	dialStateMu.Lock()
	defer dialStateMu.Unlock()
	deadIPs[ip.String()] = time.Now().Add(deadIPTTL)
}

// billingResetDialState is called when the billing retry ladder is exhausted:
// stickiness clearly picked a losing path, so forget the cached-good IP and
// dead marks and drop pooled connections (a poisoned pooled conn would
// otherwise serve the next billing call straight from the pool).
func billingResetDialState() {
	dialStateMu.Lock()
	goodIPs = map[string]net.IP{}
	goodIPAt = map[string]time.Time{}
	deadIPs = map[string]time.Time{}
	dialStateMu.Unlock()
	sharedHTTPClient().CloseIdleConnections()
	rescueHTTPClient().CloseIdleConnections()
}

// -----------------------------------------------------------------------------
// DoH resolution
// -----------------------------------------------------------------------------

// dohLookup returns cached DoH answers for host, querying both endpoints
// concurrently on cache miss. Failures are non-fatal: an empty result simply
// leaves the system-DNS candidates in charge.
func dohLookup(host string) []net.IP {
	dialStateMu.Lock()
	if e, ok := dohCache[host]; ok && time.Since(e.at) < dohCacheTTL {
		dialStateMu.Unlock()
		return e.ips
	}
	dialStateMu.Unlock()

	ips := queryDoH(host)
	if len(ips) > 0 {
		dialStateMu.Lock()
		dohCache[host] = dohEntry{ips: ips, at: time.Now()}
		dialStateMu.Unlock()
	}
	return ips
}

// queryDoH issues RFC 8484-style JSON queries (application/dns-json, the
// Google/Cloudflare/doh.pub JSON dialect) to every endpoint concurrently and
// merges the unique A records.
func queryDoH(host string) []net.IP {
	type result struct{ ips []net.IP }
	ch := make(chan result, len(dialDoHEndpoints))
	for _, ep := range dialDoHEndpoints {
		go func(endpoint string) {
			defer func() { ch <- result{} }() // default on any exit path
			u := endpoint + "?name=" + url.QueryEscape(host) + "&type=A"
			req, err := http.NewRequest(http.MethodGet, u, nil)
			if err != nil {
				return
			}
			req.Header.Set("accept", "application/dns-json")
			resp, err := dialDoHClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var payload struct {
				Answer []struct {
					Type int    `json:"type"`
					Data string `json:"data"`
				} `json:"Answer"`
			}
			if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&payload); err != nil {
				return
			}
			var ips []net.IP
			for _, a := range payload.Answer {
				if a.Type == 1 {
					if ip := net.ParseIP(strings.TrimSpace(a.Data)); ip != nil {
						ips = append(ips, ip)
					}
				}
			}
			if len(ips) > 0 {
				ch <- result{ips: ips}
			}
		}(ep)
	}
	var out []net.IP
	seen := map[string]bool{}
	for range dialDoHEndpoints {
		r := <-ch
		for _, ip := range r.ips {
			if !seen[ip.String()] {
				seen[ip.String()] = true
				out = append(out, ip)
			}
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// Exhaustion diagnostics
// -----------------------------------------------------------------------------

var diagState struct {
	mu  sync.Mutex
	at  time.Time
	val string
}

// billingNetDiagSuffix probes the billing base host stage by stage (DNS →
// TCP → TLS → HTTP/1.1) and returns a compact "; net-diag ..." suffix for the
// exhaustion error. IP-literal / localhost bases return "" so unit tests stay
// hermetic. Results are cached for diagCacheTTL — the retry ladder surfaces
// this error on every panel refresh, and probing the gateway each time would
// turn one failure into a storm.
func billingNetDiagSuffix(base string) string {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "443"
	}
	if net.ParseIP(host) != nil || isLocalHostName(host) {
		return ""
	}
	diagState.mu.Lock()
	defer diagState.mu.Unlock()
	if time.Since(diagState.at) < diagCacheTTL {
		return diagState.val
	}
	diagState.at = time.Now()
	diagState.val = billingNetDiagProbe(host, port)
	return diagState.val
}

// billingNetDiagProbe runs the staged probe and formats it. Two v4 IPs max —
// bounded to ~2×(3s tcp + 4s tls + 4s http) worst case, which is acceptable
// on an error path that has already burned ~1 minute of retries.
func billingNetDiagProbe(host, port string) string {
	sysCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sys, _ := dialSystemLookup(sysCtx, host)
	doh := dohLookup(host)

	var b strings.Builder
	fmt.Fprintf(&b, "; net-diag dns sys=%s doh=%s",
		ipListCompact(sys, 3), ipListCompact(doh, 3))
	probed := 0
	for _, ip := range orderIPs(net.IP(nil), sys, doh) {
		if probed >= 2 {
			break
		}
		if ip.To4() == nil {
			continue
		}
		fmt.Fprintf(&b, " | %s %s", ip, probeIPStages(ip, host, port))
		probed++
	}
	out := b.String()
	if len(out) > 500 {
		out = out[:500]
	}
	return out
}

// probeIPStages walks one IP through tcp → tls → raw HTTP/1.1 GET / and
// reports the deepest stage reached with a terse outcome per stage.
func probeIPStages(ip net.IP, host, port string) string {
	d := &net.Dialer{Timeout: 3 * time.Second, Control: dialControl}
	raw, err := d.Dial("tcp4", net.JoinHostPort(ip.String(), port))
	if err != nil {
		return "tcp=" + briefNetErr(err)
	}
	defer raw.Close()

	tlsConn := tls.Client(raw, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	hsCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := tlsConn.HandshakeContext(hsCtx); err != nil {
		return "tcp=ok tls=" + briefNetErr(err)
	}
	defer tlsConn.Close()

	req := "GET / HTTP/1.1\r\nHost: " + host + "\r\nUser-Agent: CodeBuddy\r\nConnection: close\r\n\r\n"
	_ = tlsConn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := tlsConn.Write([]byte(req)); err != nil {
		return "tcp=ok tls=ok http-write=" + briefNetErr(err)
	}
	_ = tlsConn.SetReadDeadline(time.Now().Add(4 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), nil)
	if err != nil {
		return "tcp=ok tls=ok http=" + briefNetErr(err)
	}
	return fmt.Sprintf("tcp=ok tls=ok http=%d", resp.StatusCode)
}

// briefNetErr compresses Go's verbose net errors to their last meaningful
// segment: "dial tcp4 1.2.3.4:443: connect: connection refused" →
// "connection refused"; a bare EOF stays "EOF".
func briefNetErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func ipListCompact(ips []net.IP, max int) string {
	if len(ips) == 0 {
		return "none"
	}
	parts := make([]string, 0, max)
	for _, ip := range ips {
		if len(parts) >= max {
			break
		}
		parts = append(parts, ip.String())
	}
	tag := fmt.Sprintf("%d/", len(ips))
	return tag + strings.Join(parts, ",")
}

func isLocalHostName(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == "localhost" || strings.HasSuffix(h, ".localhost") || h == "::1"
}
