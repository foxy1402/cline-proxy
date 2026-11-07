package app

import (
	"strings"
	"testing"
)

// setZenSystemRelocation 临时开关系统提示词换位，测试结束恢复原值。
func setZenSystemRelocation(t *testing.T, on bool) {
	t.Helper()
	cfg := getZenConfig()
	old := cfg.SystemRelocation
	cfg.SystemRelocation = boolPtr(on)
	t.Cleanup(func() { cfg.SystemRelocation = old })
}

// 默认开启：system 原文 → 首条 user 消息头部（<system> 包裹），
// system role 换成固定短提示词 + <env> 闸块，客户端 params 不被污染。
func TestZenSystemRelocationChatStringContent(t *testing.T) {
	setZenSystemRelocation(t, true)
	params := map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a strict agentic coding assistant. Rule 42."},
			map[string]any{"role": "user", "content": "hello from zcode"},
		},
	}
	body := buildZenBody(params, true)
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	s, _ := sys["content"].(string)
	if !strings.HasPrefix(s, zenFixedSystemPrompt) {
		t.Fatalf("system role must carry the fixed prompt, got %q", s)
	}
	if !zenTextHasEnvMarker(s) {
		t.Fatalf("relocated system message must keep the env gate marker: %q", s)
	}
	if strings.Contains(s, "Rule 42") {
		t.Fatalf("original system text must leave the system role: %q", s)
	}
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("second message role = %v, want user", user["role"])
	}
	u, _ := user["content"].(string)
	wantWrap := "<system>\nYou are a strict agentic coding assistant. Rule 42.\n</system>\n\n"
	if !strings.HasPrefix(u, wantWrap) {
		t.Fatalf("first user message must start with the wrapped original, got %q", u)
	}
	if !strings.HasSuffix(u, "hello from zcode") {
		t.Fatalf("client user text must be preserved after the wrap: %q", u)
	}
	// params 不被污染：端点自适应重试、日志、token 估算读到的仍是原形态。
	pm := params["messages"].([]any)
	ps, _ := pm[0].(map[string]any)
	if ps["content"] != "You are a strict agentic coding assistant. Rule 42." {
		t.Fatalf("params system message mutated: %v", ps["content"])
	}
	pu, _ := pm[1].(map[string]any)
	if pu["content"] != "hello from zcode" {
		t.Fatalf("params user message mutated: %v", pu["content"])
	}
}

// IDE 常用 parts 数组形态：system 文本从 parts 提取，user 的 parts 前插 text part。
func TestZenSystemRelocationChatPartsArray(t *testing.T) {
	setZenSystemRelocation(t, true)
	body := buildZenBody(map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "part one"},
				map[string]any{"type": "text", "text": "part two"},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "do the thing"},
			}},
		},
	}, true)
	msgs, _ := body["messages"].([]any)
	user, _ := msgs[1].(map[string]any)
	parts, ok := user["content"].([]any)
	if !ok {
		t.Fatalf("user content must stay a parts array, got %T", user["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("user parts = %d, want 2 (wrap + original)", len(parts))
	}
	first, _ := parts[0].(map[string]any)
	ft, _ := first["text"].(string)
	if !strings.HasPrefix(ft, "<system>\npart one\npart two") || !strings.Contains(ft, "</system>") {
		t.Fatalf("wrap part must contain the extracted system text: %q", ft)
	}
	second, _ := parts[1].(map[string]any)
	if st, _ := second["text"].(string); st != "do the thing" {
		t.Fatalf("original user part lost: %v", second)
	}
}

// 多条 system/developer 消息：文本全部并入 wrap，只剩一条固定 system。
func TestZenSystemRelocationMergesSystemAndDeveloper(t *testing.T) {
	setZenSystemRelocation(t, true)
	body := buildZenBody(map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": "SYS TEXT"},
			map[string]any{"role": "developer", "content": "DEV TEXT"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}, true)
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (one system + user)", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] == "SYS TEXT" {
		t.Fatalf("system message not replaced: %v", sys)
	}
	user, _ := msgs[1].(map[string]any)
	u, _ := user["content"].(string)
	if !strings.Contains(u, "SYS TEXT") || !strings.Contains(u, "DEV TEXT") {
		t.Fatalf("wrap must carry both original texts: %q", u)
	}
}

// 没有 user 消息：原文无处安放，保留客户端 system 不动（env 闸块照常追加）。
func TestZenSystemRelocationNoUserMessage(t *testing.T) {
	setZenSystemRelocation(t, true)
	body := buildZenBody(map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": "ONLY SYSTEM"},
			map[string]any{"role": "assistant", "content": "earlier reply"},
		},
	}, true)
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (unchanged)", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	s, _ := sys["content"].(string)
	if !strings.Contains(s, "ONLY SYSTEM") || !zenTextHasEnvMarker(s) {
		t.Fatalf("without a user message the client system must be preserved+marked: %q", s)
	}
}

