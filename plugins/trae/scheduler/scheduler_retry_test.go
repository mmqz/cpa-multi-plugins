package scheduler

import (
	"testing"
	"time"
)

// v0.12.39: 9074 当日退避重试节奏（前密后疏，贴 0 点重置抢签）。
func TestCheckinRetryDelay(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 1 * time.Minute},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, 16 * time.Minute},
		{6, 64 * time.Minute},
		{7, 2 * time.Hour}, // 128m → 封顶 2h
		{24, 2 * time.Hour}, // 移位溢出防护
	}
	for _, c := range cases {
		if got := checkinRetryDelay(c.attempt); got != c.want {
			t.Errorf("checkinRetryDelay(%d) = %s, want %s", c.attempt, got, c.want)
		}
	}
}

func TestScheduleCheckinRetry(t *testing.T) {
	s := New(Config{})

	// 无 9074 → 复位计数、撤销挂起定时器、不新增定时器
	s.retryAttempt = 5
	s.scheduleCheckinRetry(0)
	if s.retryAttempt != 0 || s.retryTimer != nil {
		t.Fatalf("clean round should reset: attempt=%d timer=%v", s.retryAttempt, s.retryTimer)
	}

	// 首次 9074 → 挂定时器、计数 +1
	s.scheduleCheckinRetry(1)
	if s.retryTimer == nil || s.retryAttempt != 1 {
		t.Fatalf("expected scheduled retry: timer=%v attempt=%d", s.retryTimer, s.retryAttempt)
	}
	if s.retryTimer != nil {
		s.retryTimer.Stop()
	}

	// 干净一轮 → 定时器撤销
	s.scheduleCheckinRetry(0)
	if s.retryTimer != nil || s.retryAttempt != 0 {
		t.Fatalf("clean round should cancel timer: timer=%v attempt=%d", s.retryTimer, s.retryAttempt)
	}

	// 当日预算用尽 → 不再挂新定时器
	s.retryDay = time.Now().YearDay()
	s.retryAttempt = maxCheckinRetries
	s.scheduleCheckinRetry(2)
	if s.retryTimer != nil {
		t.Fatal("budget exhausted should not schedule")
	}

	// 跨天 → 计数复位后可再次调度
	s.retryDay = 0
	s.scheduleCheckinRetry(1)
	if s.retryTimer == nil || s.retryAttempt != 1 {
		t.Fatalf("new day should reset budget: timer=%v attempt=%d", s.retryTimer, s.retryAttempt)
	}
	if s.retryTimer != nil {
		s.retryTimer.Stop()
	}
}
