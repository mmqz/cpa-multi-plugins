// touch.go reads ZCode's marketing banner feed — the second discovery list
// of the manual-claim ("weekend plan") subsystem, added for issue #69 (port
// of magpie zcode 0.1.39 / magpie #503, from kongbai01203's 10-30 capture).
//
// Why this exists: a ZCode claim banner's plan (the 10-30 "ZCODE WEEKEND
// BUILD", zcode-v3-start-plan-1030-wk, 500M GLM-5.3-Flash tokens) is in
// NEITHER /zcode-plan/billing/preview NOR /billing/current while the app
// shows the banner — so the panel's preview answered plans:[] (issue #69's
// report) and the auto-claim scheduler found no target to probe. The app
// reads the banner from GET /api/v1/marketing/touch?seq=<n> (seq counting up
// from 0 per process; the session, X-Device-Mid, X-ZCode-App-Version and
// X-Client-Language on the wire): each delivery's banner button with action
// type claim_zcode_plan carries the plan_id, the plan's name rides
// banner.background.args.zcode_plan, and the claim is the same
// POST /zcode-plan/billing/claim (claim.go) — their capture has a real code 0
// claim reply ("Plan claimed successfully.", logid 20261030141140…).
//
// Parsing is deliberately walk-based rather than schema-pinned: each
// delivery is searched recursively (depth-capped) for claim_zcode_plan
// actions and zcode_plan objects, so a button moved inside the banner is
// still found (magpie's touchPlans walk, same tolerance — the feed fixture
// there was built from the capture's description of its fields, not its
// literal bytes). A feed that doesn't read is an error, never "no plans":
// its plans are unknown, not none.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// touchClaimAction is the banner button action that claims a gift plan.
const touchClaimAction = "claim_zcode_plan"

// touchWalkDepth caps the delivery walk (magpie walks to depth 12; a banner
// deeper than that is not a shape the app ships).
const touchWalkDepth = 12

// touchFeedSeq is the feed's per-process seq counter, counting up from 0
// (the app's own semantics per #503's capture: every touch GET asks for the
// next delivery batch).
var touchFeedSeq uint64

// nextTouchSeq returns the next feed sequence number, starting at 0.
func nextTouchSeq() uint64 {
	return atomic.AddUint64(&touchFeedSeq, 1) - 1
}

// rawTouchResponse mirrors the feed envelope. Only the envelope is
// structural; everything inside data.deliveries is walked generically.
type rawTouchResponse struct {
	Code any    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Deliveries json.RawMessage `json:"deliveries"`
	} `json:"data"`
}

// touchWalkState accumulates one delivery's claim actions and plan metas.
type touchWalkState struct {
	ids   []string                  // plan_ids of claim_zcode_plan actions, in walk order
	names []string                  // zcode_plan names seen, first wins (magpie semantics)
	metas map[string]map[string]any // zcode_plan by its own plan_id
	anon  map[string]any            // first zcode_plan without a plan_id
}

// walk folds one JSON value into the state: arrays recurse elementwise;
// objects contribute a claim action / plan meta and recurse into their
// values. Non-collection leaves are ignored.
func (st *touchWalkState) walk(v any, depth int) {
	if v == nil || depth > touchWalkDepth {
		return
	}
	switch node := v.(type) {
	case []any:
		for _, item := range node {
			st.walk(item, depth+1)
		}
	case map[string]any:
		if t, _ := node["type"].(string); t == touchClaimAction {
			if args, ok := node["args"].(map[string]any); ok {
				if id, _ := args["plan_id"].(string); id != "" {
					if id = strings.TrimSpace(id); id != "" {
						st.ids = append(st.ids, id)
					}
				}
			}
		}
		if zp, ok := node["zcode_plan"].(map[string]any); ok {
			name, _ := zp["name"].(string)
			if name = strings.TrimSpace(name); name != "" && len(st.names) == 0 {
				st.names = append(st.names, name)
			}
			if id, _ := zp["plan_id"].(string); strings.TrimSpace(id) != "" {
				id = strings.TrimSpace(id)
				if st.metas == nil {
					st.metas = map[string]map[string]any{}
				}
				if _, dup := st.metas[id]; !dup {
					st.metas[id] = zp
				}
			} else if st.anon == nil {
				st.anon = zp
			}
		}
		for _, child := range node {
			st.walk(child, depth+1)
		}
	}
}

