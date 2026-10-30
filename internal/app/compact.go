package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// opencode 官方会话压缩 (SessionCompaction) 的 Go 移植
// 源码: packages/core/src/session/compaction.ts + packages/opencode/src/session/compaction.ts
// 机制: 超限时 select() 尾部预算选择 -> buildPrompt() 锚定摘要 -> LLM 生成摘要
//       -> 重组为 [摘要 + recent 尾部] 继续会话,下次压缩增量更新摘要
// ============================================================================

const (
	summaryOutputTokens = 4096 // SUMMARY_OUTPUT_TOKENS
	toolOutputMaxChars  = 2000 // TOOL_OUTPUT_MAX_CHARS
)

// summaryTemplate 官方 SUMMARY_TEMPLATE 原文
const summaryTemplate = `Output exactly the Markdown structure shown inside <template> and keep the section order unchanged. Do not include the <template> tags in your response.
<template>
## Objective
- [one or two brief sentences describing what the user is trying to accomplish]

## Important Details
- [constraints/preferences, decisions and why, important facts/assumptions, exact context needed to continue, or "(none)"]

## Work State
### Completed
- [finished work, verified facts, or changes made; otherwise "(none)"]

### Active
- [current work, partial changes, or investigation state; otherwise "(none)"]

### Blocked
- [blockers, failing commands, or unknowns; otherwise "(none)"]

## Next Move
1. [immediate concrete action, or "(none)"]
2. [next action if known, or "(none)"]

## Relevant Files
- [file or directory path: why it matters, or "(none)"]
</template>

Rules:
- Keep every section, even when empty.
- Use terse bullets, not prose paragraphs.
- Preserve exact file paths, symbols, commands, error strings, URLs, and identifiers when known.
- Do not mention the summary process or that context was compacted.`

// compactState 会话压缩状态(增量摘要记忆)
type compactState struct {
	summary string // 上次生成的锚定摘要
	recent  string // 上次保留的 recent 尾部文本
	updated time.Time
}

var (
	compactStates   = make(map[string]*compactState)
	compactStatesMu sync.Mutex
)

