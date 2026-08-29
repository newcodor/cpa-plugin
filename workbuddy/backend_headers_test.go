package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

func TestBackendHeadersAddsClientIdentity(t *testing.T) {
	makeHeaders := func() http.Header {
		req := httptest.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
		backendHeaders(req, &storedAuth{})
		return req.Header
	}

	first := makeHeaders()
	for name, want := range map[string]string{
		"X-IDE-Type":     "CLI",
		"X-IDE-Name":     "CLI",
		"X-IDE-Version":  "2.63.2",
		"X-Agent-Intent": "craft",
	} {
		if got := first.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	idPattern := regexp.MustCompile(`^[0-9a-f]{32}$`)
	idHeaders := []string{
		"X-Request-ID",
		"X-Conversation-ID",
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
	}
	seen := make(map[string]string, len(idHeaders))
	for _, name := range idHeaders {
		value := first.Get(name)
		if !idPattern.MatchString(value) {
			t.Errorf("%s = %q, want 32 lowercase hex characters", name, value)
		}
		if previous, exists := seen[value]; exists {
			t.Errorf("%s duplicates %s with value %q", name, previous, value)
		}
		seen[value] = name
	}

	second := makeHeaders()
	for _, name := range idHeaders {
		if first.Get(name) == second.Get(name) {
			t.Errorf("%s was reused across requests: %q", name, first.Get(name))
		}
	}
}

// The identity headers must agree with the User-Agent sent on the same request,
// otherwise upstream sees a CLI that claims two different versions.
func TestBackendHeadersIdentityMatchesUserAgent(t *testing.T) {
	req := httptest.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
	backendHeaders(req, &storedAuth{})

	if got, want := req.Header.Get("X-IDE-Type"), "CLI"; got != want {
		t.Errorf("X-IDE-Type = %q, want %q", got, want)
	}
	// clientUA is "CLI/2.63.2 CodeBuddy/2.63.2"; X-IDE-Version must track it.
	wantVersion := "2.63.2"
	if got := req.Header.Get("X-IDE-Version"); got != wantVersion {
		t.Errorf("X-IDE-Version = %q, want %q (clientUA = %q)", got, wantVersion, clientUA)
	}
}

// randomHex must always return exactly 2n hex characters, including on the
// fallback path. Upstream v0.8.6 formatted a timestamp there and produced a
// 15-char value, silently breaking the 32-char ID contract.
func TestRandomHexLength(t *testing.T) {
	idPattern := regexp.MustCompile(`^[0-9a-f]+$`)
	for _, n := range []int{1, 8, 16, 32} {
		got := randomHex(n)
		if len(got) != n*2 {
			t.Errorf("randomHex(%d) length = %d, want %d (value %q)", n, len(got), n*2, got)
		}
		if !idPattern.MatchString(got) {
			t.Errorf("randomHex(%d) = %q, want lowercase hex only", n, got)
		}
	}
}

// The fallback is exercised directly: crypto/rand never fails on Linux, so the
// only way to keep the fallback honest is to test it as its own function.
func TestFallbackHexLength(t *testing.T) {
	idPattern := regexp.MustCompile(`^[0-9a-f]+$`)
	for _, n := range []int{1, 8, 16, 32} {
		got := fallbackHex(n)
		if len(got) != n*2 {
			t.Errorf("fallbackHex(%d) length = %d, want %d (value %q)", n, len(got), n*2, got)
		}
		if !idPattern.MatchString(got) {
			t.Errorf("fallbackHex(%d) = %q, want lowercase hex only", n, got)
		}
	}
}

// If crypto/rand ever fails, all four ID headers collapse onto the fallback.
// They must still differ, or upstream sees one conversation ID repeated four
// times in the same request.
func TestFallbackHexVariesWithinRequest(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 4; i++ {
		value := fallbackHex(16)
		if seen[value] {
			t.Errorf("fallbackHex(16) repeated within one request: %q", value)
		}
		seen[value] = true
	}
}
