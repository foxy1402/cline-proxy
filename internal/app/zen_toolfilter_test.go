package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func setZenToolFilter(t *testing.T, cfg zenToolFilterConfig) {
	t.Helper()
	c := getZenConfig()
	old := c.ToolFilter
	c.ToolFilter = cfg
	t.Cleanup(func() { c.ToolFilter = old })
}

// 默认关闭：客户端工具原样透传（历史行为），这是安全的缺省。
func TestZenToolFilterDisabledByDefault(t *testing.T) {
	if defaultZenToolFilter().Enabled {
		t.Fatal("tool filter must default to disabled: dropping tools removes model capability")
	}
	setZenToolFilter(t, zenToolFilterConfig{Enabled: false, DenyPrefixes: []string{"mcp__"}})
	// 注意用不与 gate stub 重名的客户端工具名：同名时 zenMergeTools 会用客户端
	// 定义替换 gate stub（既有语义），工具总数就不会是 gate+N。
	tools := []any{
		chatTool("mcp__node_repl__js", "huge mcp tool"),
		chatTool("get_weather", "look up the weather"),
	}
	body := buildZenBody(map[string]any{
		"model": "big-pickle", "messages": []any{}, "tools": tools,
	}, false)
	got, _ := body["tools"].([]any)
	if len(got) != len(zenGateToolSpecs)+2 {
		t.Fatalf("disabled: tools = %d, want %d (client tools untouched)", len(got), len(zenGateToolSpecs)+2)
	}
	for _, raw := range got {
		fn, _ := raw.(map[string]any)["function"].(map[string]any)
		if n, _ := fn["name"].(string); n == "mcp__node_repl__js" || n == "get_weather" {
			return
		}
	}
	t.Fatal("client tools vanished although the filter is disabled")
}

// 开启后按前缀丢弃；未命中的客户端工具与全部 gate 工具保留。
func TestZenToolFilterDropsByPrefix(t *testing.T) {
	setZenToolFilter(t, zenToolFilterConfig{Enabled: true, DenyPrefixes: []string{"mcp__"}})
	body := buildZenBody(map[string]any{
		"model": "big-pickle",
		"messages": []any{},
		"tools": []any{
			chatTool("mcp__node_repl__js", "huge"),
			chatTool("mcp__ghidra__decompile", "huge"),
			chatTool("read", "read a file"),
			chatTool("shell", "run a command"),
		},
	}, false)
	names := toolNames(body["tools"])
	for _, n := range names {
		if strings.HasPrefix(n, "mcp__") {
			t.Fatalf("mcp tool survived the filter: %v", names)
		}
	}
	// gate 工具一个都不能少，否则上游 403。
	for _, spec := range zenGateToolSpecs {
		if !containsStr(names, spec.name) {
			t.Fatalf("gate tool %q missing after filtering: %v", spec.name, names)
		}
	}
	// 两个 mcp 工具被丢弃；read/shell 与 gate stub 同名（合并后不增数），
	// 因此断言"存在"即可，不按总数断言。
	if !containsStr(names, "read") || !containsStr(names, "shell") {
		t.Fatalf("core coding tools dropped by a prefix filter: %v", names)
	}
}

// 精确名丢弃（大小写不敏感），用于 cron/workflow/plan-mode 这类重型工具。
func TestZenToolFilterDropsByExactName(t *testing.T) {
	setZenToolFilter(t, zenToolFilterConfig{
		Enabled:   true,
		DenyNames: []string{"CronCreate", "createworkflow", "AskUserQuestion"},
	})
	body := buildZenBody(map[string]any{
		"model": "big-pickle",
		"messages": []any{},
		"tools": []any{
			chatTool("CronCreate", "x"),
			chatTool("CreateWorkflow", "x"),
			chatTool("AskUserQuestion", "x"),
			chatTool("CronList", "x"),
			chatTool("read", "x"),
		},
	}, false)
	names := toolNames(body["tools"])
	for _, banned := range []string{"CronCreate", "CreateWorkflow", "AskUserQuestion"} {
		if containsStr(names, banned) {
			t.Fatalf("%s should have been dropped: %v", banned, names)
		}
	}
	// 前缀不能误伤：CronList 不在名单里，必须保留。
	if !containsStr(names, "CronList") {
		t.Fatalf("CronList was dropped although only exact names were listed: %v", names)
	}
	// 未列出的客户端工具保留。
	if !containsStr(names, "read") {
		t.Fatalf("read was dropped: %v", names)
	}
}

// 描述截断只动 description，parameters 必须原样保留——工具调用的参数结构
// 是模型能正确调用工具的前提。
func TestZenToolFilterTruncatesDescriptionKeepsParameters(t *testing.T) {
	setZenToolFilter(t, zenToolFilterConfig{Enabled: true, MaxDescription: 20})
	long := strings.Repeat("detailed explanation. ", 50)
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"file_path": map[string]any{"type": "string", "description": "path"},
		},
		"required": []any{"file_path"},
	}
	body := buildZenBody(map[string]any{
		"model":    "big-pickle",
		"messages": []any{},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "keep_me", "description": long, "parameters": params,
		}}},
	}, false)
	var found map[string]any
	for _, raw := range body["tools"].([]any) {
		fn, _ := raw.(map[string]any)["function"].(map[string]any)
		if n, _ := fn["name"].(string); n == "keep_me" {
			found = fn
		}
	}
	if found == nil {
		t.Fatal("tool with a long description was dropped instead of truncated")
	}
	d, _ := found["description"].(string)
	// 截断后补 " …"（2 个 rune），所以上限是 max+2。
	if len([]rune(d)) > 20+2 {
		t.Fatalf("description not truncated: %d runes", len([]rune(d)))
	}
	if !strings.HasSuffix(d, "…") {
		t.Fatalf("truncated description must end with an ellipsis marker: %q", d)
	}
	// parameters 完整保留
	pj, _ := json.Marshal(found["parameters"])
	if !strings.Contains(string(pj), "file_path") || !strings.Contains(string(pj), "required") {
		t.Fatalf("parameters were damaged by the filter: %s", pj)
	}
}

