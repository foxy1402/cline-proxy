package app

import (
	"fmt"
	"strings"
	"testing"
)

func TestZenMergeToolsClientWinsOverGateStub(t *testing.T) {
	client := []any{map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "read",
			"description": "Read a file from disk",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"file_path": map[string]any{"type": "string"}},
			},
		},
	}}
	merged := zenMergeTools(client, zenGateTools())
	readCount := 0
	var readSchema map[string]any
	for _, mt := range merged {
		tm, _ := mt.(map[string]any)
		fn, _ := tm["function"].(map[string]any)
		if n, _ := fn["name"].(string); n == "read" {
			readCount++
			fnParams, _ := fn["parameters"].(map[string]any)
			readSchema, _ = fnParams["properties"].(map[string]any)
		}
	}
	if readCount != 1 {
		t.Fatalf("read appears %d times, want 1 (gate stub must not duplicate client tool)", readCount)
	}
	if _, ok := readSchema["file_path"]; !ok {
		t.Fatalf("client read schema lost: %v (gate stub won, model would see empty params)", readSchema)
	}
	if len(merged) != len(zenGateToolSpecs) {
		t.Fatalf("merged tool count = %d, want %d (gate names still all present)",
			len(merged), len(zenGateToolSpecs))
	}
}

func TestZenClientFlatToolsShapes(t *testing.T) {
	// chat 嵌套形态 + 已是 flat 形态 + parameters 缺失，三者都要转成合法 flat
	params := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{
			"name":        "get_weather",
			"description": "weather",
			"parameters":  map[string]any{"type": "object"},
		}},
		map[string]any{"type": "function", "name": "no_schema"},
	}}
	flat := zenClientFlatTools(params)
	if len(flat) != 2 {
		t.Fatalf("got %d flat tools, want 2", len(flat))
	}
	if n, _ := flat[0]["name"].(string); n != "get_weather" {
		t.Errorf("flat[0].name = %q, want get_weather", n)
	}
	if flat[0]["parameters"] == nil {
		t.Error("flat[0].parameters is nil")
	}
	if _, ok := flat[1]["parameters"].(map[string]any); !ok {
		t.Errorf("missing schema must default to an object schema, got %v", flat[1]["parameters"])
	}
}

func TestBuildZenBodyToolChoicePolicy(t *testing.T) {
	// 无客户端工具：gate 工具 + tool_choice=none。
	// gate 工具只是过闸占位，客户端没有执行层；省略 tool_choice 会让上游
	// 按 auto 处理，模型可返回客户端从未声明的 read/shell 调用。
	body := buildZenBody(map[string]any{"model": "ling-2.6-flash", "messages": []any{}}, false)
	tools, _ := body["tools"].([]any)
	if len(tools) != len(zenGateToolSpecs) {
		t.Fatalf("gate tools = %d, want %d", len(tools), len(zenGateToolSpecs))
	}
	if body["tool_choice"] != "none" {
		t.Errorf("tool_choice = %v, want none when client sent no tools (gate stubs are not callable)", body["tool_choice"])
	}
	// 客户端带工具但未指定：auto，让模型按需调用客户端真实工具
	body = buildZenBody(map[string]any{
		"model":    "ling-2.6-flash",
		"messages": []any{},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "get_weather", "parameters": map[string]any{"type": "object"},
		}}},
	}, false)
	if body["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto when client sent tools but no preference", body["tool_choice"])
	}
	tools, _ = body["tools"].([]any)
	if len(tools) != len(zenGateToolSpecs)+1 {
		t.Fatalf("merged tools = %d, want %d", len(tools), len(zenGateToolSpecs)+1)
	}
	// 客户端显式 none：尊重（chat 端点接受 none）
	body = buildZenBody(map[string]any{
		"model":       "ling-2.6-flash",
		"messages":    []any{},
		"tool_choice": "none",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "get_weather", "parameters": map[string]any{"type": "object"},
		}}},
	}, false)
	if body["tool_choice"] != "none" {
		t.Errorf("tool_choice = %v, want none (client override honored)", body["tool_choice"])
	}
}

func TestBuildZenBodyLegacyFunctions(t *testing.T) {
	body := buildZenBody(map[string]any{
		"model":    "ling-2.6-flash",
		"messages": []any{},
		"functions": []any{map[string]any{
			"name": "legacy_fn", "description": "old style", "parameters": map[string]any{"type": "object"},
		}},
		"function_call": "auto",
	}, false)
	if _, ok := body["functions"]; ok {
		t.Error("legacy functions must not be forwarded verbatim")
	}
	if _, ok := body["function_call"]; ok {
		t.Error("legacy function_call must not be forwarded verbatim")
	}
	found := false
	for _, mt := range body["tools"].([]any) {
		fn, _ := mt.(map[string]any)["function"].(map[string]any)
		if n, _ := fn["name"].(string); n == "legacy_fn" {
			found = true
		}
	}
	if !found {
		t.Error("legacy functions[] not converted into tools[]")
	}
	if body["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto for converted legacy functions (client declared them)", body["tool_choice"])
	}
}

