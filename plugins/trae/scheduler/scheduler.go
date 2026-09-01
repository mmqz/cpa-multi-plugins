// Package scheduler 定时任务：每日签到 + token 预刷新。
// 签到成功后重新查积分，积分 > 0 的冷却账号自动解冻。
package scheduler

import (
        "context"
        "errors"
        "log"
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

// Scheduler 调度器。
type Scheduler struct {
        cfg Config
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
                status, err := s.cfg.Upstream.CheckinStatus(a)
                if err != nil {
                        log.Printf("checkin status %s: %v", st.UID, err)
                } else if upstream.IsRateLimit9074(status.Code) {
                        // 9074 限流：跳过本轮，下一轮重试（外层调度器有 30s/5min/10min/... 退避）
                        log.Printf("checkin %s: rate-limited (9074), will retry next cycle", st.UID)
                } else if !status.CheckedIn && status.Enable {
                        claim, err := s.cfg.Upstream.CheckinClaim(a)
                        if err != nil {
                                log.Printf("checkin claim %s: %v", st.UID, err)
                        } else if upstream.IsRateLimit9074(claim.Code) {
                                log.Printf("checkin %s: claim rate-limited (9074), will retry", st.UID)
                        } else {
                                log.Printf("checkin %s: ok (credits=%d)", st.UID, status.Credits)
                        }
                } else if status.CheckedIn {
                        log.Printf("checkin %s: already checked in (credits=%d)", st.UID, status.Credits)
                }
                // 查积分 + 解冻
                // 对齐 cockpit-tools apply_usage_response：按 pack 优先级选最高 pack，
                // 用选中 pack 的 credits_limit 作为 remain（而非 sum 全部 pack）。
                usage, err := s.cfg.Upstream.UserEntUsage(a)
                if err != nil {
                        log.Printf("ent-usage %s: %v", st.UID, err)
                        continue
                }
                // SOLO CN 永远是 CN
                selected := upstream.SelectActivePack(usage.UserEntitlementPackList, true)
                var remain int64
                if selected != nil {
                        remain = selected.EntitlementBaseInfo.Quota.CreditsLimit
                }
                s.cfg.Pool.ReenableIfCredits(st.UID, remain)
        }
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