// MaxDescription=0 表示不截断。
func TestZenToolFilterNoTruncationWhenZero(t *testing.T) {
	setZenToolFilter(t, zenToolFilterConfig{Enabled: true})
	long := strings.Repeat("x", 500)
	body := buildZenBody(map[string]any{
		"model": "big-pickle", "messages": []any{},
		"tools": []any{chatTool("keep_me", long)},
	}, false)
	for _, raw := range body["tools"].([]any) {
		fn, _ := raw.(map[string]any)["function"].(map[string]any)
		if n, _ := fn["name"].(string); n == "keep_me" {
			if d, _ := fn["description"].(string); d != long {
				t.Fatalf("description changed with MaxDescription=0: %d runes", len(d))
			}
			return
		}
	}
	t.Fatal("tool missing")
}

// 裁剪不得就地改写客户端参数：端点自适应重试/日志/token 估算随后还要读原形态。
func TestZenToolFilterDoesNotMutateClientParams(t *testing.T) {
	setZenToolFilter(t, zenToolFilterConfig{
		Enabled: true, DenyPrefixes: []string{"mcp__"}, MaxDescription: 5,
	})
	longDesc := strings.Repeat("y", 200)
	tools := []any{
		chatTool("mcp__x", "drop me"),
		chatTool("keep", longDesc),
	}
	params := map[string]any{"model": "big-pickle", "messages": []any{}, "tools": tools}
	_ = buildZenBody(params, false)

	orig := params["tools"].([]any)
	if len(orig) != 2 {
		t.Fatalf("client tools slice shrank to %d: filter mutated caller's params", len(orig))
	}
	fn := orig[1].(map[string]any)["function"].(map[string]any)
	if d, _ := fn["description"].(string); d != longDesc {
		t.Fatalf("client tool description mutated: %d runes", len(d))
	}
}

// 全部客户端工具被丢弃时，退回"只有 gate 工具 + tool_choice=none"的形态，
// 不能让上游按 auto 让模型调用客户端没有执行层的工具。
func TestZenToolFilterAllDroppedFallsBackToGateOnly(t *testing.T) {
	setZenToolFilter(t, zenToolFilterConfig{Enabled: true, DenyPrefixes: []string{"mcp__"}})
	body := buildZenBody(map[string]any{
		"model": "big-pickle", "messages": []any{},
		"tools":   []any{chatTool("mcp__a", "x"), chatTool("mcp__b", "y")},
		"tool_choice": "auto",
	}, false)
	if got := len(body["tools"].([]any)); got != len(zenGateToolSpecs) {
		t.Fatalf("tools = %d, want gate-only %d", got, len(zenGateToolSpecs))
	}
	if body["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %v, want none when every client tool was dropped", body["tool_choice"])
	}
}

// responses 扁平路径同样过滤，且 gate 工具保留。
func TestZenToolFilterResponsesPath(t *testing.T) {
	setZenToolFilter(t, zenToolFilterConfig{Enabled: true, DenyPrefixes: []string{"mcp__"}})
	body := buildZenResponsesBody(map[string]any{
		"model": "deepseek-v4.1-flash-free",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"tools": []any{
			flatTool("mcp__node_repl__js", "big"),
			flatTool("read", "read a file"),
		},
	}, "deepseek-v4.1-flash-free", "pk")
	names := toolNames(body["tools"])
	if containsStr(names, "mcp__node_repl__js") {
		t.Fatalf("mcp tool survived the responses-path filter: %v", names)
	}
	if !containsStr(names, "read") {
		t.Fatalf("read was dropped: %v", names)
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("responses tool_choice must stay auto, got %v", body["tool_choice"])
	}
}

// 真实量级：用 ZCode 抓包里最大的几个工具名验证能省下多少（确保过滤确实
// 命中体积最大的那批，而不是无关紧要的小工具）。
func TestZenToolFilterHitsTheLargestSchemas(t *testing.T) {
	big := []string{"CronCreate", "AskUserQuestion", "EnterPlanMode", "CreateWorkflow", "mcp__node_repl__js"}
	setZenToolFilter(t, zenToolFilterConfig{
		Enabled:      true,
		DenyPrefixes: []string{"mcp__"},
		DenyNames:    []string{"CronCreate", "AskUserQuestion", "EnterPlanMode", "CreateWorkflow"},
	})
	for _, n := range big {
		if !zenToolDenied(n, getZenConfig().ToolFilter) {
			t.Errorf("%q is one of the largest schemas but is not denied by the default-ish list", n)
		}
	}
	// 小工具不该被误伤。
	for _, n := range []string{"read", "write", "edit", "shell", "grep", "glob"} {
		if zenToolDenied(n, getZenConfig().ToolFilter) {
			t.Errorf("core coding tool %q must not be denied", n)
		}
	}
}

// ---- helpers ----

func chatTool(name, desc string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": desc,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
	}}
}

func flatTool(name, desc string) map[string]any {
	return map[string]any{
		"type": "function", "name": name, "description": desc,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		"strict":     false,
	}
}

func toolNames(v any) []string {
	var out []string
	list, _ := v.([]any)
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		if n, _ := m["name"].(string); n != "" {
			out = append(out, n)
			continue
		}
		if fn, ok := m["function"].(map[string]any); ok {
			if n, _ := fn["name"].(string); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