// touchNumeric reads grant_units tolerantly: number, numeric string or
// json.Number. Returns ok=false when absent or not numeric.
func touchNumeric(raw any) (int64, bool) {
	switch v := raw.(type) {
	case float64:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// touchPlansFromDelivery extracts one delivery's claimable plans: every
// claim_zcode_plan action's plan_id, named by the matched zcode_plan when
// one carries the same id, else the delivery's first zcode_plan name, else
// the id itself (magpie's fallback). Duplicate ids inside one delivery
// collapse to the first.
func touchPlansFromDelivery(dv any) []previewPlan {
	var st touchWalkState
	st.walk(dv, 0)
	if len(st.ids) == 0 {
		return nil
	}
	firstName := ""
	if len(st.names) > 0 {
		firstName = st.names[0]
	}
	seen := map[string]bool{}
	out := make([]previewPlan, 0, len(st.ids))
	for _, id := range st.ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		meta := st.metas[id]
		if meta == nil {
			meta = st.anon
		}
		name := firstName
		var grant int64
		if meta != nil {
			if n, _ := meta["name"].(string); strings.TrimSpace(n) != "" {
				name = strings.TrimSpace(n)
			}
			if g, ok := touchNumeric(meta["grant_units"]); ok && g > 0 {
				grant = g
			}
		}
		if name == "" {
			name = id
		}
		p := previewPlan{PlanID: id, Name: name, Source: previewSourceBanner}
		if grant > 0 {
			// The banner's zcode_plan carries grant_units (500M for the
			// weekend build) but no unit_type — the grant is surfaced as a
			// bare entitlement; the claim reply carries the full detail.
			p.Entitlements = []previewEntitlement{{Grant: grant}}
		}
		out = append(out, p)
	}
	return out
}

// fetchTouchPlans queries the marketing feed for one account. An account
// without JWT is not feedable (the feed rides the plan session). HTTP 404
// means the feed is not deployed — the empty case, like billing/preview. A
// body that doesn't read (bad JSON, non-array deliveries, non-zero biz
// code) is an error: unknown, never none.
func fetchTouchPlans(sa *storedAuth) ([]previewPlan, error) {
	if sa == nil || strings.TrimSpace(sa.Auth.JWT) == "" {
		return nil, fmt.Errorf("not logged in — no plan JWT (re-run login)")
	}
	q := url.Values{}
	q.Set("seq", strconv.FormatUint(nextTouchSeq(), 10))
	endpoint := zcodeAPIBase + "/marketing/touch?" + q.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+sa.Auth.JWT)
	req.Header.Set("Accept", "application/json")
	for k, v := range buildContextIdentityHeaders(zcodeIdentity(), sa.Auth.DeviceMid) {
		req.Header.Set(k, v)
	}
	resp, err := hostHTTPDo(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return []previewPlan{}, nil // feed not deployed — the empty case
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("marketing touch http %d body=%s", resp.StatusCode, truncateRedacted(string(resp.Body), 200))
	}
	var out rawTouchResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("marketing touch parse: %w", err)
	}
	if code, ok := claimBizCode(out.Code); ok && code != 0 {
		return nil, fmt.Errorf("marketing touch: code=%d msg=%s", code, out.Msg)
	}
	trimmed := strings.TrimSpace(string(out.Data.Deliveries))
	if trimmed == "" || trimmed == "null" {
		return []previewPlan{}, nil // no deliveries key / JSON null — empty feed
	}
	if trimmed[0] != '[' {
		return nil, fmt.Errorf("marketing touch: deliveries is not a list")
	}
	var deliveries []any
	if err := json.Unmarshal(out.Data.Deliveries, &deliveries); err != nil {
		return nil, fmt.Errorf("marketing touch deliveries parse: %w", err)
	}
	plans := []previewPlan{}
	for _, dv := range deliveries {
		plans = append(plans, touchPlansFromDelivery(dv)...)
	}
	return plans, nil
}
