// school.go — 开学季活动（school-season）pure-API automation.
//
// Upstream evidence: wb2api-panel internal/upstream/school.go (2026-09-16,
// endpoints verified against three live accounts; protocol.md §7.11), synced
// into our plugin after the 0.9.13 growth-center work. Activity window per
// upstream: 2026-09-13 ~ 09-24 — the /tasks endpoint answers in_period=false
// outside the window, which is the gate we honor at runtime.
//
// Endpoints (billing domain www.codebuddy.cn, prefix /portal/activity/school,
// same account Bearer + X-User-Id auth family as check-in/billing — no web
// cookie needed):
//
//	GET  /tasks                  → {tasks[], in_period}      任务列表 + 活动是否在期
//	POST /tasks/share-complete   {channel:"wechat"}          每日 +100c +1 抽奖（纯上报，服务端不校验真实分享）
//	POST /tasks/{code}/viewed    {}                          pending → in_progress（desktop_chat_1_time 等计数前置：必须先激活后的行为才计数）
//	POST /tasks/{code}/claim     → {chance_granted}          领取任务奖励（返回获得的抽奖次数）
//	GET  /config                 → {chance:{balance}}        抽奖次数余额
//	POST /wheel/draw             {draw_uuid} → {prize_code, credit_amount}
//	GET  /vouchers               → {items[]}                 第三方券码（KFC/瑞幸/酷狗…，只读）
//
// Envelope semantics: {code,msg,data}, code=0 success — same contract as the
// growth endpoints (growthDo). All actions are best-effort inside the task
// loop; business errors ("already shared"/"not completed") surface as
// code!=0 errors that the loop logs and continues past.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const schoolBase = "/portal/activity/school"

// schoolCallTimeout bounds ONE school upstream call. The shared HTTP client
// allows 120s; three sequential school calls per account (tasks/config/
// vouchers) on a flaky gateway used to pin the whole 券码 handler long enough
// for the host management bridge to give up mid-response — the panel then
// parsed an EMPTY body and surfaced the cryptic "JSON.parse: unexpected end
// of data" instead of a real error. 20s per call keeps the scan responsive.
const schoolCallTimeout = 20 * time.Second

// schoolScanBudget bounds the ENTIRE handleSchoolVouchers scan: accounts that
// have not started by the deadline are reported as skipped instead of queued,
// so the handler always returns a complete JSON envelope well under the host
// management bridge timeout.
const schoolScanBudget = 45 * time.Second

// schoolCall is the school-season request: billing domain + activity prefix.
// method mirrors the caller (GET for reads, POST for actions); body == nil
// sends no request body. Envelope decode reuses growthDo so error shapes and
// 5xx handling stay identical to the growth endpoints.
func schoolCall(sa *storedAuth, method, path string, body any) (json.RawMessage, error) {
	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	ctx, cancel := context.WithTimeout(context.Background(), schoolCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, billingBaseFor(sa)+schoolBase+path, reader)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, sa)
	return growthDo(req)
}

// schoolTask is one entry of the school task list (field names per upstream
// response samples).
type schoolTask struct {
	TaskCode    string `json:"task_code"`
	Status      string `json:"status"` // pending | in_progress | completed | claimed
	Progress    int    `json:"progress"`
	TargetCount int    `json:"target_count"`
}

// schoolTasksList returns the task list and whether the activity is in
// period (in_period=false → the whole school loop should be skipped).
func schoolTasksList(sa *storedAuth) ([]schoolTask, bool, error) {
	data, err := schoolCall(sa, http.MethodGet, "/tasks", nil)
	if err != nil {
		return nil, false, err
	}
	var out struct {
		Tasks    []schoolTask `json:"tasks"`
		InPeriod bool         `json:"in_period"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, false, err
	}
	return out.Tasks, out.InPeriod, nil
}

// schoolShareComplete reports the daily share (share_invite 判据). Upstream
// note: pure front-end report — the server does not verify a real share
// receipt, same trust model as the growth activity report.
func schoolShareComplete(sa *storedAuth) error {
	_, err := schoolCall(sa, http.MethodPost, "/tasks/share-complete",
		map[string]any{"channel": "wechat"})
	return err
}

// schoolTaskViewed marks a task viewed (pending → in_progress). Counting
// prerequisite for tasks like desktop_chat_1_time: only behavior AFTER
// activation counts (three-account verified upstream).
func schoolTaskViewed(sa *storedAuth, taskCode string) error {
	_, err := schoolCall(sa, http.MethodPost, "/tasks/"+taskCode+"/viewed",
		map[string]any{})
	return err
}

// schoolClaimTask claims a completed task's reward and returns the granted
// lottery chance count (usually 1 per school task).
func schoolClaimTask(sa *storedAuth, taskCode string) (int, error) {
	data, err := schoolCall(sa, http.MethodPost, "/tasks/"+taskCode+"/claim",
		map[string]any{})
	if err != nil {
		return 0, err
	}
	var out struct {
		ChanceGranted int `json:"chance_granted"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, err
	}
	return out.ChanceGranted, nil
}

