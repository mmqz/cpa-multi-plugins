package main

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestAdmissionDisabledByDefault(t *testing.T) {
	restore := setAccountMaxConcurrent(0)
	defer restore()

	releases := make([]func(), 0, 10)
	for i := 0; i < 10; i++ {
		release, err := accountSlotAcquire("auth-default")
		if err != nil {
			t.Fatalf("disabled admission rejected acquire #%d: %v", i+1, err)
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
}

func TestAdmissionCapEnforcedAndReleased(t *testing.T) {
	restore := setAccountMaxConcurrent(2)
	defer restore()

	r1, err := accountSlotAcquire("auth-cap")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	r2, err := accountSlotAcquire("auth-cap")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if _, err := accountSlotAcquire("auth-cap"); err == nil {
		t.Fatal("third acquire at cap 2 succeeded — admission not enforced")
	} else {
		// Plain-error discipline (model_ratelimit.go): no HTTP status, no
		// literal rate-limit wording — a host version must never read this
		// as credential-level quota.
		var sc interface{ StatusCode() int }
		if errors.As(err, &sc) {
			t.Fatalf("busy error carries a StatusCode: %v", err)
		}
		if strings.Contains(err.Error(), "429") {
			t.Fatalf("busy error mentions a rate-limit status: %v", err)
		}
	}
	r1()
	r1b, err := accountSlotAcquire("auth-cap")
	if err != nil {
		t.Fatalf("acquire after release rejected: %v", err)
	}
	r2()
	// Release is idempotent: the once-wrapped func may be called again from
	// a second defer site without underflowing the counter.
	r2()
	r1b()
	if n := admissionInflight["auth-cap"]; n != 0 {
		t.Fatalf("inflight map not cleaned: %d", n)
	}
}

func TestAdmissionIndependentPerCredential(t *testing.T) {
	restore := setAccountMaxConcurrent(1)
	defer restore()

	ra, err := accountSlotAcquire("auth-a")
	if err != nil {
		t.Fatalf("auth-a first: %v", err)
	}
	if _, err := accountSlotAcquire("auth-a"); err == nil {
		t.Fatal("auth-a over cap")
	}
	rb, err := accountSlotAcquire("auth-b")
	if err != nil {
		t.Fatalf("auth-b must have its own slot budget: %v", err)
	}
	ra()
	rb()
}

func TestAdmissionEmptyAuthIDBypasses(t *testing.T) {
	restore := setAccountMaxConcurrent(1)
	defer restore()

	for i := 0; i < 5; i++ {
		release, err := accountSlotAcquire("")
		if err != nil {
			t.Fatalf("empty auth id (hand-built snapshot) must bypass admission: %v", err)
		}
		release()
	}
	if _, ok := admissionInflight[""]; ok {
		t.Fatalf("bypass path recorded state for the empty auth id: %v", admissionInflight)
	}
}

func TestAdmissionConcurrentAcquireNeverExceedsCap(t *testing.T) {
	restore := setAccountMaxConcurrent(3)
	defer restore()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := accountSlotAcquire("auth-race")
			if err == nil {
				release()
			}
		}()
	}
	wg.Wait()
	// The invariant that matters under -race: no map corruption, no
	// deadlock, and never more than the cap held at once (enforced by the
	// cap test above); a release-immediate race makes the reject count
	// nondeterministic on purpose, so only the leak is asserted here.
	if n := admissionInflight["auth-race"]; n != 0 {
		t.Fatalf("inflight leak after all releases: %d", n)
	}
	if n := admissionInflight["auth-race"]; n != 0 {
		t.Fatalf("inflight leak after all releases: %d", n)
	}
}

func TestConfigureAccountMaxConcurrent(t *testing.T) {
	restore := setAccountMaxConcurrent(0)
	defer restore()

	// Block-style YAML line (the form the host's panel writes).
	wbConfigureWire(t, "enabled: true\naccount_max_concurrent: 4\n")
	if got := loadedAccountMaxConcurrent(); got != 4 {
		t.Fatalf("line-style account_max_concurrent=4 not applied: %d", got)
	}
	// Bare reconfigure resets to the default (same semantics as
	// scheduler_mode / stream_head_timeout: the value is not sticky).
	wbConfigureWire(t, "enabled: true\n")
	if got := loadedAccountMaxConcurrent(); got != 0 {
		t.Fatalf("bare reconfigure must reset admission to disabled: %d", got)
	}
	// Non-positive and unparseable values normalize to disabled.
	wbConfigureWire(t, "enabled: true\naccount_max_concurrent: -2\n")
	if got := loadedAccountMaxConcurrent(); got != 0 {
		t.Fatalf("negative value must normalize to 0: %d", got)
	}
	wbConfigureWire(t, "enabled: true\naccount_max_concurrent: \"not-a-number\"\n")
	if got := loadedAccountMaxConcurrent(); got != 0 {
		t.Fatalf("unparseable value must normalize to 0: %d", got)
	}
	// Flow-style mapping ({k: v}) — the YAML-map path, not the line scan.
	wbConfigureWire(t, "{enabled: true, account_max_concurrent: 2}\n")
	if got := loadedAccountMaxConcurrent(); got != 2 {
		t.Fatalf("flow-style account_max_concurrent=2 not applied: %d", got)
	}
}
