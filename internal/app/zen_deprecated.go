package app

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============ 已弃用模型的自动迁移（410 replacement + 400 unavailable） ============
//
// 上游证据（2026-10-09 实测）：
//   - mimo-v2.5-free → 410，body 带 {"replacement":"mimo-v2.6-flash-free"}；
//   - qwen3.6-plus-free → 400 "Model is unavailable"。
// 前者：把请求透明改走继任模型（单次重试），别名持久化到
// DATA_DIR/.zen-model-aliases.json，重启后依然生效；后者：标记"死亡到下次
// 目录同步"，路由层直接拒绝，不再每次白烧一个上游请求。
//
// 别名只做"原始 ID 解析失败后"的兜底：真实模型永远优先于别名，继任模型
// 上线/下线不会被别名表遮蔽。

type zenDepEntry struct {
	Replacement string `json:"replacement"`
	Since       int64  `json:"since"` // unix 秒，迁移发生时间
}

var (
	zenDepMu      sync.Mutex
	zenDepAliases = map[string]*zenDepEntry{} // 已弃用 ID -> 继任模型
	zenDepLoaded  bool
	zenDepPath    string
)

// zenDepFile 别名持久化路径（与 .zen-sessions.json / .zen-endpoints.json 同目录）。
func zenDepFile() string {
	zenDepMu.Lock()
	defer zenDepMu.Unlock()
	return zenDepPathLocked()
}

// zenDepPathLocked 路径解析（调用方须持有 zenDepMu）。load/save 都在锁内
// 走这里，避免 zenDepFile 的重入死锁。
func zenDepPathLocked() string {
	if zenDepPath == "" {
		zenDepPath = kit.ResolveDataPath(".zen-model-aliases.json")
	}
	return zenDepPath
}

func loadZenDepsLocked() {
	if zenDepLoaded {
		return
	}
	zenDepLoaded = true
	data, err := os.ReadFile(zenDepPathLocked())
	if err != nil || len(data) == 0 {
		return
	}
	var m map[string]*zenDepEntry
	if err := json.Unmarshal(data, &m); err != nil {
		// 坏文件不夸大其词：别名只是优化层，丢弃后走原有 410 重学路径。
		p := zenDepPathLocked()
		_ = os.Rename(p, p+".corrupt")
		log.Printf("zen model aliases parse failed, ignoring (backup: %s.corrupt): %v", p, err)
		return
	}
	for k, e := range m {
		if e != nil && e.Replacement != "" {
			zenDepAliases[k] = e
		}
	}
	if len(zenDepAliases) > 0 {
		log.Printf("zen model aliases loaded: %d mapping(s)", len(zenDepAliases))
	}
}

// saveZenDepsLocked 持久化别名表（调用方持有 zenDepMu）。tmp+rename 原子写，
// 与 sessions/endpoints 同一手法。
func saveZenDepsLocked() {
	data, err := json.MarshalIndent(zenDepAliases, "", "  ")
	if err != nil {
		return
	}
	tmp := zenDepPathLocked() + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("zen model aliases save failed: %v", err)
		return
	}
	if err := os.Rename(tmp, zenDepPathLocked()); err != nil {
		log.Printf("zen model aliases save failed (rename): %v", err)
	}
}

// zenDeprecatedReplacement 返回已弃用模型的继任者（无映射时返回 ""）。
// 键统一规范化（小写去 opencode/ 前缀），与 zenMarkModelDead 同口径。
func zenDeprecatedReplacement(id string) string {
	id = normalizeZenModelID(id)
	zenDepMu.Lock()
	defer zenDepMu.Unlock()
	loadZenDepsLocked()
	if e := zenDepAliases[id]; e != nil {
		return e.Replacement
	}
	return ""
}

// zenRecordDeprecation 记录并持久化一条 410 迁移映射；继任者已弃用时返回 ""
// （调用方放弃迁移）。别名链不允许成环/成串：继任者若本身也是别名，返回 ""
// 放弃——一次请求最多迁移一跳，防止 A→B→C 无限重试。
// 两侧 ID 均规范化为小写去前缀，保证与查询侧（zenDeprecatedReplacement）
// 同键；否则 chat 路径传入 "opencode/x" 会写进去、按 "x" 查却查不到。
func zenRecordDeprecation(deadID, replacement string) string {
	deadID = normalizeZenModelID(deadID)
	replacement = normalizeZenModelID(replacement)
	if replacement == "" || replacement == deadID {
		return ""
	}
	zenDepMu.Lock()
	defer zenDepMu.Unlock()
	loadZenDepsLocked()
	if e := zenDepAliases[replacement]; e != nil {
		log.Printf("zen deprecation: replacement %q is itself deprecated (-> %s); refusing to chain", replacement, e.Replacement)
		return ""
	}
	if e := zenDepAliases[deadID]; e != nil && e.Replacement == replacement {
		return replacement // 已知映射，幂等
	}
	zenDepAliases[deadID] = &zenDepEntry{Replacement: replacement, Since: time.Now().Unix()}
	saveZenDepsLocked()
	log.Printf("zen deprecation: %s -> %s (persisted)", deadID, replacement)
	return replacement
}