// schoolChances returns the current lottery chance balance (GET /config →
// chance.balance).
func schoolChances(sa *storedAuth) (int, error) {
	data, err := schoolCall(sa, http.MethodGet, "/config", nil)
	if err != nil {
		return 0, err
	}
	var out struct {
		Chance struct {
			Balance int `json:"balance"`
		} `json:"chance"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, err
	}
	return out.Chance.Balance, nil
}

// schoolDraw spins the wheel once. draw_uuid is client-minted (front-end
// randomUUID semantics, same as the growth lottery token). Returns a human
// description of the prize.
func schoolDraw(sa *storedAuth) (string, error) {
	data, err := schoolCall(sa, http.MethodPost, "/wheel/draw",
		map[string]any{"draw_uuid": growthClientToken()})
	if err != nil {
		return "", err
	}
	var out struct {
		PrizeCode    string `json:"prize_code"`
		CreditAmount int    `json:"credit_amount"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if out.CreditAmount > 0 {
		return fmt.Sprintf("%s +%dc", out.PrizeCode, out.CreditAmount), nil
	}
	return out.PrizeCode, nil
}

// schoolVoucher is one third-party coupon won from the school wheel
// (KFC/瑞幸/酷狗…). Field names per upstream real-response samples; single
// unpaginated pull (data.items[]).
type schoolVoucher struct {
	GrantID   int64  `json:"grant_id"`
	SKUCode   string `json:"sku_code,omitempty"`   // kfc_ice_cream / voucher_luckin / voucher_kugou …
	PrizeName string `json:"prize_name,omitempty"` // 肯德基冰淇淋 …
	Code      string `json:"code"`                 // 券码本体（复制给店员核销）
	ValidTo   string `json:"valid_to,omitempty"`   // "2026-10-24"
	GrantedAt string `json:"granted_at,omitempty"` // RFC3339
}

// schoolVouchers lists the account's school-season vouchers (read-only).
// Credit wins are NOT here (those live in the growth /rewards type=credit
// entries).
func schoolVouchers(sa *storedAuth) ([]schoolVoucher, error) {
	data, err := schoolCall(sa, http.MethodGet, "/vouchers", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []schoolVoucher `json:"items"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// schoolTaskNeedsView reports whether a pending task benefits from a viewed
// activation (counting prerequisite — activating never hurts; completed /
// claimed entries must not be touched).
func schoolTaskNeedsView(t schoolTask) bool {
	return t.Status == "pending" && strings.TrimSpace(t.TaskCode) != ""
}

// schoolTaskClaimable reports whether a task has crossed its target and can
// be claimed (completed status, or progress ≥ target with any status that
// upstream marks as completable).
func schoolTaskClaimable(t schoolTask) bool {
	if t.Status == "claimed" || strings.TrimSpace(t.TaskCode) == "" {
		return false
	}
	if t.Status == "completed" {
		return true
	}
	return t.TargetCount > 0 && t.Progress >= t.TargetCount
}

// -----------------------------------------------------------------------------
// School loop orchestration (task-center step 9)
// -----------------------------------------------------------------------------

// tasksSchoolOnce runs one pass of the school-season loop for one account:
// share-complete (daily +100c +1 chance) → activate pending tasks (viewed) →
// claim completed tasks → drain lottery chances → vouchers recap.
//
// Silence rules keep the daily task summary clean: when the activity is not
// running (in_period=false, endpoint gone → any /tasks error), the whole step
// emits NOTHING — post-window runs look identical to pre-0.9.14 runs. During
// the window, "already done" business errors stay silent too (same policy as
// the growth gift claims); genuine failures surface as lines.
func tasksSchoolOnce(sa *storedAuth, add func(string, ...any)) {
	tasks, inPeriod, err := schoolTasksList(sa)
	if err != nil || !inPeriod {
		return
	}

	// 1. Daily share: +100c +1 chance, pure report.
	if err := schoolShareComplete(sa); err == nil {
		add("开学季分享 ok（+100 分 +1 抽奖）")
	} else if msg := err.Error(); !strings.Contains(msg, "已") && !strings.Contains(strings.ToLower(msg), "already") {
		add("开学季分享失败: %s", err)
	}

	// 2. Activate pending tasks (viewed) — counting prerequisite.
	activated := 0
	for _, t := range tasks {
		if !schoolTaskNeedsView(t) {
			continue
		}
		if err := schoolTaskViewed(sa, t.TaskCode); err == nil {
			activated++
		}
	}
	if activated > 0 {
		add("开学季激活任务 %d 个", activated)
	}

	// 3. Claim every completable task (chance_granted shown when nonzero).
	for _, t := range tasks {
		if !schoolTaskClaimable(t) {
			continue
		}
		chance, err := schoolClaimTask(sa, t.TaskCode)
		switch {
		case err != nil:
			msg := strings.ToLower(err.Error())
			if strings.Contains(err.Error(), "已") || strings.Contains(msg, "already") {
				continue
			}
			add("开学季领取 %s 失败: %s", t.TaskCode, err)
		case chance > 0:
			add("开学季领取 %s: +1 抽奖", t.TaskCode)
		default:
			add("开学季领取 %s: 已领过", t.TaskCode)
		}
	}

	// 4. Drain lottery chances.
	if chances, err := schoolChances(sa); err == nil && chances > 0 {
		for i := 0; i < chances; i++ {
			prize, err := schoolDraw(sa)
			if err != nil {
				add("开学季抽奖失败(第%d次): %s", i+1, err)
				break
			}
			add("开学季抽奖#%d: %s", i+1, prize)
		}
	}

	// 5. Vouchers recap (codes themselves live in the panel 券码 dialog).
	if vs, err := schoolVouchers(sa); err == nil && len(vs) > 0 {
		names := make([]string, 0, len(vs))
		for _, v := range vs {
			name := v.PrizeName
			if name == "" {
				name = v.SKUCode
			}
			names = append(names, name)
		}
		if len(names) > 4 {
			names = append(names[:4], fmt.Sprintf("…共%d张", len(vs)))
		}
		add("开学季券码 %d 张: %s（面板可查）", len(vs), strings.Join(names, "、"))
	}
}

// handleSchoolVouchers serves GET /v0/management/plugins/workbuddy/school/vouchers —
// read-only voucher + chance scan for the panel 券码 dialog (the full /tasks
// scan costs 5 upstream calls per account; this one costs 2 and returns only
// the school-season state). CN accounts only; global/intl return a clean
// skipped entry. Failures are per-account, never fatal.
func handleSchoolVouchers(req pluginapi.ManagementRequest) map[string]any {
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	out := make([]map[string]any, 0, len(files))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	deadline := time.Now().Add(schoolScanBudget)
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Scan budget (v0.12.64): accounts that start after the
			// deadline are reported skipped instead of queued — an
			// unbounded scan on a slow gateway pinned the handler past
			// the host bridge timeout and the panel parsed an empty
			// body ("JSON.parse: unexpected end of data").
			if !time.Now().Before(deadline) {
				mu.Lock()
				out = append(out, map[string]any{
					"auth_index": f.AuthIndex,
					"skipped":    true,
					"reason":     "scan budget exceeded（扫描超时，稍后重试）",
				})
				mu.Unlock()
				return
			}
			sa, err := hostAuthGet(f.AuthIndex)
			if err != nil {
				mu.Lock()
				out = append(out, map[string]any{"auth_index": f.AuthIndex, "error": err.Error()})
				mu.Unlock()
				return
			}
			entry := map[string]any{
				"auth_index": f.AuthIndex,
				"nickname":   sa.Account.Nickname,
			}
			if !tasksSupportsGrowth(sa) {
				entry["skipped"] = true
				entry["reason"] = "non-cn"
				mu.Lock()
				out = append(out, entry)
				mu.Unlock()
				return
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			if _, inPeriod, err := schoolTasksList(sa); err != nil || !inPeriod {
				// Activity gone/off → nothing to show for this account.
				entry["school_in_period"] = false
				mu.Lock()
				out = append(out, entry)
				mu.Unlock()
				return
			}
			entry["school_in_period"] = true
			if chances, err := schoolChances(sa); err == nil {
				entry["school_chances"] = chances
			}
			if vs, err := schoolVouchers(sa); err == nil {
				vView := make([]map[string]any, 0, len(vs))
				for _, v := range vs {
					vView = append(vView, map[string]any{
						"prize_name": v.PrizeName,
						"sku_code":   v.SKUCode,
						"code":       v.Code,
						"valid_to":   v.ValidTo,
						"granted_at": v.GrantedAt,
					})
				}
				entry["vouchers"] = vView
				entry["voucher_count"] = len(vView)
			}
			mu.Lock()
			out = append(out, entry)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool {
		a, _ := out[i]["auth_index"].(string)
		b, _ := out[j]["auth_index"].(string)
		return a < b
	})
	return map[string]any{"accounts": out}
}
