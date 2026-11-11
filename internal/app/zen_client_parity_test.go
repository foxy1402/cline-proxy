package app

import (
	"net/http"
	"strings"
	"testing"
)

// 2026-11-11 桌面端 2.2.7 抓包对齐：这些断言锁定"网关发往上游的形态与真实
// opencode 客户端一致"这一契约。抓包原文（r001.json）为 deepseek-v4.1-flash-free：
// reasoning_effort:"high"、无 tool_choice、b3 四段、UA 2.2.7。

// reasoning_effort 必须纯透传：客户端给就转发，不给就不出现。
// 真实客户端的取值来自目录 capabilities.reasoning.effort + 用户的变体选择，
// 网关无从推导，凭空补默认值会让非 reasoning 模型收到非法字段。
func TestBuildZenBodyReasoningEffortPassthrough(t *testing.T) {
	cases := []struct {
		name    string
		params  map[string]any
		want    any
		wantSet bool
	}{
		{
			name:    "snake_case forwarded verbatim",
			params:  map[string]any{"reasoning_effort": "high"},
			want:    "high",
			wantSet: true,
		},
		{
			name:    "camelCase normalized to snake_case",
			params:  map[string]any{"reasoningEffort": "low"},
			want:    "low",
			wantSet: true,
		},
		{
			name:    "absent stays absent (no invented default)",
			params:  map[string]any{},
			wantSet: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]any{"model": "deepseek-v4.1-flash-free", "messages": []any{}}
			for k, v := range tc.params {
				params[k] = v
			}
			body := buildZenBody(params, false)
			got, has := body["reasoning_effort"]
			if has != tc.wantSet {
				t.Fatalf("reasoning_effort present = %v, want %v (body=%v)", has, tc.wantSet, body)
			}
			if tc.wantSet && got != tc.want {
				t.Errorf("reasoning_effort = %v, want %v", got, tc.want)
			}
			// 非标准驼峰名绝不能到达上游。
			if _, has := body["reasoningEffort"]; has {
				t.Error("reasoningEffort must never be forwarded; only reasoning_effort is valid upstream")
			}
		})
	}
}

// tool_choice：客户端有工具但未指定偏好时必须省略（真实客户端如此，上游缺省
// 即 auto）。无客户端工具时仍强制 none —— gate 占位工具不可调用，这是网关
// 有意保留的唯一偏离。
func TestBuildZenBodyToolChoiceOmittedForRealClientShape(t *testing.T) {
	withTools := map[string]any{
		"model":    "deepseek-v4.1-flash-free",
		"messages": []any{},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "get_weather", "parameters": map[string]any{"type": "object"},
		}}},
	}
	if body := buildZenBody(withTools, false); func() bool {
		_, has := body["tool_choice"]
		return has
	}() {
		t.Errorf("tool_choice must be omitted when the client sent tools but no preference; got %v", body["tool_choice"])
	}

	// 无工具 → 仍然 none（安全偏离，见 buildZenBody 注释）。
	noTools := map[string]any{"model": "deepseek-v4.1-flash-free", "messages": []any{}}
	if body := buildZenBody(noTools, false); body["tool_choice"] != "none" {
		t.Errorf("tool_choice = %v, want none when no client tools (gate stubs are not callable)", body["tool_choice"])
	}
}

// b3 必须是四段：traceid-spanid-sampled-parentSpanId（真实客户端带 parentSpanId）。
func TestSetZenGateHeadersB3HasParentSpan(t *testing.T) {
	req, err := http.NewRequest("POST", "https://opencode.ai/inference/openai/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	setZenGateHeaders(req, "ses_aabbccddeeff0123456789ABCD", "msg_x", "")

	b3 := req.Header.Get("b3")
	if b3 == "" {
		t.Fatal("b3 header missing")
	}
	parts := strings.Split(b3, "-")
	if len(parts) != 4 {
		t.Fatalf("b3 = %q has %d segments, want 4 (traceid-spanid-sampled-parentSpanId)", b3, len(parts))
	}
	if len(parts[0]) != 32 {
		t.Errorf("b3 traceid = %q, want 32 hex chars", parts[0])
	}
	if len(parts[1]) != 16 {
		t.Errorf("b3 spanid = %q, want 16 hex chars", parts[1])
	}
	if len(parts[3]) != 16 {
		t.Errorf("b3 parentSpanId = %q, want 16 hex chars", parts[3])
	}
	if parts[2] != "1" {
		t.Errorf("b3 sampled = %q, want 1", parts[2])
	}

	// traceparent 仍只含 trace/span（W3C 格式，与 b3 共享同一 trace/span）。
	tp := req.Header.Get("traceparent")
	if !strings.Contains(tp, parts[0]) || !strings.Contains(tp, parts[1]) {
		t.Errorf("traceparent %q must share trace/span with b3 %q", tp, b3)
	}
}

// UA 必须与抓包来源版本一致（桌面端 2.2.7）。
func TestZenUserAgentMatchesCapturedClient(t *testing.T) {
	if zenNativeUA != "opencode/latest/2.2.7/cli" {
		t.Fatalf("zenNativeUA = %q, want opencode/latest/2.2.7/cli", zenNativeUA)
	}
	req, err := http.NewRequest("POST", "https://opencode.ai/inference/openai/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", zenNativeUA)
	if got := req.Header.Get("User-Agent"); got != "opencode/latest/2.2.7/cli" {
		t.Errorf("User-Agent = %q", got)
	}
}
