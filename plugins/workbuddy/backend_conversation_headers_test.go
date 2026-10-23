package main

import (
	"net/http"
	"regexp"
	"testing"
)

// uuidShape mirrors growthClientToken's 8-4-4-4-12 hex output without pinning
// any particular random value.
var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var conversationHeaderNames = []string{
	"X-Conversation-ID",
	"X-Conversation-Request-ID",
	"X-Conversation-Message-ID",
}

// [self 0.9.63] Upstream 0.9.61 (issues #43-#45): the Tencent channel gate
// rejects otherwise-valid chat calls with 11128 unless the desktop client's
// three conversation-correlation headers are present. The fill is
// realm-unconditional and blank-only.
func TestBackendHeaders_ConversationHeadersFillEveryRealm(t *testing.T) {
	realms := []struct {
		name string
		sa   *storedAuth
	}{
		{"cn", &storedAuth{Auth: storedTokens{AccessToken: "at", Domain: "www.codebuddy.cn"}}},
		{"cn empty legacy domain", &storedAuth{Auth: storedTokens{AccessToken: "at"}}},
		{"global", &storedAuth{Auth: storedTokens{AccessToken: "at", Domain: "www.workbuddy.ai"}}},
		{"intl", &storedAuth{Auth: storedTokens{AccessToken: "at", Domain: "www.codebuddy.ai"}}},
	}
	for _, realm := range realms {
		t.Run(realm.name, func(t *testing.T) {
			req := &http.Request{Header: http.Header{}}
			backendHeaders(req, realm.sa)
			for _, h := range conversationHeaderNames {
				got := req.Header.Get(h)
				if got == "" {
					t.Fatalf("%s missing on %s realm", h, realm.name)
				}
				if !uuidShape.MatchString(got) {
					t.Fatalf("%s = %q, want UUID shape", h, got)
				}
			}
		})
	}
}

// A caller-supplied id must survive verbatim: task_chat sets
// X-Conversation-ID to its wb-task-* value and the fill must not rotate it.
func TestBackendHeaders_ConversationHeadersPreserveCallerValues(t *testing.T) {
	req := &http.Request{Header: http.Header{}}
	req.Header.Set("X-Conversation-ID", "wb-task-abc123")
	req.Header.Set("X-Conversation-Request-ID", "caller-request")
	backendHeaders(req, &storedAuth{Auth: storedTokens{AccessToken: "at", Domain: "www.codebuddy.cn"}})

	if got := req.Header.Get("X-Conversation-ID"); got != "wb-task-abc123" {
		t.Fatalf("X-Conversation-ID = %q, caller value overwritten", got)
	}
	if got := req.Header.Get("X-Conversation-Request-ID"); got != "caller-request" {
		t.Fatalf("X-Conversation-Request-ID = %q, caller value overwritten", got)
	}
	// The one blank sibling is still filled around the preserved values.
	if got := req.Header.Get("X-Conversation-Message-ID"); !uuidShape.MatchString(got) {
		t.Fatalf("X-Conversation-Message-ID = %q, want UUID shape", got)
	}
}

// Every request must get fresh ids: a replayed id across turns is not what the
// official client sends, and the gate validates presence/shape only.
func TestBackendHeaders_ConversationHeadersArePerRequest(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{AccessToken: "at", Domain: "www.codebuddy.cn"}}
	first := &http.Request{Header: http.Header{}}
	backendHeaders(first, sa)
	second := &http.Request{Header: http.Header{}}
	backendHeaders(second, sa)

	for _, h := range conversationHeaderNames {
		if first.Header.Get(h) == second.Header.Get(h) {
			t.Fatalf("%s reused across requests (%q)", h, first.Header.Get(h))
		}
	}
}

// The fill must not disturb the pre-existing identity headers that the
// platform/realm overrides already set.
func TestBackendHeaders_ConversationHeadersLeaveIdentityIntact(t *testing.T) {
	// IDE platform on the CN realm: the platform override wins, the fill must
	// not touch it.
	ide := &storedAuth{Auth: storedTokens{AccessToken: "at", LoginPlatform: "ide", Domain: "www.codebuddy.cn"}}
	ideReq := &http.Request{Header: http.Header{}}
	backendHeaders(ideReq, ide)
	if got := ideReq.Header.Get("X-IDE-Name"); got != "CodeBuddyIDE" {
		t.Fatalf("X-IDE-Name = %q, want CodeBuddyIDE (platform override clobbered)", got)
	}

	// Intl realm: the realm override is CodeBuddy, which must survive too.
	intl := &storedAuth{Auth: storedTokens{AccessToken: "at", Domain: "www.codebuddy.ai"}}
	intlReq := &http.Request{Header: http.Header{}}
	backendHeaders(intlReq, intl)
	if got := intlReq.Header.Get("X-IDE-Name"); got != "CodeBuddy" {
		t.Fatalf("X-IDE-Name = %q, want CodeBuddy (realm override clobbered)", got)
	}
	if got := intlReq.Header.Get("X-Product"); got != "SaaS" {
		t.Fatalf("X-Product = %q, want SaaS", got)
	}
}
