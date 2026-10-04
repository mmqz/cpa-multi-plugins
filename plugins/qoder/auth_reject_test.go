package main

// auth_reject_test.go — locks the rule that a credential rejection must not
// be softened into a stale snapshot (0.8.42, adapted from bfSan f05e9e3).
//
// Field report behind this (2026-10-02): with upstream answering 401
// "missing cookie header" on the billing surface, every panel fetch failed
// and the stale-while-error carryover kept rendering the PREVIOUS snapshot —
// an account that never signed in showed 「今日已签到」 with credits frozen
// at 0. A stale snapshot is indistinguishable from a real check-in, so a
// credential rejection must clear it. Transient upstream failures (5xx)
// must still carry over — a flaky network must not blank the panel.

import (
        "fmt"
        "net/http"
        "net/http/httptest"
        "testing"
        "time"

        "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// rejectingServer answers every path with the live 2026-09-21 gateway
// rejection shape.
func rejectingServer(t *testing.T) {
        t.Helper()
        srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                w.Header().Set("Content-Type", "application/json")
                w.WriteHeader(http.StatusUnauthorized)
                _, _ = w.Write([]byte(`{"code":"UNAUTHORIZED","message":"missing cookie header"}`))
        }))
        t.Cleanup(srv.Close)
        prev := billingBaseOverride
        billingBaseOverride = func(string) string { return srv.URL }
        t.Cleanup(func() { billingBaseOverride = prev })
        t.Cleanup(resetBillingJars)
}

func TestIsAuthRejectedError(t *testing.T) {
        cases := []struct {
                name string
                err  error
                want bool
        }{
                {"nil", nil, false},
                {"plain 500", fmt.Errorf("quota/usage http 500 body=oops"), false},
                {"timeout", fmt.Errorf("context deadline exceeded"), false},
                {"parse", fmt.Errorf("campaigns parse: unexpected end of JSON input"), false},
                {"401 marker", fmt.Errorf("campaigns http 401 body={\"code\":\"UNAUTHORIZED\"}"), true},
                {"403 marker", fmt.Errorf("checkin status http 403 body=forbidden"), true},
                {"cookie body", fmt.Errorf(`campaigns http 401 body={"code":"UNAUTHORIZED","message":"missing cookie header"}`), true},
                {"unauthorized body", fmt.Errorf(`quota/usage http 400 body={"code":"UNAUTHORIZED"}`), true},
                {"wrapped typed", &authRejectedError{status: 401, err: fmt.Errorf("quota/usage http 401 body=x")}, true},
        }
        for _, tc := range cases {
                t.Run(tc.name, func(t *testing.T) {
                        if got := isAuthRejectedError(tc.err); got != tc.want {
                                t.Fatalf("isAuthRejectedError(%v) = %v, want %v", tc.err, got, tc.want)
                        }
                })
        }
}

func TestCachedDetailsRejectDoesNotCarryStaleCheckin(t *testing.T) {
        rejectingServer(t)
        authID := "auth-ghost"
        // Seed the cache with the exact ghost state the field report showed:
        // 今日已签到 + a non-zero balance, from a previous successful round.
        accountCache.Store(authID, &accountCacheEntry{
                checkin: &checkinSummary{TodayCheckedIn: true, StreakDays: 5, TotalCredits: 500},
                credits: &creditsSummary{TotalRemain: 100, TotalSize: 100},
                plan:    "Pro Trial",
                fetched: time.Now().Add(-2 * time.Hour),
        })
        t.Cleanup(func() { accountCache.Delete(authID) })

        sa := cnAuth()
        plan, ci, cr, errs := cachedAccountDetails(authID, sa, true)

        if ci != nil && ci.TodayCheckedIn {
                t.Fatalf("stale 今日已签到 survived a credential rejection: %+v", ci)
        }
        if cr != nil && cr.TotalRemain == 100 {
                t.Fatalf("stale credit balance survived a credential rejection: %+v", cr)
        }
        if plan == "Pro Trial" {
                t.Fatal("stale plan survived a credential rejection")
        }
        if len(errs) == 0 {
                t.Fatal("rejection must surface in errs, not silently blank the row")
        }
}

func TestCachedDetailsTransientStillCarriesOver(t *testing.T) {
        // 5xx = upstream had a bad moment, credential is fine: the panel keeps
        // the previous values instead of blanking the row.
        srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                w.WriteHeader(http.StatusInternalServerError)
                _, _ = w.Write([]byte(`{"oops":true}`))
        }))
        t.Cleanup(srv.Close)
        prev := billingBaseOverride
        billingBaseOverride = func(string) string { return srv.URL }
        t.Cleanup(func() { billingBaseOverride = prev })
        t.Cleanup(resetBillingJars)

        authID := "auth-transient"
        accountCache.Store(authID, &accountCacheEntry{
                checkin: &checkinSummary{TodayCheckedIn: true, StreakDays: 5},
                credits: &creditsSummary{TotalRemain: 100},
                plan:    "Pro Trial",
                fetched: time.Now().Add(-2 * time.Hour),
        })
        t.Cleanup(func() { accountCache.Delete(authID) })

        _, ci, cr, _ := cachedAccountDetails(authID, cnAuth(), true)
        if ci == nil || !ci.TodayCheckedIn {
                t.Fatalf("transient failure must carry over the previous checkin state, got %+v", ci)
        }
        if cr == nil || cr.TotalRemain != 100 {
                t.Fatalf("transient failure must carry over the previous credits, got %+v", cr)
        }
}

func TestCheckinOneAccountNeverFakesSuccessOnReject(t *testing.T) {
        rejectingServer(t)
        // Stub the host auth store so checkinOneAccount can load a credential.
        origGet := hostAuthGetPhysicalFn
        hostAuthGetPhysicalFn = func(authIndex string) (*hostAuthPhysical, error) {
                return &hostAuthPhysical{
                        AuthIndex: authIndex,
                        Name:      "qoder-cn-reject.json",
                        JSON:      []byte(`{"accessToken":"dt-reject-test","region":"cn","domain":"qoder.com.cn"}`),
                }, nil
        }
        t.Cleanup(func() { hostAuthGetPhysicalFn = origGet })

        // The manual check-in path must report the status error verbatim —
        // never skip through to a 「今日已签到」 verdict.
        out := checkinOneAccount(pluginapi.HostAuthFileEntry{AuthIndex: "auth-reject", Name: "qoder-cn-reject.json"})
        if out["error"] == nil && out["reason"] == "already" {
                t.Fatalf("auth rejection rendered as already-checked-in: %#v", out)
        }
        if out["error"] == nil {
                t.Fatalf("auth rejection must surface an error on the manual path: %#v", out)
        }
}