// ============ 消息序列化(官方 serialize 移植) ============

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func msgText(m map[string]any) string {
	content := m["content"]
	switch c := content.(type) {
	case string:
		return c
	case []any:
		parts := []string{}
		for _, block := range c {
			if b, ok := block.(map[string]any); ok {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func truncateMsg(s string) string {
	if len(s) <= toolOutputMaxChars {
		return s
	}
	return s[:toolOutputMaxChars] + "\n[truncated]"
}

// serializeMsg 单条消息 -> 一行文本(官方 serialize 格式)
func serializeMsg(m map[string]any) string {
	switch strField(m, "role") {
	case "user":
		return "[User]: " + msgText(m)
	case "system":
		return "[System update]: " + msgText(m)
	case "tool":
		return "[Tool result]: " + truncateMsg(msgText(m))
	case "assistant":
		lines := []string{}
		if t := msgText(m); t != "" {
			lines = append(lines, "[Assistant]: "+t)
		}
		if r := strField(m, "reasoning_content"); r != "" {
			lines = append(lines, "[Assistant reasoning]: "+r)
		}
		if tc, ok := m["tool_calls"].([]any); ok {
			for _, c := range tc {
				if cm, ok := c.(map[string]any); ok {
					fn, _ := cm["function"].(map[string]any)
					name := ""
					args := ""
					if fn != nil {
						name, _ = fn["name"].(string)
						if a, ok := fn["arguments"].(string); ok {
							args = a
						}
					}
					if name == "" {
						continue
					}
					lines = append(lines, fmt.Sprintf("[Assistant tool call]: %s(%s)", name, args))
				}
			}
		}
		return strings.Join(lines, "\n")
	}
	return ""
}

// ============ 尾部预算选择(官方 select 移植) ============

type selectResult struct {
	head   []string // 摘要部分文本行
	recent []string // 保留的 recent 文本行
	split  int      // 原始消息的 split 索引(recent 从该索引起)
}

// selectRecent 从尾部往前累计 token 预算;放不下时把越界消息拆成 prefix(进 head)/suffix(进 recent)
func selectRecent(serialized []string, keepTokens int) *selectResult {
	if len(serialized) == 0 || keepTokens <= 0 {
		return nil
	}
	total := 0
	split := len(serialized)
	var splitPrefix, splitSuffix string
	for i := len(serialized) - 1; i >= 0; i-- {
		next := total + estimateText(serialized[i])
		if next > keepTokens {
			remaining := keepTokens - total
			if remaining > 0 {
				remainingChars := remaining * 4
				s := serialized[i]
				rs := []rune(s)
				if remainingChars <= 0 {
					split = i + 1
				} else if len(rs) > remainingChars {
					splitPrefix = string(rs[:len(rs)-remainingChars])
					splitSuffix = string(rs[len(rs)-remainingChars:])
				} else {
					splitSuffix = s
				}
				split = i + 1
			}
			break
		}
		total = next
		split = i
	}
	if split == 0 {
		return nil
	}
	head := make([]string, 0, split+1)
	head = append(head, serialized[:split]...)
	if splitPrefix != "" {
		head = append(head, splitPrefix)
	}
	recent := make([]string, 0, len(serialized)-split+1)
	if splitSuffix != "" {
		recent = append(recent, splitSuffix)
	}
	recent = append(recent, serialized[split:]...)
	return &selectResult{head: head, recent: recent, split: split}
}

// ============ 摘要提示词(官方 buildPrompt 移植) ============

func buildSummaryPrompt(previousSummary string, context []string) string {
	var prefix string
	if previousSummary != "" {
		prefix = "Update the anchored summary below using the conversation history above.\n" +
			"Preserve still-true details, remove stale details, and merge in the new facts.\n" +
			"<previous-summary>\n" + previousSummary + "\n</previous-summary>"
	} else {
		prefix = "Create a new anchored summary from the conversation history."
	}
	parts := append([]string{prefix, summaryTemplate}, context...)
	return strings.Join(parts, "\n\n")
}

// ============ 摘要生成 ============

func generateSummary(modelID, prompt string, maxSummary int) (string, error) {
	body := map[string]any{
		"model":      modelID,
		"messages":   []any{map[string]any{"role": "user", "content": prompt}},
		"max_tokens": maxSummary,
		"stream":     true, // 见下：上游只接受流式请求
	}
	// 后台摘要生成与客户端请求无关,用独立 context,不受客户端 abort 影响。
	// 必须带超时: 上游连接挂起时否则永久占住请求与 zen 并发槽位
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// 两个上游端点都只接受 stream=true（chat/responses 的 FreeTier gate 对
	// stream=false 直接 403），非流式形态由这里聚合。此前恒发 stream=false，
	// 每次摘要必然 403 然后退回有损截断。
	var raw map[string]any
	if zm, ok := resolveZenModel(modelID); ok && zm.Upstream == "responses" {
		resp, _, err := callZenResponsesAPI(ctx, body, true)
		if err != nil {
			return "", err
		}
		raw, err = responsesSSEToChat(resp)
		if err != nil {
			return "", err
		}
	} else {
		resp, _, err := callZenAPI(ctx, body, true)
		if err != nil {
			return "", err
		}
		raw, err = collectStreamResponse(resp)
		if err != nil {
			return "", err
		}
	}
	if choices, ok := raw["choices"].([]any); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			if msg, ok := ch["message"].(map[string]any); ok {
				if s, ok := msg["content"].(string); ok {
					return strings.TrimSpace(s), nil
				}
			}
		}
	}
	return "", fmt.Errorf("no content in summary response")
}

// toolCallIDs 取 assistant 消息里所有 tool_calls 的 id（缺失的 id 也会列出，
// 调用方按需要跳过空值）。
func toolCallIDs(mm map[string]any) []string {
	tcs, ok := mm["tool_calls"].([]any)
	if !ok || len(tcs) == 0 {
		return nil
	}
	out := make([]string, 0, len(tcs))
	for _, tc := range tcs {
		tcm, _ := tc.(map[string]any)
		if tcm == nil {
			continue
		}
		id, _ := tcm["id"].(string)
		out = append(out, id)
	}
	return out
}

// ============ 估算(官方 Token.estimate 近似: JSON 长度 / 4) ============

func estimateText(s string) int {
	return len([]rune(s)) / 4
}

func estimateJSON(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

// ============ 会话状态 ============

// loadCompactState 只读查询会话压缩状态；不存在返回 nil（不插入）。
// 插入只发生在 updateCompactState —— 防止客户端伪造海量 session ID
// 把 compactStates 撑爆（条目只在真正压缩过的会话上创建）。
func loadCompactState(sessionID string) *compactState {
	if sessionID == "" {
		return nil
	}
	compactStatesMu.Lock()
	defer compactStatesMu.Unlock()
	return compactStates[sessionID]
}

func updateCompactState(sessionID string, summary, recent string) {
	if sessionID == "" {
		return
	}
	compactStatesMu.Lock()
	defer compactStatesMu.Unlock()
	compactStates[sessionID] = &compactState{
		summary: summary,
		recent:  recent,
		updated: time.Now(),
	}
}

func cleanupCompactStates() {
	ticker := time.NewTicker(30 * time.Minute)
	for range ticker.C {
		compactStatesMu.Lock()
		cutoff := time.Now().Add(-24 * time.Hour)
		for k, v := range compactStates {
			if v.updated.Before(cutoff) {
				delete(compactStates, k)
			}
		}
		compactStatesMu.Unlock()
	}
}

func requestSessionID(r map[string]any, hdr http.Header) string {
	if sid := hdr.Get("x-opencode-session"); sid != "" {
		return sid
	}
	if sid := strField(r, "session_id"); sid != "" {
		return sid
	}
	return ""
}

// ============ 压缩入口 ============

type compactOutcome struct {
	changed       bool
	note          string
	compactTokens int // 摘要生成消耗的估算 token
}

// maybeCompact 官方 compactIfNeeded 移植:
// 估算超 context - max(output, buffer) 时 -> select -> 摘要 -> 重组 [system]+[摘要]+recent
func maybeCompact(params map[string]any, m *ZenModel, sessionID string) compactOutcome {
	cfg := getZenConfig()
	if !cfg.Enabled || !cfg.Compaction.Auto {
		return compactOutcome{}
	}
	context := m.Context
	if context <= 0 {
		return compactOutcome{}
	}
	output := m.Output
	buffer := cfg.Compaction.Buffer
	if buffer <= 0 {
		buffer = defaultCompactBuffer
	}
	keep := cfg.Compaction.KeepTokens
	if keep <= 0 {
		keep = 8000
	}
	maxSum := cfg.Compaction.MaxSummary
	if maxSum <= 0 {
		maxSum = summaryOutputTokens
	}

	threshold := compactThreshold(context, output, buffer)
	if estimateJSON(params) <= threshold {
		return compactOutcome{}
	}

	messages, _ := params["messages"].([]any)
	if len(messages) == 0 {
		return compactOutcome{}
	}

	// 1. 序列化(一一对应原始消息；split 是 serialized 的下标，
	//    切片时必须映射回 messages 原始下标，否则尾部错位)
	serialized := make([]string, 0, len(messages))
	orig := make([]int, 0, len(messages))
	for i, msg := range messages {
		if mm, ok := msg.(map[string]any); ok {
			serialized = append(serialized, serializeMsg(mm))
			orig = append(orig, i)
		}
	}

	// 2. select 尾部预算
	sel := selectRecent(serialized, keep)
	if sel == nil || sel.split <= 0 {
		return compactOutcome{}
	}
	// 尾部不能以 tool 结果消息开头 —— 它对应的 assistant tool_calls 消息
	// 在被丢弃的头部里，OpenAI 方言上游会 400 拒绝；compaction 对同一会话
	// 是确定性的，不修正会变成每次重试都失败的死循环
	// 注意: sel.split == len(orig) 是合法状态 —— 最后一条消息本身超预算被
	// 拆分，recent 没有完整原始消息（尾部全部进了摘要），索引前必须判界
	for sel.split > 0 && sel.split < len(orig) {
		mm, ok := messages[orig[sel.split]].(map[string]any)
		if !ok || strField(mm, "role") != "tool" {
			break
		}
		sel.split--
	}
	hasTail := sel.split > 0 && sel.split < len(orig)
	tailOrig := 0
	if hasTail {
		tailOrig = orig[sel.split]
	} else {
		sel.split = len(orig)
	}
	if !hasTail && len(sel.head) == 0 {
		return compactOutcome{}
	}
	sel.recent = append([]string{}, serialized[sel.split:]...)

	// 3. 增量摘要: 优先复用会话摘要; 无会话时检查是否有历史摘要消息
	st := loadCompactState(sessionID)
	previousSummary := ""
	previousRecent := ""
	if st != nil {
		previousSummary = st.summary
		previousRecent = st.recent
	}
	if previousSummary == "" {
		previousSummary = findExistingSummary(messages, tailOrig)
	}

	head := strings.Join(sel.head, "\n\n")
	contextParts := []string{}
	if previousRecent != "" {
		contextParts = append(contextParts, previousRecent)
	}
	if head != "" {
		contextParts = append(contextParts, head)
	}
	if previousSummary == "" && head == "" && previousRecent == "" {
		return compactOutcome{}
	}
	prompt := buildSummaryPrompt(previousSummary, contextParts)

	// 4. 调摘要模型(默认同请求模型)
	summaryModel := cfg.Compaction.SummaryModel
	if summaryModel == "" {
		summaryModel = m.ID
	}
	log.Printf("  compact: ctx=%d est=%d > threshold=%d keep=%d split@%d summary_model=%s",
		context, estimateJSON(params), threshold, keep, sel.split, summaryModel)
	summary, err := generateSummary(summaryModel, prompt, maxSum)
	if err != nil {
		log.Printf("  compact: summary generation failed (%v), falling back to truncation", err)
		return fallbackTruncate(params, m)
	}

	// 5. 重组: [原system]+[摘要]+[recent 尾部原始消息]
	var newMsgs []any
	for _, msg := range messages {
		if mm, ok := msg.(map[string]any); ok && strField(mm, "role") == "system" {
			newMsgs = append(newMsgs, mm)
		}
	}
	newMsgs = append(newMsgs, map[string]any{
		"role":    "system",
		"content": "[Conversation Summary]\n" + summary,
	})
	// 旧摘要已由 buildSummaryPrompt 合并进新摘要，这里不再重复注入
	//（重复注入会在每次 compact 后浪费真实上下文）
	if hasTail {
		newMsgs = append(newMsgs, messages[tailOrig:]...)
	}

	updateCompactState(sessionID, summary, strings.Join(sel.recent, "\n\n"))
	params["messages"] = newMsgs
	kept := 0
	if hasTail {
		kept = len(messages) - tailOrig
	}
	return compactOutcome{
		changed:       true,
		note:          fmt.Sprintf("[compacted via summary] summary_model=%s kept=%d msgs", summaryModel, kept),
		compactTokens: estimateText(prompt) + estimateText(summary),
	}
}

// findExistingSummary 从已有消息中寻找历史摘要(兼容有会话历史的客户端)
func findExistingSummary(messages []any, upTo int) string {
	if upTo < 0 {
		upTo = 0
	}
	if upTo > len(messages) {
		upTo = len(messages)
	}
	for i := 0; i < upTo; i++ {
		mm, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		c := strField(mm, "content")
		for _, prefix := range []string{"[Conversation Summary]", "[Previous Conversation Summary]"} {
			if strings.HasPrefix(c, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(c, prefix))
			}
		}
	}
	return ""
}

// compactThreshold 触发压缩的估算 token 阈值。
//
// 下限兜底：目录/overlay 的 output 声明若不小于 context（小窗口模型配默认 32768
// output 就会这样），threshold 会算成 0 或负数 —— 于是每个请求都判定"需要压缩"，
// 而压缩又永远降不到阈值以下，形成每次请求都多跑一次摘要模型的循环。至少给输入
// 留一半窗口。
func compactThreshold(context, output, buffer int) int {
	threshold := context - max(output, buffer)
	if threshold < context/2 {
		return context / 2
	}
	return threshold
}

// fallbackTruncate 摘要失败时退回老式截断: 保留 system + 尾部消息至 60% 预算
func fallbackTruncate(params map[string]any, m *ZenModel) compactOutcome {
	messages, _ := params["messages"].([]any)
	if len(messages) == 0 {
		return compactOutcome{}
	}
	budget := int(float64(m.Context) * 0.6)
	type idxMsg struct {
		idx int
		msg any
	}
	kept := []idxMsg{}
	used := 0
	for i, msg := range messages {
		if mm, ok := msg.(map[string]any); ok && strField(mm, "role") == "system" {
			kept = append(kept, idxMsg{i, msg})
			used += estimateText(msgText(mm))
		}
	}
	for i := len(messages) - 1; i >= 0 && used < budget; i-- {
		isKept := false
		for _, k := range kept {
			if k.idx == i {
				isKept = true
				break
			}
		}
		if isKept {
			continue
		}
		mm, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		// 只算文本会大幅低估 —— assistant 的 tool_calls 参数（Write 等工具
		// 的文件内容）和 reasoning_content 往往是体积大头
		t := estimateText(msgText(mm))
		if r := strField(mm, "reasoning_content"); r != "" {
			t += estimateText(r)
		}
		if tc, ok := mm["tool_calls"].([]any); ok {
			t += estimateJSON(tc)
		}
		if used+t > budget {
			// 放不下就停：从尾部往前扫，一旦超预算即结束，保证保留的是**连续尾部**。
			// 早先这里是 continue —— 跳过放不下的继续找更老的，结果会在会话中间
			// 挖洞（保留 0..3 和 20..25，丢掉 4..19），模型看到的是断裂且无提示的
			// 历史；更糟的是最新一轮的上下文（最相关）常被更古老的记忆挤掉。
			break
		}
		kept = append(kept, idxMsg{i, messages[i]})
		used += t
	}
	sort.Slice(kept, func(a, b int) bool { return kept[a].idx < kept[b].idx })
	// 工具链完整性：截断是按预算逐条挑选的，天然会拆散"调用 → 结果"配对，
	// 而只留其一上游一律 400（assistant.tool_calls 缺结果 / tool 结果缺调用）。
	// 两个方向互相影响（丢弃 assistant 会让它的结果变孤儿，反之亦然），
	// 所以反复剔除直到稳定。上一版只查"前一条消息是否保留"，遗漏了
	// 反向孤儿（assistant 的调用被保留、结果被丢弃）。
	for pass := 0; pass < 5; pass++ {
		callIDs := map[string]bool{}
		resultIDs := map[string]bool{}
		for _, k := range kept {
			mm, _ := k.msg.(map[string]any)
			if mm == nil {
				continue
			}
			switch strField(mm, "role") {
			case "assistant":
				for _, id := range toolCallIDs(mm) {
					callIDs[id] = true
				}
			case "tool":
				if id := strField(mm, "tool_call_id"); id != "" {
					resultIDs[id] = true
				}
			}
		}
		next := make([]idxMsg, 0, len(kept))
		dropped := false
		for _, k := range kept {
			mm, _ := k.msg.(map[string]any)
			if mm != nil {
				switch strField(mm, "role") {
				case "assistant":
					complete := true
					for _, id := range toolCallIDs(mm) {
						if id != "" && !resultIDs[id] {
							complete = false
							break
						}
					}
					if !complete {
						dropped = true
						continue
					}
				case "tool":
					if id := strField(mm, "tool_call_id"); id != "" && !callIDs[id] {
						dropped = true
						continue
					}
				}
			}
			next = append(next, k)
		}
		kept = next
		if !dropped {
			break
		}
	}
	out := make([]any, 0, len(kept)+1)
	out = append(out, map[string]any{
		"role":    "system",
		"content": fmt.Sprintf("[context compaction] 上下文估算超过该模型限制(约 %d token),早期消息已被截断以继续会话。", m.Context),
	})
	for _, k := range kept {
		out = append(out, k.msg)
	}
	params["messages"] = out
	return compactOutcome{changed: true, note: "[compacted via truncation]"}
}
