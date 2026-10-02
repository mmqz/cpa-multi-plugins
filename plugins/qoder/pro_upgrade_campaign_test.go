// pro_upgrade_campaign_test.go — v0.8.34: the 领取Pro flow rides the
// campaigns channel. Upstream forensics (official CN client v0.4.3, sha256
// a796a175…05084f5): /sash/api/v1/me/pro-upgrade/* has ZERO matches in the
// shipped app.asar — the eligibility→claim flow this plugin ran since
// v0.8.18 called endpoints that do not exist, and the field 404
// ("领取失败 eligibility: http 404", CN account ud2d62d72) was upstream
// truthfully answering "no such route". These tests pin the replacement:
// claim pro-looking campaign rows via the verifiable
// GET /me/campaigns + POST /{id}/claim family, and answer a diagnostic
// listing (never a bare http error) when no such row is present.
package main

import (
        "net/http"
        "strings"
        "testing"
)

func proCampaignList(dailyStatus, proStatus string) string {
        return `{"showCampaign":true,"campaigns":[` +
                `{"campaignId":"camp-cn-daily","campaignKey":"cn_daily_check_in","actionType":"CLAIM_BENEFIT","claimStatus":"` + dailyStatus + `","benefit":{"kind":"CREDITS","amount":100}},` +
                `{"campaignId":"camp-pro-up","campaignKey":"pro_upgrade_pack","actionType":"CLAIM_BENEFIT","claimStatus":"` + proStatus + `","benefit":{"kind":"CREDITS","amount":1800}}` +
                `]}`
}

// TestClaimProClaimsProRowOnly: with both a daily and a pro row claimable,
// 领取Pro must claim ONLY the pro-looking row — the daily row belongs to the
// check-in flow, and the pro claim must carry the listed benefit amount.
func TestClaimProClaimsProRowOnly(t *testing.T) {
        dailyClaimed, proClaimed := false, false
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, proCampaignList("CLAIMABLE", "CLAIMABLE")
                },
                "/sash/api/v1/me/campaigns/camp-cn-daily/claim": func(r *http.Request) (int, string) {
                        dailyClaimed = true
                        return http.StatusOK, `{"status":"CLAIMED"}`
                },
                "/sash/api/v1/me/campaigns/camp-pro-up/claim": func(r *http.Request) (int, string) {
                        proClaimed = true
                        return http.StatusOK, `{"status":"CLAIMED"}`
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        if !proClaimed {
                t.Fatal("pro row claim endpoint was never called")
        }
        if dailyClaimed {
                t.Fatal("领取Pro must not claim the daily check-in row")
        }
        if success, _ := res["success"].(bool); !success {
                t.Fatalf("claim not successful: %v", res)
        }
        if id, _ := res["campaign_id"].(string); id != "camp-pro-up" {
                t.Fatalf("campaign_id = %v, want camp-pro-up", res["campaign_id"])
        }
        if rc, _ := res["rewardCredits"].(float64); rc != 1800 {
                t.Fatalf("rewardCredits = %v, want 1800 (listed benefit)", res["rewardCredits"])
        }
}

// TestClaimProAlreadyClaimedRow: a CLAIMED pro row renders as an
// actionable「已领取过」line, not an error.
func TestClaimProAlreadyClaimedRow(t *testing.T) {
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, proCampaignList("CLAIMABLE", "CLAIMED")
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        if success, _ := res["success"].(bool); success {
                t.Fatalf("success = true, want false for already-claimed: %v", res)
        }
        if msg, _ := res["message"].(string); !strings.Contains(msg, "已领取过") {
                t.Fatalf("message = %q, want 已领取过 line", msg)
        }
}

// TestClaimProNoProRowDiagnostics: when the account's campaign list has no
// pro-looking row at all, the answer must list what the server actually
// returned (campaignKey/actionType/claimStatus) instead of a bare failure —
// this is what makes a mis-guessed key correctable from one field report.
func TestClaimProNoProRowDiagnostics(t *testing.T) {
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[{"campaignId":"c1","campaignKey":"client_launch_26","actionType":"ACTIVITY","claimStatus":"IN_PROGRESS"}]}`
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        if success, _ := res["success"].(bool); success {
                t.Fatalf("success = true, want false for no-pro-row: %v", res)
        }
        if reason, _ := res["reason"].(string); reason != "no_pro_row" {
                t.Fatalf("reason = %v, want no_pro_row", res["reason"])
        }
        msg, _ := res["message"].(string)
        for _, want := range []string{"client_launch_26", "ACTIVITY", "IN_PROGRESS"} {
                if !strings.Contains(msg, want) {
                        t.Fatalf("diagnostic message missing %q: %q", want, msg)
                }
        }
}

// TestClaimProEmptyListClientSessionHint: an empty campaign list most likely
// means the account's eligibility never synced (#27: rows appear only after
// the account opens the activity once inside the official client) — the
// message must say so.
func TestClaimProEmptyListClientSessionHint(t *testing.T) {
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":false,"campaigns":[]}`
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        msg, _ := res["message"].(string)
        if !strings.Contains(msg, "客户端") {
                t.Fatalf("message = %q, want client-session hint", msg)
        }
}

