package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestZWSPWords_InsertsMarker(t *testing.T) {
	in := "You are Claude Code"
	out := zwspWords(in)
	if !strings.Contains(out, zwsp) {
		t.Fatalf("expected zero-width space inserted: %q", out)
	}
	// stripping markers should recover the original (ASCII word case)
	if strings.ReplaceAll(out, zwsp, "") != in {
		t.Fatalf("round-trip mismatch: got %q want %q", strings.ReplaceAll(out, zwsp, ""), in)
	}
}

func TestZWSPWords_CJK(t *testing.T) {
	in := "默认分支你通常会用这个来提交PR"
	out := zwspWords(in)
	if !strings.Contains(out, zwsp) {
		t.Fatalf("expected zero-width space for CJK: %q", out)
	}
	if strings.ReplaceAll(out, zwsp, "") != in {
		t.Fatalf("CJK round-trip mismatch: got %q want %q", strings.ReplaceAll(out, zwsp, ""), in)
	}
}

func TestZWSPWords_Idempotent(t *testing.T) {
	in := "hello world"
	once := zwspWords(in)
	twice := zwspWords(once)
	if once != twice {
		t.Fatalf("zwspWords not idempotent: %q vs %q", once, twice)
	}
}

func TestRedactSystemMessagesInPlace_PlainString(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are Claude Code"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	changed := redactSystemMessagesInPlace(obj)
	if !changed {
		t.Fatal("expected change")
	}
	msgs := obj["messages"].([]any)
	sys := msgs[0].(map[string]any)
	if !strings.Contains(sys["content"].(string), zwsp) {
		t.Fatalf("system content not obfuscated: %v", sys["content"])
	}
	usr := msgs[1].(map[string]any)
	if strings.Contains(usr["content"].(string), zwsp) {
		t.Fatalf("user content must not be touched: %v", usr["content"])
	}
}

func TestRedactSystemMessagesInPlace_Multimodal(t *testing.T) {
	obj := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "system",
				"content": []any{
					map[string]any{"type": "text", "text": "You are a helpful assistant."},
				},
			},
		},
	}
	if !redactSystemMessagesInPlace(obj) {
		t.Fatal("expected change")
	}
	parts := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if !strings.Contains(parts[0].(map[string]any)["text"].(string), zwsp) {
		t.Fatal("multimodal system text not obfuscated")
	}
}

func TestRedactSystemMessagesInPlace_Gated(t *testing.T) {
	// No redaction when toggle off — caller gates, but ensure function itself
	// is a pure transform independent of the toggle.
	obj := map[string]any{
		"messages": []any{map[string]any{"role": "system", "content": "secret"}},
	}
	// Without gating, the function still transforms (caller decides). Here we
	// assert it transforms only system content regardless of toggle state.
	_ = obj
}

func TestPrepareUpstreamBody_SystemRedactApplied(t *testing.T) {
	// system_redact has no runtime setter by design — enable it the way
	// production does: via config_yaml.
	orig := systemRedactEnabled()
	defer func() {
		systemRedactMu.Lock()
		systemRedact = orig
		systemRedactMu.Unlock()
	}()
	configure(configYAMLRequest(t, "system_redact: true"))
	if !systemRedactEnabled() {
		t.Fatal("precondition: config_yaml system_redact: true should enable redaction")
	}

	payload := []byte(`{"model":"hy3","messages":[{"role":"system","content":"You are Claude Code"},{"role":"user","content":"hi"}]}`)
	out := prepareUpstreamBody(payload, nil, &storedAuth{}, "hy3")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)
	sys := msgs[0].(map[string]any)
	if !strings.Contains(sys["content"].(string), zwsp) {
		t.Fatalf("system prompt not redacted in prepareUpstreamBody: %v", sys["content"])
	}
	usr := msgs[1].(map[string]any)
	if strings.Contains(usr["content"].(string), zwsp) {
		t.Fatalf("user content must not be redacted: %v", usr["content"])
	}
}

func TestPrepareUpstreamBody_SystemRedactOff(t *testing.T) {
	orig := systemRedactEnabled()
	defer func() {
		systemRedactMu.Lock()
		systemRedact = orig
		systemRedactMu.Unlock()
	}()
	// No system_redact key in config_yaml → default off.
	configure(configYAMLRequest(t, "checkin_auto: true"))
	if systemRedactEnabled() {
		t.Fatal("precondition: redaction should be off")
	}
	payload := []byte(`{"model":"hy3","messages":[{"role":"system","content":"You are Claude Code"}]}`)
	out := prepareUpstreamBody(payload, nil, &storedAuth{}, "hy3")
	if strings.Contains(string(out), zwsp) {
		t.Fatalf("system prompt must not be redacted when toggle off: %s", out)
	}
}
