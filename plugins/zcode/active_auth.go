// active_auth.go tracks the panel-selected zcode account used for routing.
// Default: first available candidate. When the active account is
// exhausted/disabled/missing, switch to another non-exhausted candidate and
// remember the choice.
package main

import (
	"strings"
	"sync"
)

var (
	activeAuthID string
	activeAuthMu sync.RWMutex
)

func getActiveAuthID() string {
	activeAuthMu.RLock()
	defer activeAuthMu.RUnlock()
	return strings.TrimSpace(activeAuthID)
}

func setActiveAuthID(id string) {
	id = strings.TrimSpace(id)
	activeAuthMu.Lock()
	activeAuthID = id
	activeAuthMu.Unlock()
}

// activeAuthCandidate is a thin view used by pickActiveAuth.
type activeAuthCandidate struct {
	ID        string
	Disabled  bool
	Exhausted bool
}

// pickActiveAuth chooses which zcode auth to use from host candidates.
// The panel selection is sticky: it stays on the current account unless that
// account is no longer in the candidate list (disabled/deleted by host) or
// is marked exhausted in cache. When switching, it picks the first
// non-exhausted candidate and updates activeAuthID.
func pickActiveAuth(candidates []activeAuthCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	byID := make(map[string]activeAuthCandidate, len(candidates))
	for _, c := range candidates {
		byID[c.ID] = c
	}

	cur := getActiveAuthID()
	if cur != "" {
		if c, ok := byID[cur]; ok && !c.Disabled && !c.Exhausted {
			return cur
		}
	}

	var next string
	for _, c := range candidates {
		if !c.Disabled && !c.Exhausted {
			next = c.ID
			break
		}
	}
	if next == "" {
		if cur != "" {
			if _, ok := byID[cur]; ok {
				return cur
			}
		}
		next = candidates[0].ID
	}
	if next != "" && next != cur {
		setActiveAuthID(next)
	}
	return next
}

// ensureDefaultActiveAuth keeps the panel selection aligned with pickActiveAuth.
// Called from buildDashboardEx on every /accounts and /refresh request.
func ensureDefaultActiveAuth(accounts []zcAccount) string {
	cur := getActiveAuthID()
	live := make(map[string]zcAccount, len(accounts))
	for _, a := range accounts {
		live[a.AuthID] = a
	}

	// Rule 1: current selection is live AND not exhausted → keep.
	if cur != "" {
		if a, ok := live[cur]; ok && !a.Disabled && !a.Exhausted {
			return cur
		}
	}

	var firstAny, firstOK, firstReady string
	for _, a := range accounts {
		if firstAny == "" {
			firstAny = a.AuthID
		}
		if a.Disabled {
			continue
		}
		if firstOK == "" {
			firstOK = a.AuthID
		}
		if !a.Exhausted && firstReady == "" {
			firstReady = a.AuthID
		}
	}

	next := firstReady
	if next == "" {
		if cur != "" {
			if a, ok := live[cur]; ok && !a.Disabled {
				return cur
			}
		}
		next = firstOK
	}
	if next == "" {
		next = firstAny
	}
	if next != "" && next != cur {
		setActiveAuthID(next)
	}
	return next
}