func TestMergeConcatenatedJSONJam(t *testing.T) {
	// spark 并行调用产出过 {"query":...}{"url":...} 这种粘连串
	v, err := parseToolArgs(`{"query":"zcode"}{"url":"https://example.com"}`)
	if err != nil {
		t.Fatalf("parseToolArgs jammed input failed: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("merged value = %T, want map", v)
	}
	if m["query"] != "zcode" || m["url"] != "https://example.com" {
		t.Fatalf("merged = %v, want both keys", m)
	}
	if got := repairToolArguments(`{"query":"zcode"}{"url":"https://example.com"}`); got != `{"query":"zcode","url":"https://example.com"}` {
		t.Fatalf("repairToolArguments = %q", got)
	}
}

func TestChatToolArgumentsEmpty(t *testing.T) {
	if got := chatToolArguments(""); got != "{}" {
		t.Errorf("chatToolArguments(\"\") = %q, want {}", got)
	}
	if got := chatToolArguments(`{"a":1}`); got != `{"a":1}` {
		t.Errorf("chatToolArguments valid = %q, want unchanged", got)
	}
}

// 上游工具片段 index 从 1 起跳（无 0 号桶）时，调用不能丢。
func TestCollectStreamResponseIndexGap(t *testing.T) {
	body := "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_x","type":"function","function":{"name":"read","arguments":"{\"file_path\":\"a\"}"}}]},"finish_reason":null}]}` + "\n\n"
	body += "data: " + `{"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	body += "data: [DONE]\n\n"
	chat, err := collectStreamResponse(sseResp(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := toolCallArgs(t, chat)
	if len(got) != 1 {
		t.Fatalf("tool_calls = %d, want 1 (index gap must not drop calls): %v", len(got), got)
	}
	fr, _ := chat["choices"].([]any)[0].(map[string]any)["finish_reason"].(string)
	if fr != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls (upstream said stop)", fr)
	}
}

// 上游首片 id 为空、后续分片才带 id：id 要补齐。
func TestCollectStreamResponseLateID(t *testing.T) {
	body := "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"{\"a\""}}]}}]}` + "\n\n"
	body += "data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_late","function":{"arguments":":1}"}}]}}]}` + "\n\n"
	body += "data: [DONE]\n\n"
	chat, err := collectStreamResponse(sseResp(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	call := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if id, _ := call["id"].(string); id != "call_late" {
		t.Errorf("id = %q, want call_late", id)
	}
}

// 上游在首个 data 块前发心跳注释（mimo 实测 ": keep-alive"）时不能误判成
// 非 SSE 错误体。
func TestCollectStreamResponseKeepAliveComment(t *testing.T) {
	body := ": keep-alive\n\n: keep-alive\n\n"
	body += "data: " + `{"choices":[{"delta":{"content":"PO"}}]}` + "\n\n"
	body += "data: " + `{"choices":[{"delta":{"content":"NG"},"finish_reason":"stop"}]}` + "\n\n"
	body += "data: [DONE]\n\n"
	chat, err := collectStreamResponse(sseResp(body))
	if err != nil {
		t.Fatalf("keep-alive comment must not break parsing: %v", err)
	}
	c, _ := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	if c != "PONG" {
		t.Fatalf("content = %q, want PONG", c)
	}
}

// 仍要能识别真正的 JSON 错误体（不能因为放宽判据而把错误当成功）。
func TestCollectStreamResponseJSONErrorStillDetected(t *testing.T) {
	_, err := collectStreamResponse(sseResp(`{"error":{"message":"busy","type":"server_error"}}`))
	if err == nil {
		t.Fatal("expected error for non-SSE error body")
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Errorf("error = %v, want it to carry the upstream message", err)
	}
}

// 截断把"调用 → 结果"拆开时，两个方向都要剔除，否则上游 400。
func TestFallbackTruncateToolPairing(t *testing.T) {
	big := strings.Repeat("x", 4000)
	msgs := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
			map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "res1"},
		// call_2 的结果超大：预算装不下，会被截断丢弃
		map[string]any{"role": "tool", "tool_call_id": "call_2", "content": big},
	}
	params := map[string]any{"messages": msgs}
	out := fallbackTruncate(params, &ZenModel{Context: 2000})
	if !out.changed {
		t.Fatal("expected truncation to happen")
	}
	kept, _ := params["messages"].([]any)
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	for _, m := range kept {
		mm, _ := m.(map[string]any)
		switch strField(mm, "role") {
		case "assistant":
			for _, id := range toolCallIDs(mm) {
				callIDs[id] = true
			}
		case "tool":
			resultIDs[strField(mm, "tool_call_id")] = true
		}
	}
	for id := range callIDs {
		if !resultIDs[id] {
			t.Errorf("assistant tool_calls %s kept without its tool result (upstream 400)", id)
		}
	}
	for id := range resultIDs {
		if !callIDs[id] {
			t.Errorf("tool result %s kept without its assistant call (upstream 400)", id)
		}
	}
	_ = fmt.Sprint()
}
