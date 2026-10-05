//go:build live

package main

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestLiveShellBundleAudit(t *testing.T) {
	sa := liveSA(t)
	base := billingBaseFor(sa)

	fetch := func(url string) (int, string) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return -1, err.Error()
		}
		billingHeaders(req, sa)
		resp, err := hostHTTPDo(req)
		if err != nil {
			return -1, err.Error()
		}
		return resp.StatusCode, string(resp.Body)
	}

	st, body := fetch(base + "/growth-page/activity-iframe")
	t.Logf("[shell] http %d len=%d body=%s", st, len(body), body)
	srcs := regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(body, -1)
	for _, m := range srcs {
		t.Logf("[asset] %s", m[1])
		u := m[1]
		if !strings.HasPrefix(u, "http") {
			if strings.HasPrefix(u, "/") {
				u = base + u
			} else {
				u = base + "/growth-page/" + u
			}
		}
		s2, b2 := fetch(u)
		// print api-ish strings from the bundle
		paths := regexp.MustCompile(`["'`+"`"+`](/[a-z0-9_\-/]*(?:campaign|placement|benefit|check|grant|growth|activity)[a-z0-9_\-/]*)["'`+"`"+`]`).FindAllStringSubmatch(b2, -1)
		seen := map[string]bool{}
		for _, p := range paths {
			if !seen[p[1]] {
				seen[p[1]] = true
				t.Logf("[api] %s", p[1])
			}
		}
		keys := regexp.MustCompile(`act-\d{8}-\d+`).FindAllString(b2, -1)
		uk := map[string]bool{}
		for _, k := range keys {
			if !uk[k] {
				uk[k] = true
				t.Logf("[actid-in-bundle] %s", k)
			}
		}
		t.Logf("[bundle] %s http %d len=%d", u, s2, len(b2))
	}
}
