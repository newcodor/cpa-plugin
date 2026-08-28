// redact.go strips credentials and token-shaped material from any string that
// might end up in logs, error responses, or CPAMP usage bodies. All plugin
// code that surfaces upstream error text must route through redactSecrets
// (or truncateRedacted when a length cap is also needed).
//
// It also owns system-prompt obfuscation (redactSystemMessagesInPlace): when
// the plugin-level "system_redact" toggle is on, the content of every
// role=system message gets zero-width spaces (U+200B) sprinkled in so the
// upstream content filter / exact-match scanner can no longer recognize the
// original phrasing — while the model still reads the prompt normally.
package main

import (
	"regexp"
	"strings"
)

var (
	redactREBearer  = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._\-+/=]{12,}`)
	redactREJWT     = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`)
	redactRETokenKV = regexp.MustCompile(`(?i)((?:access_token|refresh_token|id_token)\s*[=:]\s*)([A-Za-z0-9._\-+/=]{12,})`)
	// redactREJWTLoose catches JWTs that appear bare in a JSON value or path —
	// no Bearer prefix, no access_token key. Two-segment and three-segment both
	// match (some upstreams return header.payload only when signature is empty).
	redactREJWTLoose = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}(?:\.[A-Za-z0-9_\-]{4,}){1,2}\b`)
)

// redactSecrets strips bearer tokens / JWT-like blobs from error bodies before usage publish.
func redactSecrets(s string) string {
	if s == "" {
		return s
	}
	// Bearer tokens
	s = redactREBearer.ReplaceAllString(s, "Bearer ***")
	// long JWT-ish segments
	s = redactREJWT.ReplaceAllString(s, "***jwt***")
	// access_token / refresh_token query-or-json fragments (best-effort)
	s = redactRETokenKV.ReplaceAllString(s, "${1}***")
	// loose JWT fallback: bare header.payload(.sig) without Bearer / kv context
	s = redactREJWTLoose.ReplaceAllString(s, "***jwt***")
	return s
}

// truncateRedacted redacts secrets then truncates — use for any error body
// returned to clients / logs (A-37). publishUsage already redacts Fail.Body.
func truncateRedacted(s string, n int) string {
	return truncate(redactSecrets(s), n)
}

// truncate cuts s to at most n bytes. Caller is responsible for rune-boundary
// safety when the string may contain multi-byte UTF-8 (most callers pass
// upstream JSON which is ASCII-safe at token boundaries).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// -----------------------------------------------------------------------------
// System-prompt obfuscation (system_redact toggle)
// -----------------------------------------------------------------------------
//
// Inserting U+200B (zero-width space) at word boundaries keeps the text fully
// readable by the LLM tokenizer but breaks exact-match substring scanning on
// the upstream side (content filter / policy matcher), which is what the
// "system prompt redaction" feature is meant to defeat. We inject after each
// run of word characters so both ASCII and CJK (no spaces) get split up.
//
// Design notes:
//   - Only role=system content is touched (user/assistant are model I/O, not
//     "system prompt"; masking them would corrupt the actual task).
//   - Plain-string and OpenAI multimodal (array-of-parts) shapes both handled,
//     mirroring rewriteContentField.
//   - Idempotent-ish: we strip existing U+200B first so repeated passes don't
//     keep lengthening the string.

const zwsp = "​"

// zwspWords inserts a zero-width space after every run of word characters.
// Pre-existing zero-width spaces are removed first to keep re-passes stable.
func zwspWords(s string) string {
	if s == "" {
		return s
	}
	// Avoid stacking markers across repeated rewrites.
	s = strings.ReplaceAll(s, zwsp, "")
	if !strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") &&
		!strings.ContainsAny(s, "一-鿿") {
		// No word/CJK chars to split — leave untouched (e.g. emoji-only / symbols).
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	inWord := false
	for _, r := range s {
		b.WriteRune(r)
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			inWord = true
		case r >= '一' && r <= '鿿':
			// CJK: split after each character (no word boundaries in CJK).
			b.WriteString(zwsp)
			inWord = false
		default:
			if inWord {
				b.WriteString(zwsp)
			}
			inWord = false
		}
	}
	return b.String()
}

// redactSystemMessagesInPlace injects zero-width spaces into the content of
// every role=system message in obj["messages"]. Returns true if anything
// changed. Safe to call unconditionally (the caller gates on the toggle).
func redactSystemMessagesInPlace(obj map[string]any) bool {
	messages, _ := obj["messages"].([]any)
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "system") {
			continue
		}
		switch c := msg["content"].(type) {
		case string:
			if r := zwspWords(c); r != c {
				msg["content"] = r
				changed = true
			}
		case []any:
			for _, p := range c {
				part, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if t, ok := part["text"].(string); ok {
					if r := zwspWords(t); r != t {
						part["text"] = r
						changed = true
					}
				}
			}
		}
	}
	return changed
}