// TestClaimProCampaignsErrorPropagates: a campaigns listing failure surfaces
// as an error carrying the upstream status — no silent success.
func TestClaimProCampaignsErrorPropagates(t *testing.T) {
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusInternalServerError, `{"err":"boom"}`
                },
        })

        _, err := claimProViaCampaigns(cnAuth())
        if err == nil || !strings.Contains(err.Error(), "campaigns http 500") {
                t.Fatalf("err = %v, want campaigns http 500", err)
        }
}

// TestClaimProIntlSkipsBeforeAnyRequest: the Intl skip is a capability fact
// (panel contract since v0.8.18) — no request may fire for an Intl account.
func TestClaimProIntlSkipsBeforeAnyRequest(t *testing.T) {
        hit := false
        newBillingServer(t, "intl", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        hit = true
                        return http.StatusOK, `{"campaigns":[]}`
                },
        })

        res, err := claimProViaCampaigns(&storedAuth{Auth: storedTokens{AccessToken: "dt-intl", Region: "intl"}})
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        if hit {
                t.Fatal("Intl must not reach the campaigns endpoint from the Pro flow")
        }
        if skipped, _ := res["skipped"].(bool); !skipped {
                t.Fatalf("Intl result must carry skipped=true: %v", res)
        }
}

// TestClaimProClaimsActKeyBigBenefit: live campaign keys come in the
// act-YYYYMMDD-NNN form (verified against the qoder2api-hub capture), which
// no pro/upgrade substring can match — the Pro pack must also be found by
// its big one-shot CREDITS benefit (verified face value +1800).
func TestClaimProClaimsActKeyBigBenefit(t *testing.T) {
        proClaimed := false
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[` +
                                `{"campaignId":"c-daily","campaignKey":"act-20260928-620","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}},` +
                                `{"campaignId":"c-pro","campaignKey":"act-20260930-001","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":1800}}` +
                                `]}`
                },
                "/sash/api/v1/me/campaigns/c-daily/claim": func(r *http.Request) (int, string) {
                        t.Error("领取Pro must not claim the 100-credit daily row")
                        return http.StatusOK, `{"status":"CLAIMED"}`
                },
                "/sash/api/v1/me/campaigns/c-pro/claim": func(r *http.Request) (int, string) {
                        proClaimed = true
                        return http.StatusOK, `{"status":"CLAIMED"}`
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        if !proClaimed {
                t.Fatal("1800-credit act-key row was not claimed (benefit matcher failed)")
        }
        if id, _ := res["campaign_id"].(string); id != "c-pro" {
                t.Fatalf("campaign_id = %v, want c-pro", res["campaign_id"])
        }
}

// TestCampaignClaimBlockedSamePerson: upstream dedupes by PERSON — a second
// account on the same machine identity gets status=BLOCKED with
// failureCode=SAME_PERSON_ALREADY_CLAIMED (hub live capture). It must render
// as an actionable 同人已领取 line, not a raw upstream dump.
func TestCampaignClaimBlockedSamePerson(t *testing.T) {
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"campaigns":[{"campaignId":"cp","campaignKey":"act-20260930-001","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":1800}}]}`
                },
                "/sash/api/v1/me/campaigns/cp/claim": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED"}`
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        if success, _ := res["success"].(bool); success {
                t.Fatalf("success = true, want false for BLOCKED: %v", res)
        }
        if result, _ := res["result"].(string); result != "BLOCKED" {
                t.Fatalf("result = %v, want BLOCKED", res["result"])
        }
        if msg, _ := res["message"].(string); !strings.Contains(msg, "同人已领取") {
                t.Fatalf("message = %q, want 同人已领取 line", msg)
        }
}

