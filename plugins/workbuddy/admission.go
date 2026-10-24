// admission.go — opt-in per-credential in-flight chat admission control.
//
// The host's scheduler picks a credential and calls the executor; what it
// cannot see is that this plugin holds the upstream call open for the whole
// streaming response. Two conversations landing on one credential therefore
// mean two long-lived upstream calls on the same account — one big-context
// request alone carries hundreds of thousands of tokens, and the upstream
// meters per (account, minute), so parallel calls on one credential are the
// fastest path to a model-scoped 6004 that then cools the pair for everyone.
// workbuddy2api-hub runs the same policy (its 在飞名额 admission): a
// credential at capacity is skipped for THIS request and the next candidate
// serves it — never local queueing, which would just move the 429 later and
// pile latency on top.
//
// account_max_concurrent: 0 (the default) disables admission entirely — no
// state, byte-for-byte the old dispatch. N > 0 caps concurrent executor
// calls per credential. At capacity the executor fails fast with a PLAIN
// error — no HTTP status, no literal rate-limit wording, same discipline as
// model_ratelimit.go — so the host rotates to another candidate instead of
// reading it as this credential's quota trouble.
package main

import (
	"fmt"
	"sync"
)

var (
	// accountMaxConcurrent is the configured per-credential in-flight cap
	// (0 = disabled). Guarded by its own lock, applied by configure().
	accountMaxConcurrent   int
	accountMaxConcurrentMu sync.RWMutex

	// admissionInflight counts held slots per credential auth id.
	admissionMu       sync.Mutex
	admissionInflight = map[string]int{}
)

// setAccountMaxConcurrent is the test seam: it swaps the cap and returns a
// restore func (same shape as setSchedulerMode / setBillingBase).
func setAccountMaxConcurrent(n int) func() {
	accountMaxConcurrentMu.Lock()
	old := accountMaxConcurrent
	accountMaxConcurrent = n
	accountMaxConcurrentMu.Unlock()
	return func() {
		accountMaxConcurrentMu.Lock()
		accountMaxConcurrent = old
		accountMaxConcurrentMu.Unlock()
	}
}

// loadedAccountMaxConcurrent returns the configured cap; <= 0 means off.
func loadedAccountMaxConcurrent() int {
	accountMaxConcurrentMu.RLock()
	defer accountMaxConcurrentMu.RUnlock()
	return accountMaxConcurrent
}

// accountSlotAcquire takes one in-flight slot for the credential. It returns
// a release func (idempotent — safe to call from both a defer and a wrapped
// goroutine) and a nil error; when admission is off, or the auth id is empty
// (hand-built snapshots: tests, pre-persist login flows), it returns a no-op
// release. When the credential is at capacity it returns a plain error so
// the host rotates to another candidate — nothing upstream is touched.
func accountSlotAcquire(authID string) (func(), error) {
	if authID == "" || loadedAccountMaxConcurrent() <= 0 {
		return func() {}, nil
	}
	admissionMu.Lock()
	cur := admissionInflight[authID]
	if cur >= loadedAccountMaxConcurrent() {
		admissionMu.Unlock()
		return nil, fmt.Errorf("credential busy: %d in-flight chat call(s) already held (account_max_concurrent=%d); fail-fast so the host rotates to another candidate", cur, loadedAccountMaxConcurrent())
	}
	admissionInflight[authID] = cur + 1
	admissionMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			admissionMu.Lock()
			if n := admissionInflight[authID]; n <= 1 {
				delete(admissionInflight, authID)
			} else {
				admissionInflight[authID] = n - 1
			}
			admissionMu.Unlock()
		})
	}, nil
}
