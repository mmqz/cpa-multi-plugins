// Package scheduler 定时任务：每日签到 + token 预刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/mmqz/cpa-multi-plugins/plugins/trae/pool"
	"github.com/mmqz/cpa-multi-plugins/plugins/trae/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	CheckinHour  int           // 每日签到小时，默认 9
	RefreshHours []int         // token 预刷新小时，默认 [3]
	RefreshSkew  time.Duration // 预刷新窗口，默认 24h
}

// 9074（"当前参与用户太多，请稍后再试"）是官方瞬时限流，语义为"稍后再试"。
// 旧实现撞 9074 后只等"下个周期"，而签到周期每天一次（CheckinHour）——
// 等于整天放弃该账号。v0.12.33 起当日内按指数退避自动重试：
// 10m→20m→40m→80m→2h（封顶），当日最多 maxCheckinRetries 次；
// 出现一轮无 9074 或跨天即复位。
const (
	baseCheckinRetry     = 10 * time.Minute
	maxCheckinRetryDelay = 2 * time.Hour
	maxCheckinRetries    = 8
)

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	runMu sync.Mutex // 串行化 RunCheckinNow（每日主循环 / 重试定时器可并发进入）

	retryMu      sync.Mutex  // 保护以下重试状态
	retryTimer   *time.Timer // 待触发的 9074 重试定时器
	retryAttempt int         // 当日已重试次数
	retryDay     int         // 上次调度时的 YearDay，跨天复位
}

// New 构建。
func New(cfg Config) *Scheduler {
	if cfg.CheckinHour < 0 {
		cfg.CheckinHour = 9
	}
	if len(cfg.RefreshHours) == 0 {
		cfg.RefreshHours = []int{3}
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 24 * time.Hour
	}
	return &Scheduler{cfg: cfg}
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	all := append(append([]int{}, s.cfg.RefreshHours...), s.cfg.CheckinHour)
	for {
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			h := time.Now().Hour()
			if contains(s.cfg.RefreshHours, h) {
				s.RunRefreshNow()
			}
			if s.cfg.CheckinHour == h {
				s.RunCheckinNow()
			}
		}
	}
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// RunCheckinNow 立即对所有账号执行签到 + 积分刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	// v0.12.33: 每日主循环与 9074 重试定时器可能并发进入；串行化避免重复签到请求。
	s.runMu.Lock()
	defer s.runMu.Unlock()

	rateLimited := 0
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// 签到（status → 未签到则 claim）
		// 对齐 cockpit-tools workbuddy_auto_checkin.rs 指数退避策略。
		// v0.12.28: CheckinStatus/CheckinClaim 现在在上游业务码非零时返回
		// *upstream.Error（9074 限流 → BizCode=9074，其余 → 会话失效），
		// 与上游 trae_account_token_injection.rs 的 code!=0 报错语义一致；
		// 未签到状态不再被静默吞掉。
		status, err := s.cfg.Upstream.CheckinStatus(a)
		if err != nil {
			if isBizRateLimit(err) {
				rateLimited++
				log.Printf("checkin %s: rate-limited (9074), will auto-retry today", st.UID)
			} else {
				log.Printf("checkin status %s: %v", st.UID, err)
			}
			status = nil
		} else if !status.CheckedIn && status.Enable {
			// v0.12.32: 官方 claim 需要 x-device-id 携带真实绑定 did；缺失时
			// 服务端可能 code=0 但静默不入账（FINDINGS §四/§五）。提前告警。
			if a.DeviceID == "" {
				log.Printf("checkin %s: WARNING auth has no deviceId — claim may be silently dropped (x-device-id missing)", st.UID)
			}
			if _, err := s.cfg.Upstream.CheckinClaim(a); err != nil {
				if isBizRateLimit(err) {
					rateLimited++
					log.Printf("checkin %s: claim rate-limited (9074), will auto-retry today", st.UID)
				} else {
					log.Printf("checkin claim %s: %v", st.UID, err)
				}
			} else {
				// v0.12.31: 领取成功后重查状态（对齐上游 claim_trae_checkin
				// trae_account_token_injection.rs:2891"领取后重新查询状态"）。
				// 旧日志用签到前余额充当结果（"ok (credits=150)"实为签到前
				// 钱包），奖励（前后差值）与签到后余额分开记录。
				if after, stErr := s.cfg.Upstream.CheckinStatus(a); stErr == nil {
					awarded := after.Credits - status.Credits
					if awarded < 0 {
						awarded = 0
					}
					log.Printf("checkin %s: ok +%d (wallet %d)", st.UID, awarded, after.Credits)
					status = after
				} else {
					log.Printf("checkin %s: claimed, wallet refresh failed: %v", st.UID, stErr)
				}
			}
		} else if status.CheckedIn {
			log.Printf("checkin %s: already checked in (wallet %d)", st.UID, status.Credits)
		}
		// 查积分 + 解冻
		// 对齐 cockpit-tools apply_usage_response：按 pack 优先级选最高 pack。
		// v0.12.28: 套餐剩余改为上游用量模型（basic_usage_limit-amount /
		// fast-request 可用次数），credits_limit 字段在 v2 API 中不存在，
		// 读它永远是 0（"签到 150 积分一刷新就归零"的显示层根因）。
		// 池子积分 = 套餐剩余（未知则 0）+ 签到钱包。
		usage, err := s.cfg.Upstream.UserEntUsage(a)
		if err != nil {
			log.Printf("ent-usage %s: %v", st.UID, err)
			continue
		}
		// SOLO CN 永远是 CN
		remain, _ := upstream.PackListRemain(usage.UserEntitlementPackList, true)
		wallet := int64(0)
		if status != nil {
			wallet = status.Credits
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain+wallet)
	}

	// v0.12.33: 本轮有 9074 → 当日指数退避自动重试；无 9074 → 复位并撤销挂起定时器。
	s.scheduleCheckinRetry(rateLimited)
}

