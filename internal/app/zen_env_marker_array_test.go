package app

import (
	"strings"
	"testing"
)

// 2026-10-27 回归：marker 检测必须看得懂 parts 数组形态。
// IDE（Cursor/ZCode）常把 system content 发成 [{"type":"text","text":...}]。
// 只认 string 时，已带 marker 的数组 system 被判为"没有"，网关又 prepend
// 一个 system 消息 —— 产生两条相邻 system，严格校验器会拒绝。
func TestEnvMarkerDetectsArraySystemContent(t *testing.T) {
	params := map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "<env>\n  Working directory: /x\n</env>"},
			}},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	body := buildZenBody(params, true)
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (no duplicate system): got %v", len(msgs), rolesOf(msgs))
	}
	if n := countSystem(msgs); n != 1 {
		t.Fatalf("system messages = %d, want 1 (%v)", n, rolesOf(msgs))
	}
}

// 数组形态且缺 marker：追加一个 text part，而不是新增第二条 system。
func TestEnvMarkerAppendsToArraySystemContent(t *testing.T) {
	params := map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "You are helpful."},
			}},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	body := buildZenBody(params, true)
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (%v)", len(msgs), rolesOf(msgs))
	}
	if n := countSystem(msgs); n != 1 {
		t.Fatalf("system messages = %d, want 1 (%v)", n, rolesOf(msgs))
	}
	sys := msgs[0].(map[string]any)
	parts, ok := sys["content"].([]any)
	if !ok {
		t.Fatalf("array content lost: %T", sys["content"])
	}
	if !strings.Contains(zenContentText(parts), "You are helpful.") {
		t.Error("client text dropped")
	}
	if !zenTextHasEnvMarker(zenContentText(parts)) {
		t.Error("env marker not appended")
	}
}

// 2026-10-27 回归：marker 注入不得就地改写调用方的 messages。
// body["messages"] 与 params["messages"] 同底层切片，就地改 map 会把
// marker 写进客户端参数，端点自适应重试/日志/token 估算随后读到污染版本。
func TestEnvMarkerDoesNotMutateClientParams(t *testing.T) {
	orig := "You are helpful."
	params := map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": orig},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	body := buildZenBody(params, true)

	// 上游 body 应带 marker
	upSys := body["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !zenTextHasEnvMarker(upSys) {
		t.Fatal("upstream body lacks env marker")
	}
	// 客户端参数必须原样
	clSys := params["messages"].([]any)[0].(map[string]any)["content"].(string)
	if clSys != orig {
		t.Fatalf("client params mutated:\n got %q\nwant %q", clSys, orig)
	}
}

// 数组形态下同样不能污染客户端（含 parts 切片的共享）。
func TestEnvMarkerDoesNotMutateClientArrayParams(t *testing.T) {
	params := map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "keep me"},
			}},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	_ = buildZenBody(params, true)

	parts := params["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("client parts grew to %d: marker leaked into caller's slice", len(parts))
	}
	if txt, _ := parts[0].(map[string]any)["text"].(string); txt != "keep me" {
		t.Fatalf("client part text = %q, want unchanged", txt)
	}
}

// responses 路径：数组形态 system 内容必须并入 instructions，不能整段丢弃。
func TestResponsesKeepsArraySystemContent(t *testing.T) {
	params := map[string]any{
		"model": "big-pickle",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "CRITICAL: answer in French."},
			}},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	body := buildZenResponsesBody(params, "big-pickle", "ses_0123456789abAAAAAAAAAAAAAA")
	inst, _ := body["instructions"].(string)
	if !strings.Contains(inst, "answer in French") {
		t.Fatalf("client system prompt dropped: instructions = %q", inst)
	}
	if !zenTextHasEnvMarker(inst) {
		t.Fatalf("env marker missing from instructions: %q", inst)
	}
}

// 无工具客户端：gate 工具是过闸占位，客户端没有执行层。
// 必须 tool_choice=none，否则模型可返回客户端从未声明的调用。
func TestToolLessClientGetsNoGateCalls(t *testing.T) {
	body := buildZenBody(map[string]any{
		"model":    "big-pickle",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, true)
	if len(body["tools"].([]any)) != len(zenGateToolSpecs) {
		t.Fatalf("gate tools = %d, want %d", len(body["tools"].([]any)), len(zenGateToolSpecs))
	}
	if body["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %v, want none (gate stubs must not be callable)", body["tool_choice"])
	}
}

// 无工具客户端即使显式要 required/auto，也必须被压回 none：
// required 会强制模型发起一次客户端无法执行的工具调用。
func TestToolLessClientForcedNoneRegardlessOfClientChoice(t *testing.T) {
	for _, tc := range []any{"auto", "required"} {
		body := buildZenBody(map[string]any{
			"model":       "big-pickle",
			"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
			"tool_choice": tc,
		}, true)
		if body["tool_choice"] != "none" {
			t.Fatalf("client tool_choice=%v with no tools -> %v, want none", tc, body["tool_choice"])
		}
	}
}

func rolesOf(msgs []any) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok {
			r, _ := mm["role"].(string)
			out = append(out, r)
		}
	}
	return out
}

func countSystem(msgs []any) int {
	n := 0
	for _, r := range rolesOf(msgs) {
		if r == "system" || r == "developer" {
			n++
		}
	}
	return n
}