// 空 system 消息：仍换成固定提示词，但不往 user 前置空 wrap。
func TestZenSystemRelocationEmptySystem(t *testing.T) {
	setZenSystemRelocation(t, true)
	body := buildZenBody(map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": ""},
			map[string]any{"role": "user", "content": "hi"},
		},
	}, true)
	msgs, _ := body["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	s, _ := sys["content"].(string)
	if !strings.HasPrefix(s, zenFixedSystemPrompt) {
		t.Fatalf("empty system must be replaced with the fixed prompt: %q", s)
	}
	user, _ := msgs[1].(map[string]any)
	if u, _ := user["content"].(string); u != "hi" {
		t.Fatalf("user content must stay untouched when the original is empty: %q", u)
	}
}

// 关闭开关：恢复原行为（system 文本保留 + 闸块追加，user 不动）。
func TestZenSystemRelocationDisabled(t *testing.T) {
	setZenSystemRelocation(t, false)
	body := buildZenBody(map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": "KEEP ME"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}, true)
	msgs, _ := body["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)
	s, _ := sys["content"].(string)
	if !strings.HasPrefix(s, "KEEP ME") || !zenTextHasEnvMarker(s) {
		t.Fatalf("disabled: system must be preserved+marked: %q", s)
	}
	user, _ := msgs[1].(map[string]any)
	if u, _ := user["content"].(string); u != "hi" {
		t.Fatalf("disabled: user message must be untouched: %q", u)
	}
}

// 无 system 消息：换位不触发，env 闸块照常 prepend 一条 system。
func TestZenSystemRelocationNoSystem(t *testing.T) {
	setZenSystemRelocation(t, true)
	body := buildZenBody(map[string]any{
		"model":    "big-pickle",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, true)
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (prepended system + user)", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	s, _ := sys["content"].(string)
	if !strings.HasPrefix(s, zenFixedSystemPrompt) {
		// 无客户端 system 时是纯闸块消息（ensureZenEnvMarkerChat 的 prepend 分支）。
		if !zenTextHasEnvMarker(s) {
			t.Fatalf("prepended system must carry the env marker: %q", s)
		}
	}
	user, _ := msgs[1].(map[string]any)
	if u, _ := user["content"].(string); strings.Contains(u, "<system>") {
		t.Fatalf("no original system, no wrap expected in user: %q", u)
	}
}

// 确定性：同一 params 构建两次，body 逐字节一致（上游 prompt cache 依赖字节稳定）。
func TestZenSystemRelocationDeterministic(t *testing.T) {
	setZenSystemRelocation(t, true)
	mk := func() map[string]any {
		return map[string]any{
			"model": "big-pickle",
			"messages": []any{
				map[string]any{"role": "system", "content": "long instructions"},
				map[string]any{"role": "user", "content": "hi"},
			},
		}
	}
	b1 := buildZenBody(mk(), true)
	b2 := buildZenBody(mk(), true)
	m1, _ := b1["messages"].([]any)
	m2, _ := b2["messages"].([]any)
	if len(m1) != len(m2) {
		t.Fatalf("message count differs between builds: %d vs %d", len(m1), len(m2))
	}
	for i := range m1 {
		a, _ := m1[i].(map[string]any)
		b, _ := m2[i].(map[string]any)
		if a["content"] != b["content"] {
			t.Fatalf("message %d content differs: %v vs %v", i, a["content"], b["content"])
		}
	}
}

// responses 路径：instructions 原文 → 首条 user input 头部，instructions 换固定短提示词 + 闸块。
func TestZenSystemRelocationResponses(t *testing.T) {
	setZenSystemRelocation(t, true)
	body := buildZenResponsesBody(map[string]any{
		"model": "deepseek-v4.1-flash-free",
		"messages": []any{
			map[string]any{"role": "system", "content": "RESPONSES SYS TEXT"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}, "deepseek-v4.1-flash-free", "pk")
	ins, _ := body["instructions"].(string)
	if !strings.HasPrefix(ins, zenFixedSystemPrompt) {
		t.Fatalf("instructions must carry the fixed prompt, got %q", ins)
	}
	if !zenTextHasEnvMarker(ins) {
		t.Fatalf("relocated instructions must keep the env gate marker: %q", ins)
	}
	if strings.Contains(ins, "RESPONSES SYS TEXT") {
		t.Fatalf("original instructions must leave the instructions field: %q", ins)
	}
	input, _ := body["input"].([]any)
	var userText string
	for _, it := range input {
		im, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if im["role"] != "user" {
			continue
		}
		switch c := im["content"].(type) {
		case string:
			userText = c
		case []any:
			for _, p := range c {
				pm, _ := p.(map[string]any)
				if s, ok := pm["text"].(string); ok {
					userText += s
				}
			}
		}
	}
	if !strings.Contains(userText, "<system>\nRESPONSES SYS TEXT\n</system>") {
		t.Fatalf("first user input must carry the wrapped original, got %q", userText)
	}
	if !strings.Contains(userText, "hi") {
		t.Fatalf("client user text must be preserved: %q", userText)
	}
}