// TestCampaignClaim409AlreadyErrorCode: the replay can also arrive as
// HTTP 409 + errorCode=ALREADY_CLAIMED (hub capture) — normalized to
// ALREADY_CLAIMED, never a raw "http 409" error.
func TestCampaignClaim409AlreadyErrorCode(t *testing.T) {
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"campaigns":[{"campaignId":"cp","campaignKey":"act-20260930-001","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":1800}}]}`
                },
                "/sash/api/v1/me/campaigns/cp/claim": func(r *http.Request) (int, string) {
                        return http.StatusConflict, `{"errorCode":"ALREADY_CLAIMED"}`
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        if result, _ := res["result"].(string); result != "ALREADY_CLAIMED" {
                t.Fatalf("result = %v, want ALREADY_CLAIMED for 409 replay", res["result"])
        }
}

// TestClaimProRewardProbeClaimsViewDetailsPack — THE user-reported live
// shape (CN account ud2d62d72): a VIEW_DETAILS/CLAIMABLE row with NO listed
// benefit. The activity page reads the real face value via the read-only
// /reward endpoint (hub capture); when it reveals the verified big one-shot
// CREDITS pack, 领取Pro claims that row and reports the revealed amount.
func TestClaimProRewardProbeClaimsViewDetailsPack(t *testing.T) {
        t.Setenv("QD_NATIVE_IDENTITY", "0")
        rewardHit, proClaimed := false, false
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[` +
                                `{"campaignId":"act-20260901-922","campaignKey":"act-20260901-922","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"},` +
                                `{"campaignId":"act-20260930-125","campaignKey":"act-20260930-125","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}` +
                                `]}`
                },
                "/sash/api/v1/me/campaigns/act-20260901-922/reward": func(r *http.Request) (int, string) {
                        rewardHit = true
                        return http.StatusOK, `{"data":{"campaignId":"act-20260901-922","benefit":{"kind":"CREDITS","amount":1800},"status":"CLAIMABLE"}}`
                },
                "/sash/api/v1/me/campaigns/act-20260901-922/claim": func(r *http.Request) (int, string) {
                        proClaimed = true
                        return http.StatusOK, `{"status":"CLAIMED"}`
                },
                "/sash/api/v1/me/campaigns/act-20260930-125/claim": func(r *http.Request) (int, string) {
                        t.Error("领取Pro must never re-claim the already-CLAIMED daily row")
                        return http.StatusOK, `{"status":"CLAIMED"}`
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        if !rewardHit {
                t.Fatal("the VIEW_DETAILS row's /reward was never probed")
        }
        if !proClaimed {
                t.Fatalf("the revealed +1800 pack was not claimed: %v", res)
        }
        if success, _ := res["success"].(bool); !success {
                t.Fatalf("claim not successful: %v", res)
        }
        if rc, _ := res["rewardCredits"].(float64); rc != 1800 {
                t.Fatalf("rewardCredits = %v, want the revealed 1800", res["rewardCredits"])
        }
}

// TestClaimProRewardProbeSmallBenefitNotClaimed: a VIEW_DETAILS row whose
// /reward reveals a small benefit is NOT the Pro pack — it must be left
// unclaimed and annotated in the diagnostics instead.
func TestClaimProRewardProbeSmallBenefitNotClaimed(t *testing.T) {
        t.Setenv("QD_NATIVE_IDENTITY", "0")
        claimed := false
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[{"campaignId":"act-v","campaignKey":"act-20260901-922","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"}]}`
                },
                "/sash/api/v1/me/campaigns/act-v/reward": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"benefit":{"kind":"CREDITS","amount":100}}`
                },
                "/sash/api/v1/me/campaigns/act-v/claim": func(r *http.Request) (int, string) {
                        claimed = true
                        return http.StatusOK, `{"status":"CLAIMED"}`
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        if claimed {
                t.Fatal("a +100 VIEW_DETAILS row must not be claimed by the Pro flow")
        }
        if success, _ := res["success"].(bool); success {
                t.Fatalf("success = true, want diagnostics: %v", res)
        }
        msg, _ := res["message"].(string)
        for _, want := range []string{"act-20260901-922", "reward=+100"} {
                if !strings.Contains(msg, want) {
                        t.Fatalf("diagnostic message missing %q: %q", want, msg)
                }
        }
}

