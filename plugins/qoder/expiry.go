package main

// expiry.go — normalise the stored token expiry magnitude. Adapted from
// bfSan/qoder-cpa-plugin f05e9e3.
//
// The stored expiresAt is documented as unix seconds (main.go storedTokens),
// but historical writes and one consumer treated it as milliseconds. A stale
// millisecond value read as seconds resolves to 1970, which marks every token
// long expired and skews refresh scheduling.
//
// tokenExpiryUnix is the single read path: it normalises an ambiguous
// magnitude to seconds so no caller has to guess.

import "time"

// secondsFloor is the smallest unix-seconds value we accept as plausible
// (2020-01-01). Anything below is treated as milliseconds.
const secondsFloor = 1577836800

// millisFloor is the smallest unix-milliseconds value we accept as plausible
// (2020-01-01 in ms). Anything below is treated as bogus.
const millisFloor = secondsFloor * 1000

func tokenExpiryUnix(v int64) int64 {
	switch {
	case v <= 0:
		return 0
	case v >= millisFloor:
		// Clearly milliseconds — convert down.
		return v / 1000
	case v >= secondsFloor:
		// Plausible seconds already.
		return v
	default:
		return 0
	}
}

// tokenExpiryTime converts the stored value to a time, zero when unknown.
func tokenExpiryTime(v int64) time.Time {
	unix := tokenExpiryUnix(v)
	if unix <= 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0)
}

// tokenExpired reports whether the stored expiry is known and in the past.
// Unknown expiry is not expired — callers must not assume a zero value means
// "dead", otherwise a credential with a missing timestamp would be refreshed
// on every request.
func tokenExpired(v int64, now time.Time) bool {
	expiry := tokenExpiryTime(v)
	if expiry.IsZero() {
		return false
	}
	return now.After(expiry)
}
