package main

import (
	"errors"
	"strings"
	"testing"
)

var errFakeBridgeDown = errors.New("fake bridge down")

// -----------------------------------------------------------------------------
// persistLoginCredential — plugin-side login persistence (v0.9.44)
// -----------------------------------------------------------------------------

type recordedAuthSave struct {
	name string
	raw  []byte
}

func withRecordedAuthPersist(t *testing.T) *[]recordedAuthSave {
	t.Helper()
	saves := &[]recordedAuthSave{}
	old := hostAuthPersistFn
	hostAuthPersistFn = func(name, path string, raw []byte) error {
		*saves = append(*saves, recordedAuthSave{name: name, raw: raw})
		return nil
	}
	t.Cleanup(func() { hostAuthPersistFn = old })
	return saves
}

func TestPersistLoginCredentialCanonicalName(t *testing.T) {
	saves := withRecordedAuthPersist(t)

	cn := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok-cn", Domain: "copilot.tencent.com", Region: "cn"},
		Account: storedAccount{UID: "100200", Nickname: "cn-acct"},
	}
	persistLoginCredential(cn)
	intl := &storedAuth{
		Auth:    storedTokens{AccessToken: "tok-intl", Domain: "codebuddy.ai", Region: "intl"},
		Account: storedAccount{UID: "100200", Nickname: "intl-acct"},
	}
	persistLoginCredential(intl)

	if len(*saves) != 2 {
		t.Fatalf("saves = %d, want 2", len(*saves))
	}
	if (*saves)[0].name != "workbuddy-100200.json" {
		t.Errorf("cn save name = %q, want workbuddy-100200.json", (*saves)[0].name)
	}
	// The whole point of v0.9.44: the intl credential must NOT share the CN
	// file — same uid, different realms, both live.
	if (*saves)[1].name != "workbuddy-intl-100200.json" {
		t.Errorf("intl save name = %q, want workbuddy-intl-100200.json", (*saves)[1].name)
	}
	for i, wantUID := range []string{"100200", "100200"} {
		sa, err := parseStored((*saves)[i].raw)
		if err != nil {
			t.Fatalf("save %d does not parse back: %v", i, err)
		}
		if sa.Account.UID != wantUID {
			t.Errorf("save %d uid = %q, want %q", i, sa.Account.UID, wantUID)
		}
		if !strings.Contains(string((*saves)[i].raw), `"type":"workbuddy"`) {
			t.Errorf("save %d body misses the workbuddy type stamp: %s", i, (*saves)[i].raw)
		}
		if !strings.Contains(string((*saves)[i].raw), `"auth_kind":"oauth"`) {
			t.Errorf("save %d body misses the oauth attribution: %s", i, (*saves)[i].raw)
		}
	}
}

func TestPersistLoginCredentialSwallowsBridgeFailure(t *testing.T) {
	old := hostAuthPersistFn
	hostAuthPersistFn = func(name, path string, raw []byte) error {
		return errFakeBridgeDown
	}
	t.Cleanup(func() { hostAuthPersistFn = old })

	// Must not panic and must not return — persistence is best-effort; the
	// host's post-poll save remains primary.
	persistLoginCredential(&storedAuth{
		Auth:    storedTokens{AccessToken: "t"},
		Account: storedAccount{UID: "7"},
	})
}

// -----------------------------------------------------------------------------
// toAuthDataOptsWithNote — AuthData.FileName rides the file-layer rule
// -----------------------------------------------------------------------------

func TestToAuthDataFileNameMatchesFileLayer(t *testing.T) {
	cases := []struct {
		name     string
		domain   string
		uid      string
		wantFile string
		wantID   string
	}{
		{"cn uid", "copilot.tencent.com", "100200", "workbuddy-100200.json", "100200"},
		{"intl uid region-qualified", "codebuddy.ai", "100200", "workbuddy-intl-100200.json", "100200"},
		{"empty uid falls back to legacy", "copilot.tencent.com", "", "workbuddy.json", "workbuddy"},
		{"nil auth", "", "", "workbuddy.json", "workbuddy"},
	}
	for _, tc := range cases {
		var sa *storedAuth
		if tc.name != "nil auth" {
			sa = &storedAuth{
				Auth:    storedTokens{Domain: tc.domain},
				Account: storedAccount{UID: tc.uid},
			}
		}
		ad := toAuthDataOpts(sa, nil, false)
		if ad.FileName != tc.wantFile {
			t.Errorf("%s: FileName = %q, want %q", tc.name, ad.FileName, tc.wantFile)
		}
		if ad.ID != tc.wantID {
			t.Errorf("%s: ID = %q, want %q", tc.name, ad.ID, tc.wantID)
		}
	}
}