// TestClaimProDiagnosticsCarryIdentityAndProbeLines: the live diagnostic for
// ud2d62d72's exact list must keep its row listing AND gain the probe
// annotation plus the machine-identity disclosure (why device-targeted rows
// can be missing without the official client's real identity).
func TestClaimProDiagnosticsCarryIdentityAndProbeLines(t *testing.T) {
        t.Setenv("QD_NATIVE_IDENTITY", "0")
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[` +
                                `{"campaignId":"act-20260901-922","campaignKey":"act-20260901-922","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"},` +
                                `{"campaignId":"act-20260930-125","campaignKey":"act-20260930-125","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}` +
                                `]}`
                },
                // /reward unstubbed → 404 → 面值不可读 annotation.
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        msg, _ := res["message"].(string)
        for _, want := range []string{
                "act-20260901-922(VIEW_DETAILS/CLAIMABLE",
                "面值不可读",
                "模拟身份",
        } {
                if !strings.Contains(msg, want) {
                        t.Fatalf("diagnostic message missing %q: %q", want, msg)
                }
        }
}

// TestFetchCampaignStatusSelfHealRetriesOnNativeIdentity: showCampaign=false
// with a NATIVE identity means the identity likely rotated (hub live
// pattern) — exactly one forced identity refresh + one retry, and the
// retried list wins when it turns showCampaign back on.
func TestFetchCampaignStatusSelfHealRetriesOnNativeIdentity(t *testing.T) {
        machineIdentityOverride = &machineIdentity{
                MachineID: "mid", MachineToken: "mtok", MachineType: "mtype",
                MachineCode: "mcode", MachineOS: "x86_64_win32",
                MachineHostname: "DESKTOP-QODER", Source: "runtime-info",
        }
        t.Cleanup(func() { machineIdentityOverride = nil })
        forces := 0
        machineIdentityForceHook = func() { forces++ }
        t.Cleanup(func() { machineIdentityForceHook = nil })

        calls := 0
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        calls++
                        if calls == 1 {
                                return http.StatusOK, `{"showCampaign":false,"campaigns":[]}`
                        }
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[{"campaignId":"c","campaignKey":"act-20260930-001","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}}]}`
                },
        })

        out, err := fetchCampaignStatus(cnAuth())
        if err != nil {
                t.Fatalf("fetchCampaignStatus: %v", err)
        }
        if calls != 2 {
                t.Fatalf("campaigns GET count = %d, want 2 (initial + self-heal retry)", calls)
        }
        if forces != 1 {
                t.Fatalf("forced identity refresh count = %d, want 1", forces)
        }
        if !out.ShowCampaign || len(out.Campaigns) != 1 {
                t.Fatalf("retried envelope not adopted: %+v", out)
        }
}

// TestFetchCampaignStatusDerivedIdentityNeverRetries: a derived identity has
// nothing to rotate — no retry may fire even on showCampaign=false.
func TestFetchCampaignStatusDerivedIdentityNeverRetries(t *testing.T) {
        t.Setenv("QD_NATIVE_IDENTITY", "0")
        calls := 0
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        calls++
                        return http.StatusOK, `{"showCampaign":false,"campaigns":[]}`
                },
        })

        if _, err := fetchCampaignStatus(cnAuth()); err != nil {
                t.Fatalf("fetchCampaignStatus: %v", err)
        }
        if calls != 1 {
                t.Fatalf("campaigns GET count = %d, want 1 (derived identity must not retry)", calls)
        }
}

// setClaimUnverifiedForTest flips the claim_unverified flag for one test.
func setClaimUnverifiedForTest(t *testing.T, on bool) {
        t.Helper()
        claimUnverifiedMu.Lock()
        claimUnverified = on
        claimUnverifiedMu.Unlock()
        t.Cleanup(func() {
                claimUnverifiedMu.Lock()
                claimUnverified = false
                claimUnverifiedMu.Unlock()
        })
}

// TestClaimProProbeErrorDetailSurfaced (v0.8.37): the 面值不可读 annotation
// must carry the reward endpoint's own verdict (http status + body) so the
// panel shows whether the account is not registered (404) or identity-
// filtered (403) instead of a bare annotation; with the flag still off the
// message also names the opt-in.
func TestClaimProProbeErrorDetailSurfaced(t *testing.T) {
        t.Setenv("QD_NATIVE_IDENTITY", "0")
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[{"campaignId":"act-20260901-922","campaignKey":"act-20260901-922","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"}]}`
                },
                "/sash/api/v1/me/campaigns/act-20260901-922/reward": func(r *http.Request) (int, string) {
                        return http.StatusForbidden, `{"errorCode":"IDENTITY_FILTERED"}`
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        msg, _ := res["message"].(string)
        for _, want := range []string{"面值不可读: reward http 403", "IDENTITY_FILTERED", "claim_unverified"} {
                if !strings.Contains(msg, want) {
                        t.Fatalf("diagnostic missing %q: %q", want, msg)
                }
        }
}

// TestClaimProUnverifiedBlindClaimOptIn (v0.8.37): with claim_unverified the
// flow claims the cannot-verify row via the verified claim endpoint and the
// claim response is final.
func TestClaimProUnverifiedBlindClaimOptIn(t *testing.T) {
        t.Setenv("QD_NATIVE_IDENTITY", "0")
        setClaimUnverifiedForTest(t, true)

        claims := 0
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[{"campaignId":"act-20260901-922","campaignKey":"act-20260901-922","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"}]}`
                },
                // /reward unstubbed → 404 → cannot verify → blind claim.
                "/sash/api/v1/me/campaigns/act-20260901-922/claim": func(r *http.Request) (int, string) {
                        claims++
                        return http.StatusOK, `{"data":{"status":"CLAIMED"}}`
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        if claims != 1 {
                t.Fatalf("claim POST count = %d, want 1", claims)
        }
        if success, _ := res["success"].(bool); !success {
                t.Fatalf("blind claim should surface success: %v", res)
        }
}

