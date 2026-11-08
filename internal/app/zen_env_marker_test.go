package app

import (
	"strings"
	"testing"
)

// Free-tier env gate, 2026-10-26 ablation: a system message containing
// "Working directory:" inside <env>...</env> is required for upstream
// to accept a zen chat call; without it, the same IP/account/TLS/
// headers/tools get 429. The value itself is not validated.
func TestEnsureZenEnvMarkerChat(t *testing.T) {
	// No system message at all -> a system message with the marker is prepended.
	body := buildZenBody(map[string]any{
		"model":    "big-pickle",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, true)
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages in body")
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("first message role = %q, want system", first["role"])
	}
	content, _ := first["content"].(string)
	if !zenTextHasEnvMarker(content) {
		t.Fatalf("prepended system message lacks env marker: %q", content)
	}
	// Client system message without marker -> marker appended, client text kept.
	body = buildZenBody(map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": "hi"},
		},
	}, true)
	msgs, _ = body["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	s, _ := sys["content"].(string)
	if !strings.HasPrefix(s, "You are helpful.") || !zenTextHasEnvMarker(s) {
		t.Fatalf("system message not preserved+marked: %q", s)
	}
	// Client system message already with marker -> untouched, no duplication.
	body = buildZenBody(map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": "<env>\n  Working directory: /tmp/x\n</env>"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}, true)
	msgs, _ = body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (no duplicate system message)", len(msgs))
	}
}
