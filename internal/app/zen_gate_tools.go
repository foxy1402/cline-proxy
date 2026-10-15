package app

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// ============================================================================
// Zen 免费层 gate 工具的真实定义（描述 + 参数 schema）
//
// 2026-10-15 实测（新 inference 后端，干净 IP 二分定位）：迁移后的免费层
// 不再只校验工具名 —— 注入工具的 parameters 若是空 schema
// （{"type":"object","properties":{}}）直接 403 FreeTierError；换成官方
// CLI 1.19.0 的真实参数 schema 即 200。描述文本不参与校验（精简描述 +
// 真实 schema 实测通过），但这里仍存官方原文，保持与 CLI 字节级一致，
// 避免上游日后加严描述校验时再次全灭。
//
// 数据来源：opencode CLI 1.19.0 本地抓包（chat 形态与 responses 形态的
// parameters 不同：responses 的 @ai-sdk/openai 会剥 $schema 与 min/max
// 约束，故两套都存）。zen_gate_tools.json 由抓包生成，勿手改。
// ============================================================================

//go:embed zen_gate_tools.json
var zenGateToolsJSON []byte

type zenGateToolDef struct {
	Name                string         `json:"name"`
	Description         string         `json:"description"`
	ParametersChat      map[string]any `json:"parameters_chat"`
	ParametersResponses map[string]any `json:"parameters_responses"`
}

var (
	zenGateDefsOnce sync.Once
	zenGateDefs     []zenGateToolDef
	zenGateDefByName map[string]*zenGateToolDef
)

func zenGateToolDefs() []zenGateToolDef {
	zenGateDefsOnce.Do(func() {
		var defs []zenGateToolDef
		if err := json.Unmarshal(zenGateToolsJSON, &defs); err == nil {
			zenGateDefs = defs
			zenGateDefByName = make(map[string]*zenGateToolDef, len(defs))
			for i := range zenGateDefs {
				zenGateDefByName[zenGateDefs[i].Name] = &zenGateDefs[i]
			}
		}
	})
	return zenGateDefs
}

func lookupZenGateToolDef(name string) *zenGateToolDef {
	zenGateToolDefs()
	return zenGateDefByName[name]
}