// TestClaimProUnverifiedNeverClaimsReadableSmallReward (v0.8.37): a row that
// DOES reveal a readable reward below the one-shot pack threshold is verified
// small — the opt-in flag must not turn it into a blind claim.
func TestClaimProUnverifiedNeverClaimsReadableSmallReward(t *testing.T) {
        t.Setenv("QD_NATIVE_IDENTITY", "0")
        setClaimUnverifiedForTest(t, true)

        claims := 0
        newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"showCampaign":true,"campaigns":[{"campaignId":"act-20260901-922","campaignKey":"act-20260901-922","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"}]}`
                },
                "/sash/api/v1/me/campaigns/act-20260901-922/reward": func(r *http.Request) (int, string) {
                        return http.StatusOK, `{"benefit":{"kind":"CREDITS","amount":100}}`
                },
                "/sash/api/v1/me/campaigns/act-20260901-922/claim": func(r *http.Request) (int, string) {
                        claims++
                        return http.StatusOK, `{"data":{"status":"CLAIMED"}}`
                },
        })

        res, err := claimProViaCampaigns(cnAuth())
        if err != nil {
                t.Fatalf("claimProViaCampaigns: %v", err)
        }
        if claims != 0 {
                t.Fatalf("claim POST count = %d, want 0 (verified small reward is not blind-claimed)", claims)
        }
        if success, _ := res["success"].(bool); success {
                t.Fatalf("no claim should have succeeded: %v", res)
        }
}

// TestViewDetailsGrantNotFoundTypedDead (v0.8.43): the live field report
// ("面值不可读：reward http 404 body={"errorCode":"GRANT_NOT_FOUND",
// "errorMessage":"campaign was…") is NOT an unreadable face value — it is
// the upstream's authoritative no-grant-record verdict for a closed round.
// The panel must name it as a dead campaign, keep offering neither the
// 面值不可读 copy nor the claim_unverified opt-in, and never POST claim on
// the row — with or without the opt-in flag.
func TestViewDetailsGrantNotFoundTypedDead(t *testing.T) {
        t.Setenv("QD_NATIVE_IDENTITY", "0")
        for _, optIn := range []bool{false, true} {
                setClaimUnverifiedForTest(t, optIn)
                claims := 0
                newBillingServer(t, "cn", map[string]func(r *http.Request) (int, string){
                        "/sash/api/v1/me/campaigns": func(r *http.Request) (int, string) {
                                return http.StatusOK, `{"showCampaign":true,"campaigns":[{"campaignId":"act-20260901-922","campaignKey":"act-20260901-922","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"}]}`
                        },
                        "/sash/api/v1/me/campaigns/act-20260901-922/reward": func(r *http.Request) (int, string) {
                                return http.StatusNotFound, `{"errorCode":"GRANT_NOT_FOUND","errorMessage":"campaign was closed"}`
                        },
                        "/sash/api/v1/me/campaigns/act-20260901-922/claim": func(r *http.Request) (int, string) {
                                claims++
                                return http.StatusOK, `{"data":{"status":"CLAIMED"}}`
                        },
                })

                res, err := claimProViaCampaigns(cnAuth())
                if err != nil {
                        t.Fatalf("optIn=%v claimProViaCampaigns: %v", optIn, err)
                }
                if claims != 0 {
                        t.Fatalf("optIn=%v claim POST count = %d, want 0 (GRANT_NOT_FOUND row is conclusively dead)", optIn, claims)
                }
                msg, _ := res["message"].(string)
                for _, want := range []string{"活动已失效", "GRANT_NOT_FOUND"} {
                        if !strings.Contains(msg, want) {
                                t.Fatalf("optIn=%v diagnostic missing %q: %q", optIn, want, msg)
                        }
                }
                for _, banned := range []string{"面值不可读", "claim_unverified"} {
                        if strings.Contains(msg, banned) {
                                t.Fatalf("optIn=%v dead-row verdict must not carry %q: %q", optIn, banned, msg)
                        }
                }
        }
}