// checkinRetryDelay 返回第 attempt 次重试的退避时长：10m 起指数翻倍，2h 封顶。
// attempt 很大时位移动溢出为负，统一归到封顶值。
func checkinRetryDelay(attempt int) time.Duration {
	d := baseCheckinRetry << uint(attempt)
	if d <= 0 || d > maxCheckinRetryDelay {
		return maxCheckinRetryDelay
	}
	return d
}

// scheduleCheckinRetry 依据本轮 9074 账号数安排当日自动重试。
// rateLimited==0：复位计数并撤销挂起的重试（本轮全部成功或已签）。
// 当日预算（maxCheckinRetries）用尽：放弃，等次日主循环再试。
func (s *Scheduler) scheduleCheckinRetry(rateLimited int) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if rateLimited == 0 {
		s.retryAttempt = 0
		if s.retryTimer != nil {
			s.retryTimer.Stop()
			s.retryTimer = nil
		}
		return
	}
	if yday := time.Now().YearDay(); s.retryDay != yday {
		s.retryDay = yday
		s.retryAttempt = 0
	}
	if s.retryAttempt >= maxCheckinRetries {
		log.Printf("checkin: %d account(s) still rate-limited (9074), daily retry budget exhausted; next try at daily checkin hour", rateLimited)
		return
	}
	delay := checkinRetryDelay(s.retryAttempt)
	s.retryAttempt++
	if s.retryTimer != nil {
		s.retryTimer.Stop()
	}
	log.Printf("checkin: %d account(s) rate-limited (9074), auto-retry in %s (attempt %d/%d)", rateLimited, delay, s.retryAttempt, maxCheckinRetries)
	s.retryTimer = time.AfterFunc(delay, s.RunCheckinNow)
}

// NotifyCheckinRateLimited 供手动签到路径（management）复用：手动 claim/status
// 撞上 9074 时，同样进入当日退避重试节奏，无需用户反复手点。
func (s *Scheduler) NotifyCheckinRateLimited() {
	s.scheduleCheckinRetry(1)
}

// isBizRateLimit 报告 err 是否为上游 9074 业务限流（签到重试语义）。
func isBizRateLimit(err error) bool {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		return upstream.IsRateLimit9074(ue.BizCode)
	}
	return false
}

// RunRefreshNow 立即对所有账号刷新 token；session 失效的自动禁用。
func (s *Scheduler) RunRefreshNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if !a.NeedsRefresh(s.cfg.RefreshSkew) {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("refresh %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "session dead")
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("refresh %s save: %v", st.UID, err)
		}
	}
}
