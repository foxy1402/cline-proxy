package app

import "testing"

// 2026-10-15 回归：新后端要求注入工具带真实参数 schema，空 schema
// （{"type":"object","properties":{}}）→ 403 FreeTierError。
// 这里锁定：11 个 gate 工具齐全、两条路径的 parameters 都非空。
// 注意（实测二分结论）：校验的是 schema，不是描述——描述文本改成 1 个字符
// 仍然 200，所以本测试不拿描述长度当门禁条件。
func TestZenGateToolsCarryRealSchemas(t *testing.T) {
	tools := zenGateTools()
	if len(tools) != len(zenGateToolSpecs) {
		t.Fatalf("gate tools = %d, want %d", len(tools), len(zenGateToolSpecs))
	}
	for _, tv := range tools {
		tm, _ := tv.(map[string]any)
		fn, _ := tm["function"].(map[string]any)
		name, _ := fn["name"].(string)
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if len(props) == 0 {
			t.Errorf("chat gate tool %q has empty parameter properties (new backend 403s this)", name)
		}
		if desc, _ := fn["description"].(string); desc == "" {
			t.Errorf("chat gate tool %q has no description (real CLI always sends one)", name)
		}
	}

	merged := zenMergeResponsesTools(nil)
	if len(merged) != len(zenGateToolSpecs) {
		t.Fatalf("responses gate tools = %d, want %d", len(merged), len(zenGateToolSpecs))
	}
	for _, tv := range merged {
		tm, _ := tv.(map[string]any)
		name, _ := tm["name"].(string)
		params, _ := tm["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if len(props) == 0 {
			t.Errorf("responses gate tool %q has empty parameter properties (new backend 403s this)", name)
		}
		if _, hasSchema := params["$schema"]; hasSchema {
			t.Errorf("responses gate tool %q parameters must not carry $schema (responses form strips it)", name)
		}
	}
}

// 名录与嵌入定义必须一一对应：zenGateTools/zenMergeResponsesTools 以
// zenGateToolSpecs 的名字为顺序，但 schema 从嵌入定义按名查得；任何一侧改名
// 而另一侧没跟，就会静默回退到空 schema stub —— 那正是新后端 403 的成因。
// 这条测试让改名在编译期后立刻被抓住，而不是等线上全量 403。
func TestZenGateToolDefsCoverEverySpecName(t *testing.T) {
	defs := zenGateToolDefs()
	if len(defs) == 0 {
		t.Fatal("embedded gate tool definitions failed to parse (zen_gate_tools.json)")
	}
	byName := make(map[string]bool, len(defs))
	for _, d := range defs {
		if d.Name == "" {
			t.Error("embedded gate tool def with empty name")
		}
		if len(d.ParametersChat) == 0 || len(d.ParametersResponses) == 0 {
			t.Errorf("gate tool %q missing a chat or responses schema variant", d.Name)
		}
		byName[d.Name] = true
	}
	for _, s := range zenGateToolSpecs {
		if !byName[s.name] {
			t.Errorf("gate tool %q is in zenGateToolSpecs but has no embedded definition — "+
				"it would fall back to an empty schema and 403", s.name)
		}
	}
	for name := range byName {
		if lookupZenGateToolDef(name) == nil {
			t.Errorf("lookupZenGateToolDef(%q) returned nil despite being in the def list", name)
		}
	}
}