// zenModelDeadUntilSync 400 "Model is unavailable" 的死亡标记。每次成功目录
// 同步清空（syncZenModels 用 zenClearDeadModels）；另有 zenDeadTTL 兜底——
// 目录同步可能长期失败（registry 不可达），只靠同步清空会让模型被永久钉死。
var zenDeadModels = map[string]int64{} // model id -> 标记时间(unix 秒)

// zenDeadTTL 死亡标记最长存活时长。超过后自动失效并允许再次探测上游，
// 避免同步长期失败时一个瞬时 400 把模型永久下线。
const zenDeadTTL = time.Hour

// zenMarkModelDead 标记模型死亡（记下时间，供 TTL 判定）。键规范化，
// 与查询侧同口径。
func zenMarkModelDead(id string) {
	id = normalizeZenModelID(id)
	if id == "" {
		return
	}
	zenDepMu.Lock()
	zenDeadModels[id] = time.Now().Unix()
	zenDepMu.Unlock()
}

// zenModelDead 模型是否仍在死亡 TTL 内（路由层据此跳过，不再白烧请求）。
// 过期条目就地删除并返回 false，无需等待目录同步。
func zenModelDead(id string) bool {
	id = normalizeZenModelID(id)
	if id == "" {
		return false
	}
	zenDepMu.Lock()
	defer zenDepMu.Unlock()
	since, ok := zenDeadModels[id]
	if !ok {
		return false
	}
	if time.Now().Unix()-since > int64(zenDeadTTL/time.Second) {
		delete(zenDeadModels, id)
		return false
	}
	return true
}

// zenClearDeadModels 目录同步成功后清空死亡标记（上游列表已刷新，
// 之前的 400 可能只是目录滞后）。
func zenClearDeadModels() {
	zenDepMu.Lock()
	zenDeadModels = map[string]int64{}
	zenDepMu.Unlock()
}

// zenHandleDeprecatedModel 上游返回 410/400 后的弃用处理。返回 true 表示
// 已完成迁移，调用方应就地重试（params["model"] 已改写为继任模型）。
// pinned（面板探测）与已迁移过一次的请求不迁移——探测结论只属于这次点击，
// 迁移标记防止别名链无限重试。400 "unavailable" 形态永远返回 false（只标记）。
func zenHandleDeprecatedModel(status int, body, modelID string, params map[string]any, pinned, alreadyRemapped bool) bool {
	repl := zenDeprecationFromError(status, body)
	if status == http.StatusBadRequest {
		// 400 + "Model is unavailable"：标记死亡，本次请求照常失败（该模型
		// 没有可用继任信号），路由层后续直接拒绝。
		if repl == "" && isModelUnavailableBody(body) {
			if !pinned {
				zenMarkModelDead(modelID)
				log.Printf("zen model unavailable: %s marked dead until next catalog sync (upstream 400)", modelID)
			}
		}
		return false
	}
	// status == 410
	if pinned || alreadyRemapped || repl == "" {
		return false
	}
	// 防环：继任者本身已是弃用别名时拒绝链式迁移。
	if zenDeprecatedReplacement(repl) != "" {
		log.Printf("zen deprecation: %s -> %s refused (replacement is itself deprecated)", modelID, repl)
		return false
	}
	if _, ok := resolveZenFreeModel(repl); !ok {
		log.Printf("zen deprecation: %s -> %s refused (replacement unknown or not free)", modelID, repl)
		return false
	}
	if zenRecordDeprecation(modelID, repl) == "" {
		return false
	}
	params["model"] = repl
	return true
}

// isModelUnavailableBody 上游 400 错误体是否呈"模型不可用"特征
// （实测 qwen3.6-plus-free：400 "Model is unavailable"；另容忍 ModelDeprecated）。
func isModelUnavailableBody(body string) bool {
	low := strings.ToLower(body)
	return strings.Contains(low, "model is unavailable") || strings.Contains(low, "modeldeprecated")
}

// pruneZenDeadAndAliases 配置/目录变更后清理指向不存在模型的别名。
// 死亡标记整体清空（调用点在同步成功后）。
func pruneZenDeadAndAliases(valid map[string]bool) {
	zenDepMu.Lock()
	defer zenDepMu.Unlock()
	n := 0
	for k := range zenDeadModels {
		if !valid[k] {
			delete(zenDeadModels, k)
			n++
		}
	}
	for k, e := range zenDepAliases {
		if !valid[k] && !valid[e.Replacement] {
			delete(zenDepAliases, k)
			n++
		}
	}
	if n > 0 {
		saveZenDepsLocked()
	}
}
