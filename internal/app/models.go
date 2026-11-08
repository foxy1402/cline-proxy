package app

import (
	"cline-go-proxy/internal/cline"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ModelStatus string

const (
	ModelActive  ModelStatus = "active"
	ModelRemoved ModelStatus = "removed"
	ModelUnknown ModelStatus = "unknown"
)

// ModelInfo 一个可服务的免费模型。
//
// 元数据字段（ContextLength 起）来自 /ai/cline/models，是"尽力补全"而非权威值：
// 免费层的 id 是别名（cline-free/x、y-free），该 feed 里根本不存在这些别名，
// 只能按末段名回连到真实条目（MetaSource 记录连到了谁，便于审计）。连不上就
// 留零值，绝不猜。
type ModelInfo struct {
	ID             string      `json:"id"`
	Name           string      `json:"name,omitempty"`
	Source         string      `json:"source"`
	Provider       string      `json:"provider"`
	Cost           string      `json:"cost"`
	Status         ModelStatus `json:"status"`
	RequiresStream bool        `json:"requiresStream,omitempty"`
	SyncedAt       time.Time   `json:"syncedAt,omitempty"`

	// 元数据（可选，来自 /ai/cline/models 的尽力回连）
	ContextLength            int      `json:"contextLength,omitempty"`
	MaxOutputTokens          int      `json:"maxOutputTokens,omitempty"`
	InputModalities          []string `json:"inputModalities,omitempty"`
	OutputModalities         []string `json:"outputModalities,omitempty"`
	SupportsReasoning        bool     `json:"supportsReasoning,omitempty"`
	SupportsTools            bool     `json:"supportsTools,omitempty"`
	SupportsStructuredOutput bool     `json:"supportsStructuredOutput,omitempty"`
	ReasoningEfforts         []string `json:"reasoningEfforts,omitempty"`
	MetaSource               string   `json:"metaSource,omitempty"`
	MetaAt                   time.Time `json:"metaAt,omitempty"`

	// 自愈：连续失败达到阈值后暂时从列表移除，冷却后自动复活。
	// 上游的免费模型会整批下线（404）或 provider 挂掉（500 stream init），
	// 这些状态在 feed 里看不出来 —— 只能靠实际调用结果判定。
	FailStreak  int       `json:"failStreak,omitempty"`
	CooldownTil time.Time `json:"cooldownTil,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
}

var (
	modelsMu       sync.Mutex
	modelsCache    map[string]*ModelInfo
	modelsSyncing  bool
	modelsLastSync time.Time
)

const (
	modelsRefreshInterval = 60 * time.Second
	modelsSyncTimeout     = 25 * time.Second
)

const recommendedModelsURL = cline.ClineAPIBase + "/ai/cline/recommended-models"

// 免费模型集合**完全由上游 feed 决定**，本地不再维护任何"当前免费模型"清单。
//
// 历史教训（2026-11 连踩三次）：硬编码种子表 ① 2026-09-18 那版整批下线后仍挂在
// 面板上；② 我按当时的 free 组改成 3 个，结果漏掉 catalog 里同样免费的 beam/flint/
// inkling 等十几个；③ 再想把它们塞回种子表，又会漏掉明天新上的模型。
// 一份"当下为免费"的静态清单注定会腐坏，所以彻底删掉：缓存从空开始，只由
// syncRecommendedModels 用 live 数据填充，并按 modelsRefreshInterval 定时刷新。
//
// 冷启动（还没有任何成功同步）时模型列表为空，这是**正确**行为：没有账号就
// 无法确认哪些模型免费，宁可空着也不猜。/v1/models 会返回空列表，面板显示
// 空态，等第一次成功同步后自动填充。唯一的例外是 extraFreeModels ——
// 那些是 ID 明确标 free、但服务端 catalog 根本没收录的条目，属于数据缺口
// 而非"当下快照"，见其定义处。

func initModelsCache() {
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if modelsCache != nil {
		return
	}
	modelsCache = make(map[string]*ModelInfo)
}

func getFreeModels() []*ModelInfo {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	out := make([]*ModelInfo, 0, len(modelsCache))
	for _, m := range modelsCache {
		// 冷却中的模型不出现在列表里：feed 显示它免费，但实测调不通，
		// 列出来只会让客户端选中后报错。冷却到期自动复活（见 recordModelSuccess）。
		if modelInCooldown(m) {
			continue
		}
		cp := *m
		out = append(out, &cp)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			if out[j-1].ID < out[j].ID {
				break
			}
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

type recommendedPayload struct {
	Free []struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	} `json:"free"`
}

// ============ 免费模型自愈 ============
//
// feed 只能说明"这个模型存在且标价免费"，说明不了"现在真的能调通"。实测
// 2026-11-08：19 个标价 0 的模型里有 4 个是坏的 —— gemma ×2 上游 500
// inference failed、laguna 的 provider 整个挂掉、crouton-alpha 直接 404。
// 这些在 feed 里全都正常列着。
//
// 所以用真实调用结果兜底：连续失败 modelFailThreshold 次后进入冷却，冷却
// 期间不出现在列表里；冷却到期自动复活再试。成功一次就清零。
// 只对"模型级"错误计数：429/限流属于配额问题，不是模型坏了，不能因此下架。
const (
	modelFailThreshold = 3
	modelFailCooldown  = 30 * time.Minute
)

// isModelLevelFailure 该错误是否说明"模型本身不可用"（而非配额/网络/鉴权）。
//
// 这里必须"宁可漏判不可错判"：把一个好的模型误下架 30 分钟的代价，远大于
// 让一个坏模型多失败几次。实测踩过的坑：免费模型的上游是 OpenRouter，
// 它会把"上游暂时限流"也包成 500 "inference request failed" —— 但那句
// 错误体的真实内容是 "is temporarily rate-limited upstream, retry shortly"，
// 属于临时限流，跟模型好坏无关。若按 "inference request failed" 一刀切，
// 一个健康的模型会因一阵高峰期流量被永久冷却。
//
// 所以只认两条**确定性**信号：
//   - "model not found"：模型确实已下线（feed 还列着但上游没了）；
//   - "failed to create stream" / stream_initialization_failed：provider 侧
//     彻底不可用（laguna 当时整个上游挂掉）。
//
// 限流、鉴权、超时、5xx 过载一律不算 —— 那些是"等一会儿就好"。
func isModelLevelFailure(status int, body string) bool {
	lb := strings.ToLower(body)
	if strings.Contains(lb, "rate-limited") || strings.Contains(lb, "rate limited") ||
		strings.Contains(lb, "try again") || strings.Contains(lb, "retry shortly") ||
		strings.Contains(lb, "overloaded") || strings.Contains(lb, "timeout") ||
		strings.Contains(lb, "unauthorized") || strings.Contains(lb, "quota") {
		return false
	}
	switch {
	// 形态不止一种，实测两种都出现过：
	//   {"error":"model not found"}
	//   Model 'x-free' not found. If you were using its free tier, that has ended.
	case strings.Contains(lb, "model not found"), strings.Contains(lb, "not found"):
		return true
	case strings.Contains(lb, "free tier, that has ended"),
		strings.Contains(lb, "free tier has ended"):
		return true
	case strings.Contains(lb, "stream_initialization_failed"),
		strings.Contains(lb, "failed to create stream"):
		return true
	case status == http.StatusNotFound:
		return true
	}
	return false
}

// recordModelFailure 记一次模型级失败；达到阈值就进入冷却。
func recordModelFailure(modelID string, status int, body string) {
	if !isModelLevelFailure(status, body) {
		return
	}
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	m, ok := modelsCache[modelID]
	if !ok {
		return
	}
	m.FailStreak++
	m.LastError = truncateErr(body)
	if m.FailStreak >= modelFailThreshold {
		m.CooldownTil = time.Now().Add(modelFailCooldown)
		log.Printf("  model %q marked unavailable after %d failures (%s), cooldown %s",
			modelID, m.FailStreak, m.LastError, modelFailCooldown)
	}
}

// recordModelSuccess 成功一次就清零失败计数并解除冷却。
func recordModelSuccess(modelID string) {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	if m, ok := modelsCache[modelID]; ok && m.FailStreak > 0 {
		m.FailStreak = 0
		m.CooldownTil = time.Time{}
		m.LastError = ""
	}
}

// modelInCooldown 该模型当前是否处于冷却期（nil 安全）。
func modelInCooldown(m *ModelInfo) bool {
	return !m.CooldownTil.IsZero() && time.Now().Before(m.CooldownTil)
}

func truncateErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

// ============ 免费模型元数据补全 ============
//
// /ai/cline/recommended-models 的 free 条目只有 id/name/description/tags，
// 拿不到上下文窗口、输出上限、模态、推理支持度这些客户端关心的信息。
// 完整元数据在 /ai/cline/models（447 条，2026-11 实测约 520KB / 0.5s）。
//
// 关键约束：free 列表里的 id 是别名，/ai/cline/models 里并不存在这些别名。
// 实测（2026-11-08）：
//
//	cline-free/nemotron-3.5-super-vl  → nvidia/nemotron-3.5-super-vl{,:free}
//	cline-free/solar-mini4            → upstage/solar-mini4
//	nvidia/nemotron-3.5-lightning-free → nvidia/nemotron-3.5-lightning{,:free}
//
// 因此按"末段名"回连：先精确匹配整条 id，再匹配去掉 -free 后缀的末段，
// 再按末段唯一命中。歧义（多个候选）时优先非 :free 的付费条目——免费别名
// 的上游就是该模型本身，:free 变体只是同一模型的限流版本，参数上限往往更小，
// 用它反而低估能力。
//
// 全部失败就留零值：宁可缺字段，也不给客户端一个猜出来的错窗口。
const clineModelsMetaURL = cline.ClineAPIBase + "/ai/cline/models"

// clineMetaEntry /ai/cline/models 单条（只取用得上的字段）。
//
// Pricing 是"该模型是否免费"的权威信号：免费条目的 prompt/completion 都是
// 字符串 "0"。注意 Recommended 的 free 组只是人工挑选的一小撮，而 catalog 里
// 标价 0 的聊天模型有十几个（beam-free、flint、inkling、nemotron-*-free…），
// 用 CLI 都能选中并回话 —— 只读 free 组会漏掉一大半免费模型。
type clineMetaEntry struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ContextLen   int    `json:"context_length"`
	Architecture struct {
		Modality        string   `json:"modality"`
		InputModalities []string `json:"input_modalities"`
		OutputModality  []string `json:"output_modalities"`
	} `json:"architecture"`
	SupportedParameters []string `json:"supported_parameters"`
	Pricing             struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
	TopProvider struct {
		ContextLength       int `json:"context_length"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

// extraFreeModels 免费层的第三个来源：ID 明确标 free、但服务端 catalog 里
// 完全没有条目的模型。
//
// 为什么必须硬编码这一小组：`reflection/beam-free` 和 `poolside/laguna-s-2.1-free`
// 在 /ai/cline/models 里**查不到**（不是价格不对，是整个条目缺失），但官方 CLI
// 自带的模型清单（@cline/llms 的 `cline` provider 块）把它们标为 input:0/output:0，
// 且实测在免费账号上 200 通。只靠 catalog 会把它们当成"已下线"修剪掉。
//
// 这组只在"官方两个 feed 都不认识它"时作为兜底；一旦 catalog 收录同名条目，
// catalog 的判据优先（见 syncRecommendedModels 的并集逻辑）。
// 数据来源：@cline/llms dist/models.js 的 cline block（2026-11-08 提取）。
var extraFreeModels = []struct {
	ID              string
	Name            string
	ContextLength   int
	MaxOutputTokens int
}{
	{ID: "reflection/beam-free", Name: "Beam 1.0 (Free)", ContextLength: 1048576, MaxOutputTokens: 131072},
	{ID: "poolside/laguna-s-2.1-free", Name: "Laguna S 2.1 Free", ContextLength: 256000, MaxOutputTokens: 32768},
}

// isZeroPrice 上游把价格写成字符串（"0" / "0.0" / "0.00000008"）。空串表示
// 该字段缺失，不能当成免费——否则任何没填价的条目都会被误判为免费模型。
func isZeroPrice(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == 0
}

// isFreeChatModel catalog 里这条模型是否属于"免费层可直接调用"的集合。
//
// 判据（三条都要满足，全部来自实测 + catalog 事实）：
//   - 输入与输出价格都是 0（prompt 缺失就不算——宁可少收不可误收）；
//   - 输出模态含 text（排除 lyria 这类纯音频生成，它接不了 chat completion）；
//   - 声明支持 tools（免费层的对话模型都支持；不支持的基本是专用生成模型）。
func (e *clineMetaEntry) isFreeChatModel() bool {
	if !isZeroPrice(e.Pricing.Prompt) || !isZeroPrice(e.Pricing.Completion) {
		return false
	}
	hasTextOut := false
	for _, m := range e.Architecture.OutputModality {
		if m == "text" {
			hasTextOut = true
			break
		}
	}
	if !hasTextOut {
		return false
	}
	for _, p := range e.SupportedParameters {
		if p == "tools" {
			return true
		}
	}
	return false
}

// metaCandidateIndex 末段名 -> 该名下的所有完整 id，供别名回连用。
func metaCandidateIndex(entries []clineMetaEntry) map[string][]string {
	idx := make(map[string][]string, len(entries))
	for _, e := range entries {
		seg := e.ID
		if i := strings.LastIndex(seg, "/"); i >= 0 {
			seg = seg[i+1:]
		}
		if i := strings.Index(seg, ":"); i >= 0 {
			seg = seg[:i]
		}
		idx[seg] = append(idx[seg], e.ID)
	}
	return idx
}

// resolveMetaID 把模型 id 回连到 /ai/cline/models 里的条目。
//
// 索引的键是"末段名再去掉 :variant"（见 metaCandidateIndex），所以查询也必须
// 先做同样的归一化：`liquid/lfm-3-1.7b:free` 要先变成 `lfm-3-1.7b` 才能命中。
// 早期实现只剥了 -free 后缀、没剥 :variant，导致所有 :free 条目永远回连不上
// （面板上一整片 ctx/out 空白）。
func resolveMetaID(freeID string, idx map[string][]string) (string, bool) {
	seg := freeID
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	if i := strings.Index(seg, ":"); i >= 0 {
		seg = seg[:i]
	}
	for _, cand := range []string{seg, strings.TrimSuffix(seg, "-free")} {
		ids := idx[cand]
		if len(ids) == 0 {
			continue
		}
		if len(ids) == 1 {
			return ids[0], true
		}
		// 同名多条时优先精确匹配原始 id（它自己就在 catalog 里），
		// 其次非 :free 变体（:free 是限流版，参数上限更小会低估能力）。
		for _, id := range ids {
			if id == freeID {
				return id, true
			}
		}
		for _, id := range ids {
			if !strings.Contains(id, ":free") {
				return id, true
			}
		}
		return ids[0], true
	}
	return "", false
}

// fetchClineMeta 拉取完整模型元数据。失败返回 nil（元数据是可选增强，
// 绝不能让它的失败影响免费列表本身）。
func fetchClineMeta(client *http.Client, token string) []clineMetaEntry {
	req, err := http.NewRequest("GET", clineModelsMetaURL, nil)
	if err != nil {
		return nil
	}
	req.Header = clineHeaders(token, "")
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("  model meta: fetch failed (%v), keeping previous metadata", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("  model meta: HTTP %d, keeping previous metadata", resp.StatusCode)
		return nil
	}
	var payload struct {
		Data []clineMetaEntry `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		log.Printf("  model meta: decode failed (%v), keeping previous metadata", err)
		return nil
	}
	return payload.Data
}

// reasoningEffortsFor 推理档位。feed 只给出"是否支持 reasoning"
// （supported_parameters 里有 reasoning/include_reasoning 之类），不给可接受
// 的档位取值，所以不臆造：实测三个免费模型都接受 minimal/low/medium/high/xhigh
// （2026-11-08，bogus 会被上游拒绝），统一给出这五档并标注来源是实测约定。
func reasoningEffortsFor(entry *clineMetaEntry) []string {
	if entry == nil {
		return nil
	}
	has := false
	for _, p := range entry.SupportedParameters {
		if p == "reasoning" || p == "include_reasoning" || p == "reasoning_effort" {
			has = true
			break
		}
	}
	if !has {
		return nil
	}
	return []string{"minimal", "low", "medium", "high", "xhigh"}
}

// applyClineMetaLocked 把元数据写进缓存条目。调用方必须已持有 modelsMu
// （网络拉取在外面做完，避免持锁发请求）。
// 元数据缺失的条目保留旧值——feed 抖一下不该让面板字段清空。
func applyClineMetaLocked(entries []clineMetaEntry) int {
	if len(entries) == 0 {
		return 0
	}
	byID := make(map[string]*clineMetaEntry, len(entries))
	for i := range entries {
		byID[entries[i].ID] = &entries[i]
	}
	idx := metaCandidateIndex(entries)

	enriched := 0
	for id, m := range modelsCache {
		metaID, ok := resolveMetaID(id, idx)
		if !ok {
			continue
		}
		e := byID[metaID]
		if e == nil {
			continue
		}
		ctx := e.ContextLen
		if e.TopProvider.ContextLength > 0 {
			ctx = e.TopProvider.ContextLength
		}
		m.ContextLength = ctx
		m.MaxOutputTokens = e.TopProvider.MaxCompletionTokens
		m.InputModalities = e.Architecture.InputModalities
		m.OutputModalities = e.Architecture.OutputModality
		has := func(p string) bool {
			for _, x := range e.SupportedParameters {
				if x == p {
					return true
				}
			}
			return false
		}
		m.SupportsTools = has("tools")
		m.SupportsStructuredOutput = has("structured_outputs")
		m.ReasoningEfforts = reasoningEffortsFor(e)
		m.SupportsReasoning = len(m.ReasoningEfforts) > 0
		m.MetaSource = metaID
		m.MetaAt = time.Now()
		if m.Name == "" && e.Name != "" {
			m.Name = e.Name
		}
		enriched++
	}
	return enriched
}

func syncRecommendedModels() (int, error) {
	initModelsCache()

	// 优先活跃账号；全部冷却时退回任意持凭证账号——模型列表与推理配额无关
	// （实测 429 期间该接口仍 200），否则模型列表会长期停留在种子兜底状态
	acc := pickAccount()
	if acc == nil {
		acc = pickAccountAny()
	}
	if acc == nil {
		return 0, fmt.Errorf("no accounts")
	}
	token, err := ensureAccountToken(acc)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequest("GET", recommendedModelsURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header = clineHeaders(token, "")
	req.Header.Set("X-Task-ID", fmt.Sprintf("sess_sync_%d", time.Now().UnixMilli()))

	client := &http.Client{Timeout: modelsSyncTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var payload recommendedPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}

	// 元数据拉取放在加锁之前：/ai/cline/models 约 520KB，不能持锁等网络。
	// 失败返回 nil，本轮只是不补全，不影响免费列表本身。
	meta := fetchClineMeta(client, token)

	modelsMu.Lock()
	defer modelsMu.Unlock()

	// 免费模型来自两个来源，必须取并集：
	//   1. recommended-models 的 free 组 —— 官方"推荐"的一小撮（2026-11 只有 3 个）；
	//   2. /ai/cline/models 里标价 0 的对话模型 —— 真正的免费全集（beam-free、
	//      flint、inkling、nemotron-*-free 等，CLI 里都能选中并正常回话）。
	// 只读来源 1 会漏掉大半免费模型（用户实测在 CLI 里选 Beam 1.0 (Free) 能聊）。
	live := make(map[string]bool, len(payload.Free)+16)
	added := 0

	upsert := func(id, name string) {
		if id == "" {
			return
		}
		live[id] = true
		provider := id
		if i := indexByte(id, '/'); i >= 0 {
			provider = id[:i]
		}
		if cached, ok := modelsCache[id]; ok {
			cached.Source = "free"
			cached.Cost = "free"
			cached.Provider = provider
			cached.Status = ModelActive
			cached.SyncedAt = time.Now()
			if cached.Name == "" {
				cached.Name = name
			}
			return
		}
		modelsCache[id] = &ModelInfo{
			ID:             id,
			Name:           name,
			Source:         "free",
			Provider:       provider,
			Cost:           "free",
			Status:         ModelActive,
			RequiresStream: indexByte(id, ':') < 0,
			SyncedAt:       time.Now(),
		}
		added++
	}

	for _, m := range payload.Free {
		upsert(m.ID, m.Name)
	}
	freeFromCatalog := 0
	for i := range meta {
		if meta[i].isFreeChatModel() {
			upsert(meta[i].ID, meta[i].Name)
			freeFromCatalog++
		}
	}
	if freeFromCatalog > 0 {
		log.Printf("model sync: %d zero-priced model(s) discovered in catalog", freeFromCatalog)
	}
	// 第三来源：ID 标 free 但服务端 catalog 完全没收录的条目（见 extraFreeModels）。
	// 只有当 catalog 里确实不存在同 id 时才补，避免覆盖 catalog 的权威数据。
	known := make(map[string]bool, len(meta))
	for i := range meta {
		known[meta[i].ID] = true
	}
	for _, e := range extraFreeModels {
		if known[e.ID] {
			continue
		}
		upsert(e.ID, e.Name)
		// 元数据直接取自 CLI 自带清单（catalog 里没有这些条目，回连必然失败）
		if m := modelsCache[e.ID]; m != nil {
			m.ContextLength = e.ContextLength
			m.MaxOutputTokens = e.MaxOutputTokens
			m.SupportsTools = true
			m.SupportsReasoning = true
			m.MetaSource = "cline-cli catalog"
			m.MetaAt = time.Now()
		}
	}

	pruned := pruneModelsLocked(live, meta != nil)
	if pruned > 0 {
		log.Printf("model sync: pruned %d model(s) no longer on official feed", pruned)
	}

	if enriched := applyClineMetaLocked(meta); enriched > 0 {
		log.Printf("model sync: metadata resolved for %d/%d model(s)", enriched, len(live))
	}

	modelsLastSync = time.Now()
	return added, nil
}

// pruneModelsLocked 修剪已从官方 feed 下线的模型。调用方必须已持有 modelsMu。
//
// 防御：feed 短暂为空时不清空本地列表。旧实现用"live 数量不足现有模型一半
// 就跳过"的比例判断，这在 free 列表大幅收缩时会把修剪永久禁用——2026-11 官方
// free 列表从 5 降到 3，缓存里却还挂着 8 个（含 5 个早已 404 的死模型），
// 3*2 < 8 每轮都跳过，死模型就一直留在面板上。
//
// catalogOK 说明本轮的"免费全集"来源是否成功。live 是
// recommended 的 free 组 **并上** catalog 里标价 0 的模型；catalog 拉取失败时
// fetchClineMeta 返回 nil，live 里就只剩推荐的那 3 个 —— 此时 live 非空，
// 但它是**残缺**的。若照旧按它修剪，一次 520KB 接口抖动就会删掉十几个
// beam-free/flint/inkling 这类只从 catalog 发现的模型，下一轮再以
// FailStreak=0 重建，自愈计数被静默清零。所以残缺 feed 只增不删。
func pruneModelsLocked(live map[string]bool, catalogOK bool) int {
	if len(live) == 0 {
		if len(modelsCache) > 0 {
			log.Printf("model sync: empty live feed (%d cached), skipping prune", len(modelsCache))
		}
		return 0
	}
	if !catalogOK && len(modelsCache) > 0 {
		log.Printf("model sync: catalog fetch failed, keeping %d cached model(s) (partial feed: %d)",
			len(modelsCache), len(live))
		return 0
	}
	// extraFreeModels 的 id 天生不在 live feed 里（它们是 catalog 的数据缺口），
	// 不能按"不在 live 就删"处理 —— 否则每轮同步都会删掉再重建，连带把自愈
	// 累积的 failStreak 清零，健康的坏模型永远达不到下架阈值。
	keep := make(map[string]bool, len(extraFreeModels))
	for _, e := range extraFreeModels {
		keep[e.ID] = true
	}
	pruned := 0
	for id := range modelsCache {
		if !live[id] && !keep[id] {
			delete(modelsCache, id)
			pruned++
		}
	}
	return pruned
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func syncModelsOnce() {
	initModelsCache()
	modelsMu.Lock()
	if modelsSyncing {
		modelsMu.Unlock()
		return
	}
	modelsSyncing = true
	modelsMu.Unlock()
	defer func() {
		modelsMu.Lock()
		modelsSyncing = false
		modelsMu.Unlock()
	}()

	added, err := syncRecommendedModels()
	if err != nil {
		log.Printf("  model sync: failed (%v), using cached list", err)
		return
	}
	if added > 0 {
		log.Printf("  model sync: %d new free models from official feed", added)
	} else {
		log.Printf("  model sync: %d free models up to date", len(getFreeModels()))
	}
}

// getDefaultModel 默认模型：面板里设置的偏好（pool.DefaultModel）优先，
// 其次取 live 列表里排序最小的可用模型（确定性 —— 之前是 map 随机序，
// 修剪掉偏好模型后会随机漂移到别的模型，客户端看到的行为不可预期）。
func getDefaultModel() string {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()

	if m, ok := modelsCache[defaultModel]; ok && m.Status == ModelActive && !modelInCooldown(m) {
		return defaultModel
	}
	best := ""
	for id, m := range modelsCache {
		if m.Status != ModelActive || modelInCooldown(m) {
			continue
		}
		if best == "" || id < best {
			best = id
		}
	}
	if best != "" {
		return best
	}
	return defaultModel
}

// lookupFreeModel 在缓存里查模型，并且**把冷却中的模型视为不可用**。
// 调用方必须已持有 modelsMu。
//
// 为什么冷却必须在解析路径生效、而不只是在列表路径：IDE 客户端会把模型名
// 写进配置并发起长期流量，它从来不看 /v1/models。只从列表里隐藏，等于对
// 这类客户端毫无作用——请求照旧打到死模型，每次失败又把 CooldownTil 续期
// 30 分钟，冷却在持续流量下永远不会到期。冷却必须在"选模型"这一步生效，
// 死模型才会真正被绕开、计时才有机会走完。
func lookupFreeModel(id string) (*ModelInfo, bool) {
	m, ok := modelsCache[id]
	if !ok || modelInCooldown(m) {
		return nil, false
	}
	return m, true
}

func normalizeRequestModel(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return getDefaultModel()
	}
	initModelsCache()
	modelsMu.Lock()
	_, ok := lookupFreeModel(id)
	modelsMu.Unlock()
	if ok {
		return id
	}
	log.Printf("  model %q not in free list (or cooling down), fallback to %q", id, getDefaultModel())
	return getDefaultModel()
}

func modelInFreeList(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return true
	}
	initModelsCache()
	modelsMu.Lock()
	_, ok := lookupFreeModel(id)
	modelsMu.Unlock()
	return ok
}

// strictModelGate STRICT_MODEL_MATCH 开启时，对既不在 cline 免费模型表、
// 也不是 zen 模型的名字返回错误提示；空串表示放行。zen 模型必须放行：
// zen 故障转移时会把 zen 免费模型路由到 cline 池，由 normalizeRequestModel
// 兜底为默认模型 —— 这是故障转移的既有机制。combo 别名由调用方先改写。
func strictModelGate(model string) string {
	if !StrictModelMatchEnv() || modelInFreeList(model) {
		return ""
	}
	initZenModels()
	if _, ok := resolveZenModel(model); ok {
		return ""
	}
	return fmt.Sprintf("model %q is not available on this gateway (see /v1/models for the model list). "+
		"Set STRICT_MODEL_MATCH=false to fall back to the default model instead", model)
}

func apiModelList() []map[string]any {
	// 先取快照再装箱：getFreeModels 内部持锁，但容量提示不能直接读
	// len(modelsCache)——那是无锁读 map 头，与 60s 刷新器并发时是 data race
	//（/v1/models 是客户端热路径，每个请求都会走到这里）。
	free := getFreeModels()
	out := make([]map[string]any, 0, len(free))
	for _, m := range free {
		item := map[string]any{
			"id":             m.ID,
			"object":         "model",
			"created":        time.Now().UnixMilli(),
			"owned_by":       m.Provider,
			"source":         m.Source,
			"status":         m.Status,
			"cost":           m.Cost,
			"requiresStream": m.RequiresStream,
			"syncedAt":       m.SyncedAt,
		}
		// 元数据是"尽力补全"的：拿不到就不出现这些键，别给客户端假信息。
		if m.Name != "" {
			item["name"] = m.Name
		}
		if m.ContextLength > 0 {
			item["context_length"] = m.ContextLength
		}
		if m.MaxOutputTokens > 0 {
			item["max_completion_tokens"] = m.MaxOutputTokens
			item["max_output_tokens"] = m.MaxOutputTokens
		}
		if len(m.InputModalities) > 0 || len(m.OutputModalities) > 0 {
			arch := map[string]any{}
			if len(m.InputModalities) > 0 {
				arch["input_modalities"] = m.InputModalities
			}
			if len(m.OutputModalities) > 0 {
				arch["output_modalities"] = m.OutputModalities
			}
			item["architecture"] = arch
		}
		if m.SupportsReasoning || len(m.ReasoningEfforts) > 0 {
			item["supports_reasoning"] = m.SupportsReasoning
			if len(m.ReasoningEfforts) > 0 {
				item["reasoning_efforts"] = m.ReasoningEfforts
			}
		}
		if m.SupportsTools {
			item["supports_tools"] = true
		}
		if m.SupportsStructuredOutput {
			item["supports_structured_output"] = true
		}
		if m.MetaSource != "" {
			item["metadata_source"] = m.MetaSource
		}
		out = append(out, item)
	}
	return out
}

// modelsSyncSnapshot 无锁安全地取同步状态。modelsLastSync 是 time.Time（三个
// 字），syncRecommendedModels 持锁写它；admin 面板直接读字段会拿到撕裂值，
// 也会被 -race 判为 data race。
func modelsSyncSnapshot() (lastSync time.Time, syncing bool) {
	initModelsCache()
	modelsMu.Lock()
	defer modelsMu.Unlock()
	return modelsLastSync, modelsSyncing
}

func ensureModelsFresh() {
	initModelsCache()
	modelsMu.Lock()
	needSync := modelsLastSync.IsZero() || time.Since(modelsLastSync) > modelsRefreshInterval
	syncing := modelsSyncing
	modelsMu.Unlock()
	if needSync && !syncing {
		go syncModelsOnce()
	}
}

func startModelsRefresher() {
	go func() {
		syncModelsOnce()
		ticker := time.NewTicker(modelsRefreshInterval)
		for range ticker.C {
			syncModelsOnce()
		}
	}()
}
