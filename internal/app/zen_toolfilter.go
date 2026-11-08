package app

import (
	"encoding/json"
	"log"
	"strings"
)

// ============ zen 请求体积裁剪（工具过滤 + 描述截断） ============
//
// 背景（2026-11-07 抓包实测）：ZCode 一次真实对话的上游 body 是 137KB，
// 其中 tools 占 112KB（82%），76 个工具里最大的几个是 CronCreate 6.5KB、
// AskUserQuestion 5.1KB、EnterPlanMode 4.4KB、CreateWorkflow 4.1KB、
// mcp__node_repl__js 3.3KB。单条消息因此约 33k tokens，而免费层日额度
// 撑不住几条——429 的定性是**体积**，不是内容（同 key 同出口，ping 45
// tokens 成功、IDE 一单就 429）。
//
// 所以这里做真正减体积的事（不改写内容、不伪装客户端）：按名字前缀/名单
// 丢弃客户端工具，并对过长的 description 做截断。schema 一律保留——模型
// 调用工具靠的是 parameters 结构，描述只是提示。
//
// 关键约束：只过滤**客户端**工具。12 个 gate 占位工具由 zenMergeTools 在
// 之后合并，绝不参与过滤（缺了它们上游直接 403）。

// zenToolFilterConfig 体积裁剪配置。全部默认关闭：丢工具会改变模型可用
// 能力，必须由使用者显式开启（面板或配置文件）。
type zenToolFilterConfig struct {
	// Enabled 总开关。false = 完全透传客户端工具（历史行为）。
	Enabled bool `json:"enabled"`
	// DenyPrefixes 按前缀丢弃（大小写不敏感）。典型值 "mcp__"——MCP 工具
	// 单个体积最大，且一次对话里往往只用其中一两个。
	DenyPrefixes []string `json:"denyPrefixes,omitempty"`
	// DenyNames 按精确名丢弃（大小写不敏感）。用于 cron/workflow/plan-mode
	// 这类 IDE 附带但单轮对话很少用到的重型工具。
	DenyNames []string `json:"denyNames,omitempty"`
	// MaxDescription 描述字符上限（按 rune 计），0 = 不截断。截断只动
	// description，parameters 原样保留，因此工具调用仍然有完整的参数结构。
	MaxDescription int `json:"maxDescription,omitempty"`
}

func defaultZenToolFilter() zenToolFilterConfig {
	// 默认值只在 Enabled 被显式打开时生效：给出一套"通用 IDE 重型工具"
	// 名单，但 Enabled 仍是 false。使用者开启总开关即可获得这份默认名单，
	// 也可以清空/改写它。
	return zenToolFilterConfig{
		Enabled:      false,
		DenyPrefixes: []string{"mcp__"},
		DenyNames:    []string{},
		// 0 = 不截断：先只做丢弃，避免默认行为里混入质量风险。
		MaxDescription: 0,
	}
}

// zenToolFilterActive 返回生效的过滤配置；未启用时 ok=false。
func zenToolFilterActive() (zenToolFilterConfig, bool) {
	c := getZenConfig().ToolFilter
	if !c.Enabled {
		return c, false
	}
	return c, true
}

// zenToolDenied 判断工具名是否命中丢弃规则。
func zenToolDenied(name string, cfg zenToolFilterConfig) bool {
	if name == "" {
		return false
	}
	lower := strings.ToLower(name)
	for _, f := range cfg.DenyPrefixes {
		f = strings.ToLower(strings.TrimSpace(f))
		if f != "" && strings.HasPrefix(lower, f) {
			return true
		}
	}
	for _, n := range cfg.DenyNames {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" && lower == n {
			return true
		}
	}
	return false
}

// truncateZenDescription 按 rune 截断描述并补省略号。
func truncateZenDescription(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max]), " \t\n") + " …"
}

// zenFilterToolStats 记录一次裁剪的字节数，用于日志/抓包观测。
type zenFilterToolStats struct {
	In       int
	Out      int
	Dropped  []string
	BytesIn  int
	BytesOut int
}

// zenFilterClientToolsChat 过滤 chat 嵌套形态的客户端工具
// (tools[].function{name,description,parameters})。
//
// 返回新切片，不修改传入的 map：params 是客户端参数，端点自适应重试、
// 日志和 token 估算随后还要读原形态（与 env marker / 换位同样的纪律）。
func zenFilterClientToolsChat(tools []any, cfg zenToolFilterConfig) ([]any, zenFilterToolStats) {
	st := zenFilterToolStats{In: len(tools)}
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		tm, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		name, _ := fn["name"].(string)
		st.BytesIn += approxToolBytes(tm)
		if zenToolDenied(name, cfg) {
			st.Dropped = append(st.Dropped, name)
			continue
		}
		// 复制 function 再改 description，避免污染客户端参数。
		nf := make(map[string]any, len(fn))
		for k, v := range fn {
			nf[k] = v
		}
		if d, ok := nf["description"].(string); ok {
			nf["description"] = truncateZenDescription(d, cfg.MaxDescription)
		}
		cp := make(map[string]any, len(tm))
		for k, v := range tm {
			cp[k] = v
		}
		cp["function"] = nf
		st.BytesOut += approxToolBytes(cp)
		out = append(out, cp)
	}
	st.Out = len(out)
	return out, st
}

// zenFilterClientToolsFlat 过滤 responses 扁平形态的工具
// ({type,name,description,parameters,strict})。
func zenFilterClientToolsFlat(tools []map[string]any, cfg zenToolFilterConfig) ([]map[string]any, zenFilterToolStats) {
	st := zenFilterToolStats{In: len(tools)}
	out := make([]map[string]any, 0, len(tools))
	for _, tm := range tools {
		name, _ := tm["name"].(string)
		st.BytesIn += approxToolBytes(tm)
		if zenToolDenied(name, cfg) {
			st.Dropped = append(st.Dropped, name)
			continue
		}
		cp := make(map[string]any, len(tm))
		for k, v := range tm {
			cp[k] = v
		}
		if d, ok := cp["description"].(string); ok {
			cp["description"] = truncateZenDescription(d, cfg.MaxDescription)
		}
		st.BytesOut += approxToolBytes(cp)
		out = append(out, cp)
	}
	st.Out = len(out)
	return out, st
}

// approxToolBytes 估算单个工具定义的序列化体积（用于日志与抓包统计，
// 不需要精确——精确值由抓包的 upstreamBody 提供）。
func approxToolBytes(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// logZenToolFilter 只在真的裁掉了东西时打一行，避免每请求一条噪声日志。
func logZenToolFilter(label string, st zenFilterToolStats) {
	if st.In == st.Out && st.BytesIn == st.BytesOut {
		return
	}
	saved := st.BytesIn - st.BytesOut
	if saved < 0 {
		saved = 0
	}
	log.Printf("  zen tool filter(%s): %d→%d tools, %d→%d bytes (saved %d B ≈ %d tok)%s",
		label, st.In, st.Out, st.BytesIn, st.BytesOut, saved, saved/4,
		droppedSummary(st.Dropped))
}

func droppedSummary(dropped []string) string {
	if len(dropped) == 0 {
		return ""
	}
	const max = 6
	if len(dropped) <= max {
		return " dropped=" + strings.Join(dropped, ",")
	}
	return " dropped=" + strings.Join(dropped[:max], ",") + ",…"
}
