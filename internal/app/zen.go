package app

import (
	"bufio"
	"bytes"
	"cline-go-proxy/internal/kit"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ZenModel opencode zen 免费模型定义
// ZenModel zen 模型条目。
//
// 并发约定（重要）：写者（initZenModels / applyZenCatalog / learnZenEndpoint /
// loadZenEndpoints）一律在 zenModelsMu 下"改副本再挂回"，绝不就地改写已发布的
// 结构体。因此 resolveZenModel 返回的指针在锁外可安全读取任意字段——请求路径
// 正是在锁外读 Upstream/Source/Context。新增写者必须遵守 copy-on-write，否则
// 就是与请求路径的数据竞争。
type ZenModel struct {
	ID        string   `json:"id"`
	Aliases   []string `json:"aliases,omitempty"`
	Context   int      `json:"context"`
	Output    int      `json:"output"`
	Source    string   `json:"source"`               // seed=内置 / registry=公共目录同步 / synced=zen 上游同步
	Upstream  string   `json:"upstream,omitempty"`   // 强制原生上游端点: "responses"；空=默认 chat/completions
	ToolCall  bool     `json:"toolCall,omitempty"`   // 目录声明的 tool_call 能力
	Reasoning bool     `json:"reasoning,omitempty"`  // 目录声明的 reasoning 能力
	Attach    bool     `json:"attachment,omitempty"` // 目录声明的图片/附件输入能力
}

// zenSeedModels 冷启动兜底列表：仅在"首次成功同步之前 + 目录不可达"时作为
// 可服务的模型集（离线也能用）。它不再是 ID 的来源——运行时模型表由
// syncZenModels 从 live 目录按价格门推导，种子条目若通不过价格门会被移除。
// 种子里保留的别名（mimo/ling/nemotron/pickle…）与 muse-spark 的
// Upstream=responses 提示会随同名 live 条目沿用。
// 免费资格 = 公共目录 cost.input==0 && cost.output==0（big-pickle 这类无
// "free" 后缀的免费模型也因此入选；deepseek-v4-flash 这类 status=deprecated
// 的条目即使 cost 为 0 也不入选）。
var zenSeedModels = []ZenModel{
	{ID: "mimo-v2.5-free", Aliases: []string{"mimo-v2.5", "mimo"}, Context: 200000, Output: 32000, Source: "seed"},
	{ID: "nemotron-3-ultra-free", Aliases: []string{"nemotron-3-ultra", "nemotron"}, Context: 1000000, Output: 128000, Source: "seed"},
	{ID: "nemotron-3.5-lightning-free", Aliases: []string{"nemotron-3.5-lightning", "nemotron-lightning"}, Context: 262144, Output: 262144, Source: "seed"},
	{ID: "ling-3.0-flash-fin-free", Aliases: []string{"ling-3.0-flash-fin", "ling-fin", "ling"}, Context: 262144, Output: 32768, Source: "seed"},
	// big-pickle：zen 创始免费模型（无 free 后缀，目录 cost 0/0，1.18.31 实测可用；
	// opencode zen 免费层的默认别名，永久保留在种子中）
	{ID: "big-pickle", Aliases: []string{"pickle"}, Context: 200000, Output: 32000, Source: "seed"},
	// muse-spark 只在原生 /v1/responses 端点上可用：官方 opencode CLI 实测
	// 对该模型只发 POST /zen/v1/responses（带 tools + reason、返回 SSE），
	// chat/completions 上该模型 500（需经 responses 原生调用再转回 chat 形态）
	{ID: "muse-spark-1.3-contributor-free", Aliases: []string{"muse-spark-contributor"}, Context: 1048576, Output: 131072, Source: "seed", Upstream: "responses"},
	{ID: "muse-spark-1.2-contributor-free", Aliases: []string{"muse-spark"}, Context: 1048576, Output: 131072, Source: "seed", Upstream: "responses"},
}

var (
	zenModelsMu sync.RWMutex
	zenModels   = make(map[string]*ZenModel) // 主表:ID
	zenAliases  = make(map[string]*ZenModel) // 别名表
)

const zenAPIBase = "https://opencode.ai/zen/v1"

func initZenModels() {
	zenModelsMu.Lock()
	defer zenModelsMu.Unlock()
	if len(zenModels) > 0 {
		return
	}
	for _, m := range zenSeedModels {
		cp := m
		zenModels[cp.ID] = &cp
		for _, a := range cp.Aliases {
			zenAliases[a] = &cp
		}
	}
}

// normalizeZenModelID 规范化 zen 模型 ID 用于表查找：trim + 小写 + 去掉
// "opencode/" 前缀。zen 模型 ID（种子与官方目录 id）均为小写；客户端可能发
// "Big-Pickle" 或 "opencode/big-pickle"，不规范化的查找会落空，进而把请求
// 错误路由到 cline 池（或让 dead/alias 标记不可达）。
func normalizeZenModelID(id string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(id)), "opencode/")
}

// resolveZenModel 解析模型名到 zen 模型。支持 "opencode/<id>" 前缀与别名（大小写不敏感）。
// 别名优先: 同步来的付费同名模型(如 deepseek-v4-flash)不会覆盖 free 别名解析。
// 410 迁移别名是最后兜底：原始 ID 不存在时返回继任模型（真实模型永远优先，
// 继任者上线不会被弃用别名遮蔽）。
func resolveZenModel(id string) (*ZenModel, bool) {
	id = normalizeZenModelID(id)
	if id == "" {
		return nil, false
	}
	zenModelsMu.RLock()
	if m, ok := zenAliases[id]; ok {
		zenModelsMu.RUnlock()
		return m, true
	}
	if m, ok := zenModels[id]; ok {
		zenModelsMu.RUnlock()
		return m, true
	}
	zenModelsMu.RUnlock()
	if repl := zenDeprecatedReplacement(id); repl != "" {
		return resolveZenModel(repl)
	}
	return nil, false
}

// isZenFreeModel 免费判定: seed 白名单、live 同步（已过价格门）、
// learned（学习器改过端点但仍是同一批已入选免费模型）、或通用的 -free 后缀。
// big-pickle/union-alpha 无 free 后缀，靠 Source=="live"/"learned" 入选。
func isZenFreeModel(m *ZenModel) bool {
	if m == nil {
		return false
	}
	if m.Source == "seed" || m.Source == "live" || m.Source == "learned" {
		return true
	}
	return strings.HasSuffix(m.ID, "-free")
}

// zenProbeModel 管理面板 per-key "Test" 探测用的模型，按优先级：
//  1. big-pickle —— zen 免费层的默认别名（mint 收割的首选同款），最稳；
//  2. Source=="live" 的最小 id 模型 —— live 条目来自官方目录同步，
//     是"上游当前确实在供"的证明，种子条目可能早已下架；
//  3. 任意 free 模型的最小 id —— 冷启动且同步不可达时的纯种子兜底。
// native-responses 模型也可能被选中——testZenKey 按其 Upstream 字段走对应
// 上游调用，无需特判。目录为空时返回 nil，探测直接报错。
func zenProbeModel() *ZenModel {
	// 目录表是惰性填充的（面板打开模型页 / 定时同步 / 首个请求才会触发）：
	// 探测必须自给自足，进程刚启动、谁都没碰过模型页时也要能测。
	initZenModels()
	zenModelsMu.RLock()
	defer zenModelsMu.RUnlock()
	if m, ok := zenModels["big-pickle"]; ok && isZenFreeModel(m) {
		return m
	}
	var best *ZenModel
	for _, m := range zenModels {
		if !isZenFreeModel(m) || (m.Source != "live" && m.Source != "learned") {
			continue
		}
		if best == nil || m.ID < best.ID {
			best = m
		}
	}
	if best != nil {
		return best
	}
	for _, m := range zenModels {
		if !isZenFreeModel(m) {
			continue
		}
		if best == nil || m.ID < best.ID {
			best = m
		}
	}
	return best
}

// resolveZenFreeModel 只解析免费 zen 模型
func resolveZenFreeModel(id string) (*ZenModel, bool) {
	m, ok := resolveZenModel(id)
	if !ok || !isZenFreeModel(m) {
		return nil, false
	}
	return m, true
}

// routeModel 决定请求走哪个上游: "zen" / "cline" / "reject"
// zen 免费模型 -> zen; zen 付费模型 -> reject(400); 其他 -> cline
// 故障转移: zen 连续失败期间,zen 免费模型请求临时路由到 cline 账号池
func routeModel(id string) string {
	id = strings.TrimSpace(id)
	// combo 别名兜底：调用方通常已把 model 改写为 target，这里防止 combo ID
	// 直接进入路由（按 combo 声明的平台走，不解析为普通模型）
	if c := resolveCombo(id); c != nil {
		return c.Platform
	}
	// zen 表查找统一用小写去前缀的规范 ID：dead 标记与 410 别名都以规范 ID
	// 为键，客户端发 "Big-Pickle" / "opencode/big-pickle" 时不做归一就查不到，
	// 请求会误落到 cline 池（或让标记形同虚设）。
	zenID := normalizeZenModelID(id)
	initZenModels()
	cfg := getZenConfig()
	// 上游 400 "Model is unavailable" 的死亡标记：直接拒绝，不再每次请求
	// 都白烧一个上游往返。下次目录同步成功（或 TTL 过期）后自动解除。
	// 例外：该 id 若已有 410 继任别名，别名优先——迁移后请求仍有真实去处，
	// 不该被一条陈旧的 400 标记挡住。
	if zenModelDead(zenID) && zenDeprecatedReplacement(zenID) == "" {
		log.Printf("  route: %q marked unavailable by upstream, rejecting until next catalog sync", zenID)
		return "reject"
	}
	if zm, ok := resolveZenModel(zenID); ok {
		if isZenFreeModel(zm) {
			// 与 cline 模型表冲突时(几乎不可能)走 cline
			initModelsCache()
			modelsMu.Lock()
			_, inCline := modelsCache[zenID]
			modelsMu.Unlock()
			if !inCline {
				if cfg.Failover && zenFailedNow() {
					log.Printf("  failover: zen degraded, %q routed to cline pool", zenID)
					return "cline"
				}
				return "zen"
			}
		} else {
			return "reject"
		}
	}
	return "cline"
}

// ============ zen 配置 ============

type zenCompactConfig struct {
	Auto         bool   `json:"auto"`         // 官方风格摘要压缩开关
	Buffer       int    `json:"buffer"`       // 预留输出缓冲 token,默认 20000
	KeepTokens   int    `json:"keepTokens"`   // 尾部保留 token 预算,默认 8000
	SummaryModel string `json:"summaryModel"` // 摘要模型,空=用请求模型
	MaxSummary   int    `json:"maxSummary"`   // 摘要最大输出 token,默认 4096
}

type zenConfigData struct {
	Enabled         bool             `json:"enabled"`
	Key             string           `json:"key"`            // 兼容字段：始终等于 Keys[0]（旧版单 key 读取用）
	Keys            []string         `json:"keys,omitempty"` // zen 多 key 池，请求按 round-robin 轮转
	BaseURL         string           `json:"baseURL"`
	Proxies         []string         `json:"proxies"`         // http(s)/socks5 代理,轮询出口
	ProxyStrategy   string           `json:"proxyStrategy"`   // round_robin / random / fill
	MaxConcurrency  int              `json:"maxConcurrency"`  // zen 上游最大并发,防 worker 瞬时超限,默认 8
	Retries         int              `json:"retries"`         // 限流/网络错误重试次数,默认 3
	Failover        bool             `json:"failover"`        // zen 连续失败后故障转移到 cline 账号池,默认 true
	FailoverCount   int              `json:"failoverCount"`   // 触发故障转移的连续失败次数,默认 3
	FailoverMinutes int              `json:"failoverMinutes"` // 故障转移窗口(分钟),默认 5
	Compaction      zenCompactConfig `json:"compaction"`
}

func defaultZenConfig() *zenConfigData {
	return &zenConfigData{
		Enabled:         true,
		Key:             "public",
		Keys:            []string{"public"},
		BaseURL:         zenAPIBase,
		ProxyStrategy:   "round_robin",
		MaxConcurrency:  8,
		Retries:         3,
		Failover:        true,
		FailoverCount:   3,
		FailoverMinutes: 5,
		Compaction: zenCompactConfig{
			Auto:       true,
			Buffer:     20000,
			KeepTokens: 8000,
			MaxSummary: 4096,
		},
	}
}

var (
	zenConfig   = loadZenConfig()
	zenConfigMu sync.Mutex
)

// ============ 限流防御状态机 ============

var (
	zenSem       chan struct{} // 并发信号量
	zenFailCount int           // 连续失败计数
	zenFailUntil time.Time     // 故障转移截止时间
	zenStateMu   sync.Mutex
)

func init() {
	rebuildZenSem()
}

func rebuildZenSem() {
	cfg := getZenConfig()
	n := cfg.MaxConcurrency
	if n <= 0 {
		n = 8
	}
	zenStateMu.Lock()
	zenSem = make(chan struct{}, n)
	zenStateMu.Unlock()
}

func markZenSuccess() {
	zenStateMu.Lock()
	zenFailCount = 0
	zenFailUntil = time.Time{}
	zenStateMu.Unlock()
}

func markZenFail() {
	cfg := getZenConfig()
	thr := cfg.FailoverCount
	if thr <= 0 {
		thr = 3
	}
	window := cfg.FailoverMinutes
	if window <= 0 {
		window = 5
	}
	zenStateMu.Lock()
	zenFailCount++
	if zenFailCount >= thr {
		zenFailUntil = time.Now().Add(time.Duration(window) * time.Minute)
	}
	zenStateMu.Unlock()
}

// zenFailedNow zen 是否处于故障转移状态
func zenFailedNow() bool {
	zenStateMu.Lock()
	defer zenStateMu.Unlock()
	if zenFailUntil.IsZero() {
		return false
	}
	if time.Now().After(zenFailUntil) {
		zenFailCount = 0
		zenFailUntil = time.Time{}
		return false
	}
	return true
}

// isRateLimited 限流信号识别: 429/503 直接命中; 502/403 按错误体关键词。
// 关键词含配额耗尽类措辞——它们与限流同样应触发冷却/换 key，而不是当作
// 普通错误立刻回给客户端。
func isRateLimited(status int, body string) bool {
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		return true
	}
	if status == http.StatusBadGateway || status == http.StatusForbidden {
		low := strings.ToLower(body)
		for _, kw := range []string{
			"resourceexhausted", "limit reached", "rate limit", "too many", "overloaded", "busy",
			"quota", "exceeded", "insufficient",
		} {
			if strings.Contains(low, kw) {
				return true
			}
		}
	}
	return false
}

// normalizeZenKeys 规范 key 池：迁移旧单 key 字段、去空去重、回退默认 "public"、
// 同步兼容字段 Key = Keys[0]。
func normalizeZenKeys(cfg *zenConfigData) {
	if len(cfg.Keys) == 0 && cfg.Key != "" {
		cfg.Keys = []string{cfg.Key}
	}
	cleaned := make([]string, 0, len(cfg.Keys))
	seen := map[string]bool{}
	for _, k := range cfg.Keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		cleaned = append(cleaned, k)
	}
	if len(cleaned) == 0 {
		cleaned = []string{"public"}
	}
	cfg.Keys = cleaned
	cfg.Key = cleaned[0]
}

func loadZenConfig() *zenConfigData {
	path := kit.ResolveDataPath(".zen-config.json")
	cfg := defaultZenConfig()
	fileExists := false
	if data, err := os.ReadFile(path); err == nil {
		fileExists = true
		if err := json.Unmarshal(data, cfg); err != nil {
			// 损坏配置改名留档，避免下次保存把可手工恢复的原文覆盖掉
			stamp := time.Now().Format("20060102-150405")
			if renErr := os.Rename(path, path+".corrupt-"+stamp); renErr == nil {
				log.Printf("zen config is corrupt JSON; moved to %s.corrupt-%s", path, stamp)
			}
			log.Printf("zen config parse failed: %v", err)
		}
	}
	// ZEN_KEYS 环境变量：配置为空（无文件或只有默认 public key）时注入多 key 池
	if envKeys := envList("ZEN_KEYS"); len(envKeys) > 0 {
		if !fileExists || len(cfg.Keys) == 0 || (len(cfg.Keys) == 1 && cfg.Keys[0] == "public") {
			cfg.Keys = envKeys
			log.Printf("zen keys seeded from ZEN_KEYS env: %d key(s)", len(envKeys))
		}
	}
	normalizeZenKeys(cfg)
	if cfg.BaseURL == "" {
		cfg.BaseURL = zenAPIBase
	}
	return cfg
}

func saveZenConfig() {
	zenConfigMu.Lock()
	defer zenConfigMu.Unlock()
	data, err := json.MarshalIndent(zenConfig, "", "  ")
	if err != nil {
		log.Printf("zen config marshal failed: %v", err)
		return
	}
	// 原子写: 临时文件 + rename，崩溃中途写入不会截断原文件
	path := kit.ResolveDataPath(".zen-config.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		log.Printf("zen config save failed: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("zen config save failed (rename): %v", err)
	}
}

func getZenConfig() *zenConfigData {
	zenConfigMu.Lock()
	defer zenConfigMu.Unlock()
	return zenConfig
}

func setZenConfig(c *zenConfigData) {
	normalizeZenKeys(c)
	zenConfigMu.Lock()
	old := zenConfig
	zenConfig = c
	zenConfigMu.Unlock()
	saveZenConfig()
	rebuildZenSem()
	// 清理已移除 key 的轮转状态
	valid := map[string]bool{}
	for _, k := range c.Keys {
		valid[k] = true
	}
	zenKeyMu.Lock()
	for k := range zenKeyCool {
		if !valid[k] {
			delete(zenKeyCool, k)
		}
	}
	for k := range zenKeyUsage {
		if !valid[k] {
			delete(zenKeyUsage, k)
		}
	}
	zenKeyIdx = 0
	zenKeyMu.Unlock()
	// 会话粘性与 403 恢复计数同样按 key 存储，移除的 key 必须一并清掉：
	// 否则长跑容器上轮换 key 会让 .zen-sessions.json 与计数表单调增长，
	// 被删掉的旧 key 的粘性身份还留在盘上。
	pruneZenKeyState(valid)
	// 代理列表变化时: 索引会位移,按索引记录的冷却整体失效,直接清空;
	// 同时驱逐已移除代理的钉定客户端,释放其空闲连接
	if proxiesChanged(old.Proxies, c.Proxies) {
		zenProxyCooldownsMu.Lock()
		zenProxyCooldowns = map[int]time.Time{}
		zenProxyCooldownsMu.Unlock()
	}
	validProxy := map[string]bool{"": true}
	for _, p := range c.Proxies {
		validProxy[p] = true
	}
	proxyClientCacheMu.Lock()
	for u, cl := range proxyClientCache {
		if !validProxy[u] {
			delete(proxyClientCache, u)
			if cl != nil {
				cl.CloseIdleConnections()
			}
		}
	}
	proxyClientCacheMu.Unlock()
}

// proxiesChanged 按顺序比较两个代理列表是否不同（数量或任一位置变化都算）。
func proxiesChanged(a, b []string) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i] != b[i] {
			return true
		}
	}
	return false
}

// validateProxyList 校验代理列表格式: 支持 http/https/socks5/socks5h, 必须包含 host:port。
func validateProxyList(proxies []string) error {
	for _, p := range proxies {
		line := strings.TrimSpace(p)
		if line == "" {
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			return fmt.Errorf("代理格式无效 %q: %v", line, err)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("代理 %q 协议不受支持（支持 http/https/socks5/socks5h）", line)
		}
		if u.Host == "" {
			return fmt.Errorf("代理 %q 缺少 host:port", line)
		}
		if _, _, err := net.SplitHostPort(u.Host); err != nil {
			return fmt.Errorf("代理 %q 缺少端口: %v", line, err)
		}
	}
	return nil
}

// ============ zen 多 key 轮转（round_robin 默认策略） ============

// maskZenKey 管理面板与 mint 结果里展示 key 的掩码形式。
// 不复用 kit.Truncate：它对短于上限的串原样返回，短 key 会被整只打印出来。
func maskZenKey(k string) string {
	switch {
	case k == "":
		return "-"
	case k == "public":
		return "public (no key)"
	case len(k) <= 6:
		return "…" // 太短，连前缀都不给
	default:
		return k[:6] + "…"
	}
}

var (
	zenKeyMu    sync.Mutex
	zenKeyIdx   int
	zenKeyUsage = map[string]int64{} // 每 key 成功调用计数（内存态）
	zenKeyCool  = map[string]time.Time{}
)

// pickZenKey round-robin 选取一个未冷却的 key；全部冷却时按轮转顺序返回下一个。
// 返回 "" 表示当前没有配置任何 key。
func pickZenKey() string {
	keys := getZenConfig().Keys
	if len(keys) == 0 {
		return ""
	}
	// 先取 live 会话集合（zenSessMu）再进 zenKeyMu：两把锁不嵌套，避免与
	// 会话换新路径（持 zenSessMu 时不取 zenKeyMu，反之亦然）形成锁序反转。
	live := zenLiveKeys(keys)
	zenKeyMu.Lock()
	defer zenKeyMu.Unlock()
	now := time.Now()
	// 先清理过期冷却
	for k, until := range zenKeyCool {
		if now.After(until) {
			delete(zenKeyCool, k)
		}
	}
	// 第一轮：非冷却 + 有 live 会话。本地铸造后所有 key 生而有合法会话，
	// 此轮几乎总是命中；仅在文件加载失败等异常时退化到第二轮。
	for i := 0; i < len(keys); i++ {
		idx := (zenKeyIdx + i) % len(keys)
		k := keys[idx]
		if _, cooling := zenKeyCool[k]; !cooling && live[k] {
			zenKeyIdx = (idx + 1) % len(keys)
			return k
		}
	}
	// 第二轮：非冷却（与 live 无关——异常窗口内仍须发请求，
	// 上游 403 是唯一可用信号，恢复路径随后本地换新会话）
	for i := 0; i < len(keys); i++ {
		idx := (zenKeyIdx + i) % len(keys)
		k := keys[idx]
		if _, cooling := zenKeyCool[k]; !cooling {
			zenKeyIdx = (idx + 1) % len(keys)
			return k
		}
	}
	// 全部冷却中：仍按轮转返回（保持请求流动，上游会再次限流）
	k := keys[zenKeyIdx%len(keys)]
	zenKeyIdx = (zenKeyIdx + 1) % len(keys)
	return k
}

// markZenKeySuccess 在上游 200 后累计 key 的成功调用数（仅内存态，面板展示用）。
// 计数放在成功路径而非选取路径，失败重试才不会虚增用量。
func markZenKeySuccess(key string) {
	if key == "" {
		return
	}
	zenKeyMu.Lock()
	zenKeyUsage[key]++
	zenKeyMu.Unlock()
}

// cooldownZenKey 将 key 置为冷却，冷却期内 round-robin 跳过它。
// 默认的 "public" key 仅在它是池中唯一 key 时跳过冷却（无其他 key 可轮转）；
// 多 key 池中 "public" 也参与冷却轮转。
// 返回实际生效的冷却时长（0 = 未冷却：空 key，或单 key 池的 public 哨兵）——
// 调用点的日志必须打印这个值而不是原始解析值，否则"缺头默认 1 分钟 / 24h 上限"
// 的换算不会体现在日志里。
func cooldownZenKey(key string, d time.Duration) time.Duration {
	if key == "" {
		return 0
	}
	if key == "public" && len(getZenConfig().Keys) <= 1 {
		return 0
	}
	if d <= 0 {
		d = time.Minute
	}
	if d > maxCooldown {
		d = maxCooldown
	}
	zenKeyMu.Lock()
	zenKeyCool[key] = time.Now().Add(d)
	zenKeyMu.Unlock()
	return d
}

// zenKeyCooling 查询 key 是否处于冷却期。
func zenKeyCooling(key string) bool {
	zenKeyMu.Lock()
	defer zenKeyMu.Unlock()
	until, ok := zenKeyCool[key]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(zenKeyCool, key)
		return false
	}
	return true
}

// zenKeyCooldownUntil 只读查询冷却截止时刻（不清理过期项）。
// 管理面板"Test"命中 429 后用它回报预计恢复时间——上游的 Retry-After 决定
// 冷却时长（上限 24h），不写进日志的话面板是唯一能看到的观测点。
func zenKeyCooldownUntil(key string) (time.Time, bool) {
	zenKeyMu.Lock()
	defer zenKeyMu.Unlock()
	until, ok := zenKeyCool[key]
	return until, ok
}

// uncoolZenKey 清除 key 的冷却。管理面板"Test"探测成功后调用，语义与 cline
// 账号的"测试成功即复位"一致：一次真实的 2xx 是"该 key 现在能用"的最强证据，
// 比干等 Retry-After 更可信。对未冷却/无关 key 是无害 no-op。
func uncoolZenKey(key string) {
	if key == "" {
		return
	}
	zenKeyMu.Lock()
	delete(zenKeyCool, key)
	zenKeyMu.Unlock()
}

// zenCallOpts 单次上游调用的可选参数（变参：既有 callZenAPI/callZenResponsesAPI
// 调用点零改动）。目前只有 pinKey：把本次调用的**所有**尝试固定在一个 key 上。
// 与 ZEN_PIN_KEY 环境变量的区别：那是进程级排障开关，会影响所有并发请求；
// pinKey 只作用于这一次调用（管理面板的 per-key "Test" 按钮），绝不影响正常轮转。
type zenCallOpts struct {
	pinKey string
}

// zenKeyStatus 每个 key 的运行时状态（管理面板展示用，key 值打码）。
func zenKeyStatus() []map[string]any {
	keys := getZenConfig().Keys
	// 会话状态先快照（zenSessMu），再进 zenKeyMu：两把锁不嵌套。
	sess := make([]zenSessionSnapshot, len(keys))
	for i, k := range keys {
		sess[i] = zenSessionSnapshotOf(k)
	}
	zenKeyMu.Lock()
	defer zenKeyMu.Unlock()
	now := time.Now()
	out := make([]map[string]any, 0, len(keys))
	for i, k := range keys {
		st := map[string]any{
			"index":   i,
			"keyMask": maskZenKey(k),
			"usage":   zenKeyUsage[k],
			"current": i == zenKeyIdx%maxInt(len(keys), 1),
			// live 会话状态：未 mint 的 key 必 403，面板必须能看出来，
			// 否则"首启为什么这么慢"只能靠翻容器日志猜。
			"sessionLive":   sess[i].Live,
			"sessionMinted": sess[i].Minted,
			"session":       sess[i].Session,
		}
		if sess[i].CreatedAt > 0 {
			st["sessionCreatedAt"] = time.Unix(sess[i].CreatedAt, 0).Format(time.RFC3339)
		}
		if until, cooling := zenKeyCool[k]; cooling && now.Before(until) {
			st["cooling"] = true
			st["cooldownUntil"] = until.Format(time.RFC3339)
		} else {
			st["cooling"] = false
		}
		out = append(out, st)
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// pinnedZenKey 取 ZEN_PIN_KEY 指定的固定 key（1-based 序号，如 "2"）。
// 排障/单 key 直测用：所有 attempt 都用该 key，避免轮换污染归因。
// 未设置或越界时返回 ""（正常轮转）。
func pinnedZenKey() string {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("ZEN_PIN_KEY")))
	if err != nil || n < 1 {
		return ""
	}
	keys := getZenConfig().Keys
	if n > len(keys) {
		return ""
	}
	return keys[n-1]
}

// ============ zen 上游调用 ============

// zenGateToolSpecs 免费层两个端点必须携带的 opencode 工具集（工具名与官方
// CLI 一致）。FreeTier 中间件按工具名校验"请求是否来自 opencode CLI"
//（2026-09-18 实测解码：缺工具/工具名不齐 → 403 FreeTierError，即使会话
// 有效；乱序、假描述、仅核心 5 名(bash/edit/glob/grep/read)也通过）。
// 描述用精简占位即可——工具注入后模型在 tool_choice=none（chat）或
// 文本语义（responses）下不产生实质工具调用，回复保持纯文本。
// 单一来源：改名/增删必须同时满足两个端点的校验，只改这里。
var zenGateToolSpecs = []struct{ name, desc string }{
	{"bash", "Execute a bash command"},
	{"edit", "Edit a file"},
	{"glob", "Find files by glob pattern"},
	{"grep", "Search file contents"},
	{"read", "Read a file"},
	{"skill", "Load a skill"},
	{"task", "Run a background task"},
	{"todowrite", "Write a todo list"},
	{"webfetch", "Fetch a web page"},
	{"websearch", "Search the web"},
	{"write", "Write a file"},
}

// zenGateTools chat 端点形态：tools[].{type,function:{name,description,parameters}}。
// 返回 []any 与客户端解码出来的 tools 同型（下游要拼接两者）。
func zenGateTools() []any {
	out := make([]any, 0, len(zenGateToolSpecs))
	for _, s := range zenGateToolSpecs {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        s.name,
				"description": s.desc,
				"parameters":  zenEmptySchema(),
			},
		})
	}
	return out
}

// buildZenBody 构造 zen 请求体:只带 OpenAI 兼容字段,改写模型为 zen ID
func buildZenBody(params map[string]any, stream bool) map[string]any {
	body := map[string]any{}
	for _, key := range passThroughKeys {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}
	for _, key := range []string{"model", "messages", "max_tokens", "max_completion_tokens", "stream"} {
		if val, ok := params[key]; ok {
			body[key] = val
		}
	}
	if stream {
		body["stream"] = true
	}
	if model, ok := params["model"].(string); ok {
		if m, ok := resolveZenModel(model); ok {
			body["model"] = m.ID
		}
	}
	delete(body, "reasoning_effort")
	delete(body, "reasoningEffort")
	// FreeTier gate（2026-09-18 实测解码）：zen 免费层 chat 端点校验请求体
	// 是否携带 opencode 工具集，缺失则 403（"can only be used from within
	// OpenCode"）。恒注入 11 个规范工具名保证过闸。tool_choice 按客户端
	// 是否带工具分流（实测对比）：
	//  - 客户端无工具（纯文本问答）：tool_choice=none。auto 会让部分模型
	//    （ling 多轮实测）在有工具但无需调用时返回空文本；none 保证回复。
	//  - 客户端带工具（IDE agent 模式，Cursor 等）：客户端定义优先（同名时
	//    覆盖 gate 占位 stub，模型才能拿到真实参数 schema）+ tool_choice=auto，
	//    模型可发起调用、IDE 执行后回传结果（实测 ling 调用客户端自定义
	//    get_weather 成功）。客户端显式 none 时尊重（实测 chat 端点接受 none）。
	delete(body, "functions")
	delete(body, "function_call")
	clientTools := zenClientTools(params)
	if len(clientTools) == 0 {
		body["tools"] = zenGateTools()
		body["tool_choice"] = "none"
	} else {
		body["tools"] = zenMergeTools(clientTools, zenGateTools())
		body["tool_choice"] = "auto"
		if tc, _ := params["tool_choice"].(string); tc == "none" {
			body["tool_choice"] = "none"
		}
	}
	return body
}

// zenClientTools 提取客户端工具（chat 嵌套形态 tools[]，兼容已废弃的
// functions[] 形态），返回可直接发给 chat 端点的嵌套定义。
func zenClientTools(params map[string]any) []any {
	var out []any
	if ct, ok := params["tools"].([]any); ok {
		for _, t := range ct {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			fn, ok := tm["function"].(map[string]any)
			if !ok {
				continue
			}
			n, _ := fn["name"].(string)
			if n == "" {
				continue
			}
			if fn["parameters"] == nil {
				fn["parameters"] = zenEmptySchema() // null schema 上游判非法
			}
			out = append(out, tm)
		}
	}
	if len(out) > 0 {
		return out
	}
	if fns, ok := params["functions"].([]any); ok {
		for _, f := range fns {
			fm, ok := f.(map[string]any)
			if !ok {
				continue
			}
			n, _ := fm["name"].(string)
			if n == "" {
				continue
			}
			fn := map[string]any{"name": n}
			if d, ok := fm["description"].(string); ok {
				fn["description"] = d
			}
			if p, ok := fm["parameters"]; ok && p != nil {
				fn["parameters"] = p
			} else {
				fn["parameters"] = zenEmptySchema()
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		}
	}
	return out
}

// zenMergeTools 合并客户端工具与 gate 占位工具：客户端定义优先，同名 gate
// stub 跳过。gate 只校验工具名是否存在，带真实 schema 的客户端定义同样过闸，
// 而 stub 抢先会让模型拿到空参数结构（如客户端 read 的 file_path 丢失）。
func zenMergeTools(clientTools []any, gateTools []any) []any {
	merged := make([]any, 0, len(clientTools)+len(gateTools))
	provided := map[string]bool{}
	for _, t := range clientTools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tm["function"].(map[string]any)
		if fn == nil {
			continue
		}
		n, _ := fn["name"].(string)
		if n == "" || provided[n] {
			continue
		}
		provided[n] = true
		merged = append(merged, tm)
	}
	for _, gt := range gateTools {
		tm, ok := gt.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := tm["function"].(map[string]any)
		n, _ := fn["name"].(string)
		if n == "" || provided[n] {
			continue
		}
		provided[n] = true
		merged = append(merged, tm)
	}
	return merged
}

func zenEmptySchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// ============ zen 上游调用：原生 /v1/responses 端点 ============
//
// 背景: 部分 zen 免费模型（如 muse-spark）在 chat/completions 端点上 500，
// 官方 opencode CLI 对它们只发原生 POST /zen/v1/responses（Responses 形态
// 请求体 + SSE 事件流响应）。网关对这类模型走同样路径：请求体按 Responses
// 形态构造，响应再转回 chat completions 形态，上下游都不感知差异。

// buildZenResponsesBody 把 chat 请求参数映射为 Responses 形态请求体：
// messages -> input（system/developer 指令并入 instructions），
// max_tokens/max_completion_tokens -> max_output_tokens。
// 字段经官方 CLI 实际请求逐字段核对：reasoning.summary=auto、
// include=[reasoning.encrypted_content]、tool_choice 缺省 auto、
// prompt_cache_key=会话 ID 均为 CLI 常发字段，缺省会与原生形态不一致。
// promptKey 为本次上游会话 ID（调用方传入本次请求的 sess_ 值）。
// 无 stream 形参：上游恒为 stream=true（见函数内注释）。
func buildZenResponsesBody(params map[string]any, modelID string, promptKey string) map[string]any {
	body := map[string]any{
		"model":  modelID,
		"stream": true, // FreeTier gate 硬要求：responses 端点只接受 stream=true，
		// stream=false（即使显式传）上游按"非 CLI"请求 403。网关非流式客户端
		// 由调用方聚合（responsesSSEToChat），body 恒发 true。
		"store": false,
	}
	if promptKey != "" {
		body["prompt_cache_key"] = promptKey
	}
	var input []any
	if msgs, ok := params["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			content := responsesContentToInput(mm["content"])
			if role == "system" || role == "developer" {
				// system/developer 指令提升为顶层 instructions（Responses 标准字段；
				// 官方 CLI 发 role=developer 条目且无顶层 instructions——网关把
				// 历史 system/developer 内容并入 instructions 保持等价）
				if s, ok := content.(string); ok && s != "" {
					if prev, _ := body["instructions"].(string); prev != "" {
						body["instructions"] = prev + "\n\n" + s
					} else {
						body["instructions"] = s
					}
				}
				continue
			}
			if role == "tool" {
				// tool 结果 -> function_call_output（Responses 标准形态）
				callID, _ := mm["tool_call_id"].(string)
				out := ""
				switch o := content.(type) {
				case string:
					out = o
				default:
					if b, err := json.Marshal(content); err == nil {
						out = string(b)
					}
				}
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": callID,
					"output":  out,
				})
				continue
			}
			entry := map[string]any{"role": role, "content": content}
			// assistant 历史 tool_calls -> function_call 条目（保留调用链）。
			// 内容与工具调用分开：assistant 的文本内容仍在 entry 里，每个
			// tool_call 各 append 一条 function_call 输入（旧实现 break 只保
			// 第一条且把 entry 换成 function_call，丢掉正文文本）。
			if role == "assistant" {
				if tcs, ok := mm["tool_calls"].([]any); ok {
					for ci, tc := range tcs {
						tcm, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						fn, _ := tcm["function"].(map[string]any)
						id, _ := tcm["id"].(string)
						name := ""
						args := ""
						if fn != nil {
							name, _ = fn["name"].(string)
							args, _ = fn["arguments"].(string)
						}
						if id == "" {
							// function_call 的 call_id 是必填：缺 id 的历史
							// 条目会让整个请求 400；补一个稳定的合成 id
							id = fmt.Sprintf("call_hist_%d_%d", len(input), ci)
						}
						if args == "" {
							args = "{}" // 空串不是合法 JSON 参数
						} else if !json.Valid([]byte(args)) {
							args = repairToolArguments(args)
						}
						input = append(input, map[string]any{
							"type":      "function_call",
							"id":        id,
							"call_id":   id,
							"name":      name,
							"arguments": args,
						})
					}
				}
			}
			input = append(input, entry)
		}
	}
	if len(input) == 0 {
		input = []any{map[string]any{"role": "user", "content": ""}}
	}
	body["input"] = input
	// 输出上限：Responses 字段名是 max_output_tokens（官方 CLI 实测发送该字段）
	if mt := asInt(params["max_tokens"]); mt > 0 {
		body["max_output_tokens"] = mt
	} else if mt := asInt(params["max_completion_tokens"]); mt > 0 {
		body["max_output_tokens"] = mt
	}
	// 透传采样参数（Responses 与 chat 同名）
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := params[k]; ok {
			body[k] = v
		}
	}
	// reasoning_effort 在 Responses 形态下保留为 reasoning.effort（官方 CLI 实测发送）
	if eff, ok := params["reasoning_effort"].(string); ok && eff != "" {
		body["reasoning"] = map[string]any{"effort": eff, "summary": "auto"}
	} else if eff, ok := params["reasoningEffort"].(string); ok && eff != "" {
		body["reasoning"] = map[string]any{"effort": eff, "summary": "auto"}
	} else {
		// 官方 CLI 即使无 effort 也发 reasoning.summary=auto；缺该字段的
		// 请求与原生形态不一致，补默认值保持一致
		body["reasoning"] = map[string]any{"summary": "auto"}
	}
	// include 原生字段：官方 CLI 实测发送 reasoning.encrypted_content；
	// 缺省时补齐以匹配原生请求形态
	body["include"] = []any{"reasoning.encrypted_content"}
	// FreeTier gate（2026-09-18 实测解码）：/v1/responses 端点同样校验请求体
	// 是否携带 opencode 工具集，缺失则 403；且必须 flat 形态 + tool_choice=auto
	//（chat 嵌套形态 400 missing name、tool_choice=none 400 only auto allowed）。
	// 恒注入 11 个规范工具名 + auto——auto 是 responses 端点唯一接受的值。
	// 客户端工具（chat 嵌套或已是 flat）优先于同名 gate stub，schema 才不丢。
	body["tools"] = zenMergeResponsesTools(zenClientFlatTools(params))
	body["tool_choice"] = "auto"
	return body
}

// setZenGateHeaders 写入 zen 免费门要求的客户端身份头，两条上游路径（chat
// /responses）共用——此前两处逐行重复，改一处漏一处就会让某条路径掉头。
// 直接写 map（而非 Header.Set）以保留小写头名：官方 CLI 发的就是小写，
// Set 会规范化为 X-Opencode-...；HTTP 头大小写不敏感，但字节级对齐更安全。
// Authorization/Content-Type/User-Agent 因需承载变量，由调用方先行设置。
func setZenGateHeaders(req *http.Request, sess, requestID string) {
	req.Header["x-opencode-session-id"] = []string{sess}
	req.Header["x-opencode-session"] = []string{sess}
	req.Header["x-opencode-request"] = []string{requestID}
	req.Header["x-opencode-client"] = []string{"cli"}
	req.Header["x-opencode-project"] = []string{"global"}
}

// zenClientFlatTools 提取客户端工具并转成 responses 端点要求的 flat 形态。
// 同时接受 chat 嵌套形态 {type,function:{...}} 与 responses 原生 flat 形态
// {type,name,description,parameters,strict}。
func zenClientFlatTools(params map[string]any) []map[string]any {
	var out []map[string]any
	add := func(name, desc string, p any) {
		if name == "" {
			return
		}
		if p == nil {
			p = zenEmptySchema()
		}
		out = append(out, map[string]any{
			"type":        "function",
			"name":        name,
			"description": desc,
			"parameters":  p,
			"strict":      false,
		})
	}
	if ct, ok := params["tools"].([]any); ok {
		for _, t := range ct {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := tm["function"].(map[string]any); ok {
				n, _ := fn["name"].(string)
				desc, _ := fn["description"].(string)
				add(n, desc, fn["parameters"])
				continue
			}
			n, _ := tm["name"].(string)
			if n == "" {
				continue
			}
			desc, _ := tm["description"].(string)
			add(n, desc, tm["parameters"])
		}
	}
	if len(out) > 0 {
		return out
	}
	if fns, ok := params["functions"].([]any); ok {
		for _, f := range fns {
			fm, ok := f.(map[string]any)
			if !ok {
				continue
			}
			n, _ := fm["name"].(string)
			desc, _ := fm["description"].(string)
			add(n, desc, fm["parameters"])
		}
	}
	return out
}

// zenMergeResponsesTools 客户端 flat 工具优先，缺名补 gate stub 过闸。
func zenMergeResponsesTools(clientTools []map[string]any) []any {
	merged := make([]any, 0, len(clientTools)+len(zenGateToolSpecs))
	provided := map[string]bool{}
	for _, t := range clientTools {
		n, _ := t["name"].(string)
		if n == "" || provided[n] {
			continue
		}
		provided[n] = true
		merged = append(merged, t)
	}
	for _, s := range zenGateToolSpecs {
		if provided[s.name] {
			continue
		}
		provided[s.name] = true
		merged = append(merged, map[string]any{
			"type":        "function",
			"name":        s.name,
			"description": s.desc,
			"parameters":  zenEmptySchema(),
			"strict":      false,
		})
	}
	return merged
}

// responsesContentToInput chat content -> Responses input content。
// 字符串原样；parts 数组只保留文本/图片两种 Responses 原生类型。
func responsesContentToInput(content any) any {
	if s, ok := content.(string); ok {
		return s
	}
	parts, ok := content.([]any)
	if !ok {
		return ""
	}
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		t, _ := pm["type"].(string)
		switch t {
		case "text", "input_text":
			txt, _ := pm["text"].(string)
			out = append(out, map[string]any{"type": "input_text", "text": txt})
		case "image_url":
			url := ""
			if u, ok := pm["image_url"].(map[string]any); ok {
				url, _ = u["url"].(string)
			}
			if url != "" {
				// Responses 原生图片 part 是 input_image；chat 形态的
				// image_url 会被上游拒绝/忽略
				out = append(out, map[string]any{"type": "input_image", "image_url": url})
			}
		}
	}
	if len(out) == 1 {
		if one, ok := out[0].(map[string]any); ok && one["type"] == "input_text" {
			txt, _ := one["text"].(string)
			return txt
		}
	}
	return out
}

// responsesSSEToChat 把原生 responses SSE 事件流聚合成 chat completions 形态。
// 处理两种事件风格（上游按次返回其一）：
//   - delta 流：response.output_text.delta / function_call_arguments.delta /
//     reasoning_summary_text.delta（逐块增量文本）
//   - output_item.done：response.output_item.done 内 item.content[].text 全量
//     文本（muse-spark 对纯文本问答只发该事件，无 delta 流）
// usage 从 response.completed 或 response.incomplete 提取；
// incomplete_details.reason 映射为 finish 原因（length→length）。
// zenSSECall 单个 function_call 输出项的流式累积器。上游可能并行/连续输出
// 多个工具调用，必须按 item 分开累积参数，否则两个调用的 arguments delta
// 会串成一段坏 JSON（上游实测：webfetch 双调用 jam 成 {"query":...}{"url":...}）。
type zenSSECall struct {
	id, name, callID string
	idx              int // response.output_index：无 id/call_id 时的绑定键（缺失为 -1）
	args             strings.Builder
	final            string // output_item.done 一次性给全参（无 delta 事件时）
}

// findZenSSECall 定位事件所属的累积器：id/call_id 精确匹配 → output_index
// 匹配。两者都对不上时返回 nil，由调用方按场景决定要不要认领无主累积器 ——
// 绝不"回退到最后一个调用"：并行调用时那会把 A 的参数 delta 追加到 B 上，
// 拼出 {"query":...}{"url":...} 这种坏 JSON（上游实测的 jam 正是如此）。
func findZenSSECall(calls []*zenSSECall, id, callID string, outIdx int) *zenSSECall {
	for _, c := range calls {
		if id != "" && (c.id == id || c.callID == id) {
			return c
		}
		if callID != "" && (c.callID == callID || c.id == callID) {
			return c
		}
	}
	if outIdx >= 0 {
		for _, c := range calls {
			if c.idx == outIdx {
				return c
			}
		}
	}
	return nil
}

// zenOrphanCall 返回第一个"无标识但已有内容"的累积器：delta/done 先于
// added 到达时内容落在无主累积器里，随后的 added/done 按它归位。
func zenOrphanCall(calls []*zenSSECall) *zenSSECall {
	for _, c := range calls {
		if c.id == "" && c.callID == "" && (c.args.Len() > 0 || c.final != "") {
			return c
		}
	}
	return nil
}

// zenSSEOutIdx 取事件里的 output_index，缺失返回 -1。
func zenSSEOutIdx(ev map[string]any) int {
	if v, ok := ev["output_index"].(float64); ok {
		return int(v)
	}
	return -1
}

func responsesSSEToChat(resp *http.Response) (map[string]any, error) {
	defer resp.Body.Close()
	var text, reasoning strings.Builder
	var usage map[string]any
	var incompleteReason string
	var failedMsg string
	failed := false
	textDeltas := false
	sawData := false
	var rawHead strings.Builder // 非 SSE 响应体（错误 JSON）的采样，用于报错
	calls := []*zenSSECall{}
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data:") {
				sawData = true
				payload := strings.TrimSpace(line[5:])
				if payload != "" && payload != "[DONE]" {
					var ev map[string]any
					if json.Unmarshal([]byte(payload), &ev) == nil {
						typ, _ := ev["type"].(string)
						switch typ {
						case "response.output_text.delta":
							if d, _ := ev["delta"].(string); d != "" {
								textDeltas = true
								text.WriteString(d)
							}
						case "response.reasoning_summary_text.delta":
							if d, _ := ev["delta"].(string); d != "" {
								reasoning.WriteString(d)
							}
						case "response.function_call_arguments.delta":
							if d, _ := ev["delta"].(string); d != "" {
								iid, _ := ev["item_id"].(string)
								c := findZenSSECall(calls, iid, "", zenSSEOutIdx(ev))
								if c == nil {
									switch {
									case len(calls) == 0:
										// 首个事件就是 delta：先建无主累积器，
										// 用 item_id 当身份，后续 added/done 才找得回来
										c = &zenSSECall{idx: zenSSEOutIdx(ev), id: iid}
										calls = append(calls, c)
									case len(calls) == 1 && calls[0].id == "" && calls[0].callID == "":
										// 唯一且无主的累积器不可能是别人的
										c = calls[0]
									default:
										// 归属不明的 delta：宁可丢弃（done 事件会带全量
										// 参数），也不能把 A 的碎片拼进 B 的参数里
										c = zenOrphanCall(calls)
									}
								}
								if c != nil {
									c.args.WriteString(d)
								}
							}
						case "response.function_call_arguments.done":
							// 收尾事件：部分上游只发 delta + 本事件（无 output_item.done）
							if a, _ := ev["arguments"].(string); a != "" {
								iid, _ := ev["item_id"].(string)
								c := findZenSSECall(calls, iid, "", zenSSEOutIdx(ev))
								if c == nil {
									c = zenOrphanCall(calls)
								}
								if c != nil && c.args.Len() == 0 && c.final == "" {
									c.final = a
								}
							}
						case "response.output_item.added":
							if item, ok := ev["item"].(map[string]any); ok {
								if it, _ := item["type"].(string); it == "function_call" {
									id, _ := item["id"].(string)
									cid, _ := item["call_id"].(string)
									name, _ := item["name"].(string)
									// 就地更新：重复 added、以及先到的孤儿 delta 都归位到
									// 同一累积器，不再无条件新建（新建会把 delta 变成无名壳）
									c := findZenSSECall(calls, id, cid, zenSSEOutIdx(ev))
									if c == nil {
										c = zenOrphanCall(calls)
									}
									if c == nil {
										c = &zenSSECall{idx: zenSSEOutIdx(ev)}
										calls = append(calls, c)
									}
									if id != "" {
										c.id = id
									}
									if cid != "" {
										c.callID = cid
									}
									if name != "" {
										c.name = name
									}
								}
							}
						case "response.output_item.done":
							// 全量输出项：message 条目 content[].output_text/text 即最终文本；
							// function_call 条目（无 delta 事件的模型）一次性给全参
							if item, ok := ev["item"].(map[string]any); ok {
								switch it, _ := item["type"].(string); it {
								case "message":
									// delta 流已经给过正文时不再采纳全量文本，否则文本翻倍
									if textDeltas {
										break
									}
									if content, ok := item["content"].([]any); ok {
										for _, part := range content {
											pm, ok := part.(map[string]any)
											if !ok {
												continue
											}
											pt, _ := pm["type"].(string)
											if pt != "output_text" && pt != "text" {
												continue
											}
											if t, _ := pm["text"].(string); t != "" {
												text.WriteString(t)
											}
										}
									}
								case "function_call":
									// 全量参数（无 delta 事件的模型一次给全）
									a := ""
									if av, ok := item["arguments"].(string); ok {
										a = av
									}
									id, _ := item["id"].(string)
									cid, _ := item["call_id"].(string)
									name, _ := item["name"].(string)
									target := findZenSSECall(calls, id, cid, zenSSEOutIdx(ev))
									if target == nil {
										target = zenOrphanCall(calls)
									}
									if target == nil {
										// 无标识可归属时只能收回"最后一个完全空的壳"；
										// 否则新建（不能覆盖已有调用的 id/name/参数）
										if n := len(calls); n > 0 && calls[n-1].id == "" &&
											calls[n-1].callID == "" && calls[n-1].name == "" &&
											calls[n-1].args.Len() == 0 && calls[n-1].final == "" {
											target = calls[n-1]
										} else {
											target = &zenSSECall{idx: zenSSEOutIdx(ev)}
											calls = append(calls, target)
										}
									}
									if id != "" {
										target.id = id
									}
									if cid != "" {
										target.callID = cid
									}
									if name != "" {
										target.name = name
									}
									if a != "" && target.args.Len() == 0 && target.final == "" {
										target.final = a
									}
								}
							}
						case "response.completed", "response.incomplete", "response.failed":
							if r, ok := ev["response"].(map[string]any); ok {
								if u, ok := r["usage"].(map[string]any); ok {
									usage = u
								}
								if det, ok := r["incomplete_details"].(map[string]any); ok {
									if reason, _ := det["reason"].(string); reason != "" {
										incompleteReason = reason
									}
								}
								if typ == "response.failed" {
									failed = true
									if incompleteReason == "" {
										incompleteReason = "error"
									}
									// failed 事件里 response.error.message 才是真因
									// （限流/余额/会话失效），不带出去就只剩 "error"
									if em, ok := r["error"].(map[string]any); ok {
										if m, _ := em["message"].(string); m != "" {
											failedMsg = m
										}
									}
								}
							}
						}
					}
				}
			}
		}
		if !sawData && !strings.HasPrefix(line, "data:") && rawHead.Len() < 8192 {
			rawHead.WriteString(line)
			rawHead.WriteString("\n")
		}
		if err != nil {
			break
		}
	}
	// 一个 data: 行都没有 = 上游返回的不是 SSE（402/403/500 全是 JSON 错误体）。
	// 直接报错，别把它当成"空成功响应"下发（客户端会收到空 output 且无报错）。
	if !sawData {
		head := strings.TrimSpace(rawHead.String())
		if head == "" {
			return nil, fmt.Errorf("zen responses upstream returned empty body")
		}
		var obj map[string]any
		if json.Unmarshal([]byte(head), &obj) == nil {
			if e, ok := obj["error"].(map[string]any); ok {
				if m, _ := e["message"].(string); m != "" {
					return nil, fmt.Errorf("zen responses upstream error: %s", m)
				}
			}
		}
		if len(head) > 300 {
			head = head[:300]
		}
		return nil, fmt.Errorf("zen responses upstream returned non-SSE body: %s", head)
	}
	if failed {
		if failedMsg != "" {
			return nil, fmt.Errorf("zen responses upstream failed: %s", failedMsg)
		}
		return nil, fmt.Errorf("zen responses upstream failed: %s", incompleteReason)
	}
	textStr := text.String()
	msg := map[string]any{"role": "assistant", "content": textStr}
	finish := "stop"
	if incompleteReason == "max_output_tokens" || incompleteReason == "length" {
		finish = "length"
	}
	if len(calls) > 0 {
		outCalls := make([]any, 0, len(calls))
		for i, call := range calls {
			if call.name == "" {
				// 无名调用客户端无法执行（多半是 added 后未闭合的空壳）。有内容时
				// 说明上游真的发了调用却缺名字，值得留日志排查；空壳则静默丢弃。
				if call.args.Len() > 0 || call.final != "" {
					log.Printf("  zen responses: dropping nameless tool call (%d bytes args)",
						call.args.Len()+len(call.final))
				}
				continue
			}
			raw := call.final
			if call.args.Len() > 0 {
				raw = call.args.String()
			}
			if raw == "" {
				raw = "{}" // 无参调用：空串不是合法 JSON，IDE 解析会直接失败
			}
			id := call.callID
			if id == "" {
				id = call.id
			}
			if id == "" {
				// 带序号，避免同一响应内多个无 id 调用撞成同一个 call_id
				id = fmt.Sprintf("call_%x_%d", time.Now().UnixNano(), i)
			}
			outCalls = append(outCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      call.name,
					"arguments": repairToolArguments(raw),
				},
			})
		}
		if len(outCalls) > 0 {
			msg["tool_calls"] = outCalls
			finish = "tool_calls"
		}
	}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	out := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%x", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	if usage != nil {
		u := map[string]any{
			"prompt_tokens":     float64(0),
			"completion_tokens": float64(0),
			"total_tokens":      float64(0),
		}
		if v, ok := usage["input_tokens"].(float64); ok {
			u["prompt_tokens"] = v
		}
		if v, ok := usage["output_tokens"].(float64); ok {
			u["completion_tokens"] = v
		}
		if v, ok := usage["total_tokens"].(float64); ok {
			u["total_tokens"] = v
		} else {
			u["total_tokens"] = u["prompt_tokens"].(float64) + u["completion_tokens"].(float64)
		}
		out["usage"] = u
	}
	return out, nil
}

// callZenResponsesAPI 原生 /v1/responses 端点调用：与 callZenAPI 相同的
// 轮转/重试/冷却/统计语义，只差请求体形态与端点路径。
//   - stream=true 时返回原生 SSE 流（调用方自行转换呈现）
//   - stream=false 时在内部把 SSE 聚合成 chat completions 形态返回，
//     响应体可直接按 chat 结构解码
// 返回 (响应, 命中限流次数, 错误)。
//
// prompt_cache_key 绑定本次上游会话：官方 CLI 发 prompt_cache_key=<会话 ID>
// 且与 x-opencode-session 同值。网关此前每 attempt 换新 sess_ 导致同一请求
// 的 header 与 body 会话不一致；现 body 在请求体构造时绑定当次 sess_。
func callZenResponsesAPI(ctx context.Context, params map[string]any, stream bool, opts ...zenCallOpts) (*http.Response, int, error) {
	var o zenCallOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	cfg := getZenConfig()
	model, _ := params["model"].(string)
	zm, ok := resolveZenModel(model)
	if !ok {
		return nil, 0, fmt.Errorf("model %q is not a known zen model", model)
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/responses"

	zenStateMu.Lock()
	sem := zenSem
	zenStateMu.Unlock()
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, fmt.Errorf("client aborted: %w", ctx.Err())
	}
	defer func() { <-sem }()

	retries := cfg.Retries
	if retries <= 0 {
		retries = 3
	}
	delay := time.Second
	rateLimited := 0
	retryKey := ""       // 非空时重试沿用该 key（保持 key sess_ 一致）
	remapped := false    // 410 弃用迁移至多一次，防别名链循环
	sessionRetried := false // FreeTier 403 换新会话后同 key 重试至多一次

	for attempt := 0; ; attempt++ {
		proxyURL, pidx := pickUpstreamProxy()
		viaProxy := "direct"
		if proxyURL != "" {
			viaProxy = maskProxyURL(proxyURL)
		}
		// 先选 key：attempt==0 或尚无粘性 key 时轮转；重试链内沿用
		// retryKey（会话绑定要求 key 与 sess_ 一致；换 key 由下方
		// 限流/403 分支显式改写 retryKey 后 continue 实现）。
		// ZEN_PIN_KEY=n 时固定用第 n 个 key（排障直测），默认轮转。
		// pinKey（面板 Test）：整条调用链固定该 key，优先级最高——探测的
		// 结论只对被探测的 key 成立，中途换 key 会把别的 key 的成败算到它头上。
		var key string
		if o.pinKey != "" {
			key = o.pinKey
			retryKey = o.pinKey
		} else if pk := pinnedZenKey(); pk != "" {
			key = pk
			retryKey = pk
		} else if retryKey == "" {
			retryKey = pickZenKey()
		}
		key = retryKey
		sess, user, ua := "", "", ""
		if key != "" {
			// 会话粘性：同一 key 复用稳定的 sess_/UA（免费门校验 sess_ 格式，
			// 稳定身份也利于上游侧一致性），msg_ 请求 ID 仍每次随机。
			sess, user, ua = StickyZenIdentity(key)
		} else {
			sess, user, ua = kit.FreshZenIdentity()
		}
		body := buildZenResponsesBody(params, zm.ID, sess)
		bodyJSON, err := json.Marshal(body)
		if err != nil {
			return nil, rateLimited, fmt.Errorf("marshal zen responses body: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyJSON))
		if err != nil {
			return nil, rateLimited, fmt.Errorf("create zen responses request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		// x-opencode-session-id：真实 CLI 两个头都发（request.ts）；对本仓库的
		// 格式门结论无害，补上只为字节级对齐官方流量。
		setZenGateHeaders(req, sess, user)
		log.Printf("  zen upstream: model=%s responses stream=%v via=%s key=#%d attempt=%d session=%s",
			zm.ID, stream, viaProxy, keyIndex(key), attempt+1, kit.Truncate(sess, 24))

		resp, err := proxyClientFor(proxyURL).Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, rateLimited, fmt.Errorf("client aborted: %w", err)
			}
			if pidx >= 0 {
				cooldownUpstreamProxy(pidx, 2*time.Minute)
				log.Printf("  zen proxy failed (%v), cooldown exit %s for 2m", err, viaProxy)
			}
			if attempt < retries {
				log.Printf("  zen responses network error (%v), retry %d/%d after %v", err, attempt+1, retries, delay)
				if !sleepCtx(ctx, kit.WithRetryJitter(delay)) {
					return nil, rateLimited, fmt.Errorf("client aborted during retry wait")
				}
				delay = zenRetryDelay(delay)
				continue
			}
			return nil, rateLimited, fmt.Errorf("zen responses request: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			markZenKeySuccess(key)
			markZenSuccess()
			zenSessionMarkSuccess(key)
			if stream {
				return resp, rateLimited, nil
			}
			// 非流式：内部聚合为 chat completions 形态后返回
			chat, aerr := responsesSSEToChat(resp)
			if aerr != nil {
				return nil, rateLimited, aerr
			}
			chat["model"] = zm.ID
			data, merr := json.Marshal(chat)
			if merr != nil {
				return nil, rateLimited, merr
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(data)),
			}, rateLimited, nil
		}

		bodyBytes := kit.ReadBody(resp)
		resp.Body.Close()
		// 类型化错误携带状态码：端点学习只认"上游按 HTTP 拒绝了"，
		// 不认错误文本（网络错误串里也有 "no such host" 之类的词）
		apiErr := &zenHTTPError{Status: resp.StatusCode, Body: kit.Truncate(bodyBytes, 500)}

		if isRateLimited(resp.StatusCode, bodyBytes) {
			rateLimited++
			// 上游按限流处理（429，或关键词命中的 403/502/503）：标记随错误
			// 返回，消费者据此区分"限流型 403"（key 已冷却、收割机未触发）与
			// "会话死亡型 403"（FreeTier，触发收割机）。
			apiErr.RateLimited = true
			// 原样记录上游的 Retry-After（HTTP 日期还是秒数、值是多少）：
			// 冷却时长完全由它决定，而面板只能看到换算后的截止时刻。没有这行，
			// "这个 key 为什么冷却这么久"只能靠猜（实测 FreeUsageLimitError
			// 的 Retry-After 落在每日窗口复位点，见 TODO.md）。
			rawRetry := resp.Header.Get("Retry-After")
			applied := cooldownZenKey(key, parseRetryAfter(rawRetry))
			log.Printf("  zen rate limited (%d) key#%d: Retry-After=%q -> cooldown %v",
				resp.StatusCode, keyIndex(key), rawRetry, applied)
			// pinKey（面板 Test）：探测结论必须是被探测 key 自己的——立即原样
			// 上报 429（冷却已在上一行生效），不换 key 重试、不吃重试睡眠。
			if o.pinKey != "" {
				return nil, rateLimited, apiErr
			}
			if next := pickZenKey(); next != "" && next != key && !zenKeyCooling(next) {
				// 必须同步改写 retryKey：循环头 key=retryKey，只改 key 不改
				// retryKey 会让下一次迭代继续用刚冷却的旧 key，无限 429 空转。
				key = next
				retryKey = next
				log.Printf("  zen responses rate limited (%d), switching to next zen key (%d configured)", resp.StatusCode, len(cfg.Keys))
				continue
			}
			if attempt < retries {
				wait := delay
				if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > wait {
					wait = retryAfter
				}
				if wait > 30*time.Second {
					wait = 30 * time.Second
				}
				log.Printf("  zen responses rate limited (%d), retry %d/%d after %v", resp.StatusCode, attempt+1, retries, wait)
				if !sleepCtx(ctx, kit.WithRetryJitter(wait)) {
					return nil, rateLimited, fmt.Errorf("client aborted during retry wait")
				}
				delay = zenRetryDelay(delay)
				continue
			}
			markZenFail()
			return nil, rateLimited, apiErr
		}

		// 5xx 推进故障转移；pinned 探测除外（同 chat 路径：探测结论只属于
		// 这次点击，不能改写全池的路由状态）。
		if resp.StatusCode >= 500 && o.pinKey == "" {
			markZenFail()
		}
		// 会话失效（FreeTier 403 且非限流）：403 针对 key/会话，换到别的 key 只会
		// 把同一批坏状态扩散到全池，绕过不了它。正确做法是同一 key 上同步换一次
		// 会话再重试一次（sessionRetried 保证本请求至多一次）；仍 403 则该 key
		// 确实不可用——短暂冷却并快速失败，不再跨 key 扇出。
		if resp.StatusCode == http.StatusForbidden {
			// pinKey（面板 Test）：探测结论只属于这次点击，绝不触碰会话状态、
			// 不冷却、不重试——同 key 立刻原样上报。
			if o.pinKey != "" {
				return nil, rateLimited, apiErr
			}
			if !sessionRetried {
				sessionRetried = true
				log.Printf("  zen responses session rejected (403) [%s] key#%d: refreshing session locally, retry same key",
					zenSessionDesc(key), keyIndex(key))
				refreshZenSession(key)
				continue
			}
			// 换新后仍 403：该 key 不可用，冷却 5 分钟后快速失败。
			zenSessionFailed(key)
			applied := cooldownZenKey(key, 5*time.Minute)
			log.Printf("  zen responses session rejected twice (403) key#%d: key cooled %v, failing fast (no cross-key fan-out)",
				keyIndex(key), applied)
			return nil, rateLimited, apiErr
		}
		// 弃用迁移（410 + replacement / 400 unavailable）：非 pin 探测才迁移——
		// 探测的结论只属于这次点击，不得改写共享状态。
		if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusBadRequest {
			if migrated := zenHandleDeprecatedModel(resp.StatusCode, bodyBytes, zm.ID, params, o.pinKey != "", remapped); migrated {
				remapped = true
				// params["model"] 已改写为继任模型；重建 body 后就地重试。
				body = buildZenResponsesBody(params, params["model"].(string), sess)
				bodyJSON, err = json.Marshal(body)
				if err != nil {
					return nil, rateLimited, fmt.Errorf("marshal zen responses body: %w", err)
				}
				zm, _ = resolveZenModel(params["model"].(string))
				if zm == nil {
					// 极窄竞态（继任模型刚被目录修剪）：退回原错误，不裸解引用。
					return nil, rateLimited, apiErr
				}
				continue
			}
		}
		return nil, rateLimited, apiErr
	}
}

// callZenAPI 调用 zen 上游,带限流防御: 并发信号量 + 指数退避重试 + 代理冷却 + 故障计数
// 返回 (响应, 命中限流次数, 错误)。
// ctx 来自客户端请求: 客户端取消（IDE abort）时立即终止上游调用,不重试、
// 不计数、不冷却任何 key/代理 —— 客户端行为不会污染限流状态。
func callZenAPI(ctx context.Context, params map[string]any, stream bool, opts ...zenCallOpts) (*http.Response, int, error) {
	var o zenCallOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	cfg := getZenConfig()
	body := buildZenBody(params, stream)

	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal zen body: %w", err)
	}

	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"

	// 先取信号量再等待：不能在持有 zenStateMu 时阻塞在 channel 上，
	// 否则并发打满时（in-flight 调用要拿 zenStateMu 记成功/失败才能释放槽位）
	// 会形成确定性死锁。ctx 取消（客户端 abort）时不再占用槽位。
	zenStateMu.Lock()
	sem := zenSem
	zenStateMu.Unlock()
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, fmt.Errorf("client aborted: %w", ctx.Err())
	}
	defer func() { <-sem }()

	retries := cfg.Retries
	if retries <= 0 {
		retries = 3
	}
	delay := time.Second
	rateLimited := 0
	remapped := false       // 410 弃用迁移至多一次，防别名链循环
	sessionRetried := false // FreeTier 403 换新会话后同 key 重试至多一次

	for attempt := 0; ; attempt++ {
		// 代理轮转（round_robin 默认）: 每次上游尝试显式挑选出口,
		// 冷却中的代理被跳过; 未配置代理时直连。
		proxyURL, pidx := pickUpstreamProxy()
		viaProxy := "direct"
		if proxyURL != "" {
			viaProxy = maskProxyURL(proxyURL)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyJSON))
		if err != nil {
			return nil, rateLimited, fmt.Errorf("create zen request: %w", err)
		}
		// 客户端身份：会话粘性——同一 key 复用稳定的会话 ID/UA（免费门是
		// sess_ 格式校验而非服务端绑定，粘住身份仍利于上游侧一致性），
		// msg_ 请求 ID 每次随机。
		// ZEN_PIN_KEY=n 时固定用第 n 个 key（单 key 直测/排障），默认轮转。
		// pinKey（面板 Test）优先且不碰轮转指针：一次探测不该挪动 round-robin
		// 的游标，否则每点一次 Test 都会让下一次正常请求换 key。
		var key string
		if o.pinKey != "" {
			key = o.pinKey
		} else {
			key = pickZenKey()
			if pk := pinnedZenKey(); pk != "" {
				key = pk
			}
		}
		sess, user, ua := StickyZenIdentity(key)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", ua)
		// x-opencode-session-id：真实 CLI 两个头都发（request.ts），见 responses 路径。
		setZenGateHeaders(req, sess, user)

		log.Printf("  zen upstream: model=%s stream=%v msgs=%d via=%s key=#%d attempt=%d session=%s",
			body["model"], stream, getMsgCount(params), viaProxy, keyIndex(key), attempt+1, kit.Truncate(sess, 24))

		resp, err := proxyClientFor(proxyURL).Do(req)
		if err != nil {
			// 客户端取消: 立即返回,不重试不冷却不计故障
			if ctx.Err() != nil {
				return nil, rateLimited, fmt.Errorf("client aborted: %w", err)
			}
			// 隧道层失败才冷却出口: 拨号/握手/连接被重置。冷却从 5m 缩到 2m ——
			// 上游过载时 CF 重置连接的表现与死代理相同,5~10 分钟的冷却会让
			// 几次慢模型测试就毒化整个池;2 分钟仍能跳过真死代理,又快速自愈。
			if pidx >= 0 {
				cooldownUpstreamProxy(pidx, 2*time.Minute)
				log.Printf("  zen proxy failed (%v), cooldown exit %s for 2m", err, viaProxy)
			}
			// 网络错误:退避重试(不计入故障转移,瞬时可恢复);ctx 取消时中断等待
			if attempt < retries {
				log.Printf("  zen network error (%v), retry %d/%d after %v", err, attempt+1, retries, delay)
				if !sleepCtx(ctx, kit.WithRetryJitter(delay)) {
					return nil, rateLimited, fmt.Errorf("client aborted during retry wait")
				}
				delay = zenRetryDelay(delay)
				continue
			}
			return nil, rateLimited, fmt.Errorf("zen request: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			markZenKeySuccess(key)
			markZenSuccess()
			zenSessionMarkSuccess(key)
			return resp, rateLimited, nil
		}

		bodyBytes := kit.ReadBody(resp)
		resp.Body.Close()
		// 类型化错误携带状态码（同 callZenResponsesAPI）：端点学习只看状态码
		apiErr := &zenHTTPError{Status: resp.StatusCode, Body: kit.Truncate(bodyBytes, 500)}

		if isRateLimited(resp.StatusCode, bodyBytes) {
			rateLimited++
			// 上游按限流处理（标记随错误返回，同 responses 路径）。
			apiErr.RateLimited = true
			// 不冷却出口代理: 代理成功送达了 HTTP 响应,它没有故障。限流是 zen
			// 对 key/身份/IP 组合的判定,把出口毒化 10 分钟只会让上游繁忙期
			// (慢模型 503/429)把整个池打瘫;下一次尝试的轮转自然换到下一出口,
			// key 冷却 + 轮转已足够分摊负载。
			// 冷却当前 key；若还有其他未冷却 key 则立即切换重试（不睡眠）。
			// Retry-After 原样入日志（同 responses 路径）。
			rawRetry := resp.Header.Get("Retry-After")
			applied := cooldownZenKey(key, parseRetryAfter(rawRetry))
			log.Printf("  zen rate limited (%d) key#%d: Retry-After=%q -> cooldown %v",
				resp.StatusCode, keyIndex(key), rawRetry, applied)
			// pinKey（面板 Test）：立即原样上报 429（冷却已生效），不换 key。
			if o.pinKey != "" {
				return nil, rateLimited, apiErr
			}
			if next := pickZenKey(); next != "" && next != key && !zenKeyCooling(next) {
				log.Printf("  zen rate limited (%d), switching to next zen key (%d configured)", resp.StatusCode, len(cfg.Keys))
				continue
			}
			if attempt < retries {
				// Retry-After 封顶 30s：更长的等待没有意义（槽位被占、客户端早已离开）
				wait := delay
				if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > wait {
					wait = retryAfter
				}
				if wait > 30*time.Second {
					wait = 30 * time.Second
				}
				log.Printf("  zen rate limited (%d), retry %d/%d after %v", resp.StatusCode, attempt+1, retries, wait)
				if !sleepCtx(ctx, kit.WithRetryJitter(wait)) {
					return nil, rateLimited, fmt.Errorf("client aborted during retry wait")
				}
				delay = zenRetryDelay(delay)
				continue
			}
			markZenFail()
			return nil, rateLimited, apiErr
		}

		// 非 2xx：只有限流信号或服务端错误才推进全局故障转移，
		// 客户端侧 400/401（提示词超限、key 配错）不应污染 failover 状态。
		// pinned 探测除外：它是管理员对单个 key 的主动探测，其结论（包括上游
		// 5xx）只属于这次点击——连点几次 Test 撞上上游 500，绝不能把全部正常
		// 流量切去 cline 池。
		if resp.StatusCode >= 500 && o.pinKey == "" {
			markZenFail()
		}
		// 会话失效（FreeTier 403 且非限流）：403 针对 key/会话，换到别的 key 只会
		// 把坏状态扩散到全池。同一 key 上同步换一次会话再重试一次（sessionRetried
		// 保证至多一次）；仍 403 则冷却该 key 并快速失败，不再跨 key 扇出。
		if resp.StatusCode == http.StatusForbidden {
			// 记录会话年龄：合法格式会话 403 是"会话寿命/额度窗口到期或
			// 门禁收紧"的证据；换新频率升高即是 tripwire。
			// pinKey（面板 Test）：探测结论只属于这次点击，不触碰会话状态。
			if o.pinKey != "" {
				log.Printf("  zen chat session rejected (403) [%s], key#%d (probe: no state change)", zenSessionDesc(key), keyIndex(key))
				return nil, rateLimited, apiErr
			}
			if !sessionRetried {
				sessionRetried = true
				log.Printf("  zen chat session rejected (403) [%s] key#%d: refreshing session locally, retry same key",
					zenSessionDesc(key), keyIndex(key))
				refreshZenSession(key)
				continue
			}
			zenSessionFailed(key)
			applied := cooldownZenKey(key, 5*time.Minute)
			log.Printf("  zen chat session rejected twice (403) key#%d: key cooled %v, failing fast (no cross-key fan-out)",
				keyIndex(key), applied)
			return nil, rateLimited, apiErr
		}
		// 弃用迁移（410 + replacement / 400 unavailable）：非 pin 探测才迁移；
		// 迁移成功则重建 body 就地重试（至多一次）。
		if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusBadRequest {
			if migrated := zenHandleDeprecatedModel(resp.StatusCode, bodyBytes, body["model"].(string), params, o.pinKey != "", remapped); migrated {
				remapped = true
				body = buildZenBody(params, stream)
				bodyJSON, err = json.Marshal(body)
				if err != nil {
					return nil, rateLimited, fmt.Errorf("marshal zen body: %w", err)
				}
				continue
			}
		}
		return nil, rateLimited, apiErr
	}
}

// sleepCtx ctx 感知的睡眠；返回 false 表示等待期间被取消。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// keyIndex key 在池中的序号（日志用）
func keyIndex(key string) int {
	for i, k := range getZenConfig().Keys {
		if k == key {
			return i + 1
		}
	}
	return 0
}

// zenRetryDelay 指数退避的下一步延迟，封顶 30s。
//
// 必须封顶：Retries 来自面板配置（.zen-config.json 亦可手改），裸翻倍到约 63 次
// 就会让 int64 纳秒溢出为负数，而 time.NewTimer(负值) 立即触发 —— 重试退化成
// 无退避热循环，把并发槽位全部耗在空转上。封顶同时也限住了网络错误重试的等待
// 时长（该路径不做 Retry-After 修正，直接用 delay）。
func zenRetryDelay(d time.Duration) time.Duration {
	d *= 2
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

func zenModelList() []map[string]any {
	initZenModels()
	// 别名表先快照（zenDepMu 不与 zenModelsMu 嵌套）。
	depAliases := map[string]string{}
	zenDepMu.Lock()
	for dead, e := range zenDepAliases {
		depAliases[dead] = e.Replacement
	}
	zenDepMu.Unlock()
	zenModelsMu.RLock()
	out := make([]map[string]any, 0, len(zenModels))
	for _, m := range zenModels {
		if !isZenFreeModel(m) {
			continue
		}
		cp := *m
		entry := map[string]any{
			"id":        cp.ID,
			"context":   cp.Context,
			"output":    cp.Output,
			"source":    cp.Source,
			"upstream":  cp.Upstream,
			"toolCall":  cp.ToolCall,
			"reasoning": cp.Reasoning,
			"attach":    cp.Attach,
		}
		if repl, dep := depAliases[cp.ID]; dep {
			entry["replacedBy"] = repl
		}
		out = append(out, entry)
	}
	zenModelsMu.RUnlock()
	return out
}

// opencodeModelsRegistry 公共模型目录（官方 CLI 同源，无需认证；
// 替代 zen /v1/models，后者用 "public" key 恒失败，只能靠种子兜底）。
// 测试用 zenRegistryURL 覆写指向 httptest 服务。
const opencodeModelsRegistry = "https://models.opencode.ai/api.json"

// maxRegistryBytes 目录响应体积上限（8 MiB）。正常目录约几百 KB，设置上限
// 只为防止异常响应把内存吃穿；超限按"不可达"处理。
const maxRegistryBytes = 8 << 20

var zenRegistryURL = opencodeModelsRegistry

// zenModelOverlay 公共目录里比 zen 真源多的限额/旗标字段。
type zenModelOverlay struct {
	Context, Output     int
	ToolCall, Reasoning bool
	Attachment          bool
	// NPM 该模型的 SDK 包名（per-model provider.npm；解析规则与官方 CLI
	// fromModelsDevModel 一致：model.provider?.npm ?? provider.npm ?? 默认）。
	// 端点路由的种子信号，见 zenNPMToUpstream。
	NPM string
}

// fetchZenRegistry 拉取公共目录 api.json，返回（限额 overlay、价格门集合、
// 是否可达）。价格门 = cost.input==0 && cost.output==0 且 status 非
// deprecated。模型 ID 以条目内 id 优先、map key 兜底。目录不可达返回
// (空, 空, false)，调用方按 fail-open 处理。
// 唯一实现：syncZenModels 使用（会话铸造改本地后不再有第二消费者），
// 避免两处价格门逻辑漂移。
func fetchZenRegistry() (map[string]zenModelOverlay, map[string]bool, bool) {
	overlay := map[string]zenModelOverlay{}
	freeGate := map[string]bool{}
	oreq, err := http.NewRequest("GET", zenRegistryURL, nil)
	if err != nil {
		return overlay, freeGate, false
	}
	oreq.Header.Set("User-Agent", "opencode/latest/cli")
	client := &http.Client{Timeout: 25 * time.Second}
	oresp, err := client.Do(oreq)
	if err != nil {
		return overlay, freeGate, false
	}
	defer oresp.Body.Close()
	if oresp.StatusCode != 200 {
		return overlay, freeGate, false
	}
	var payload map[string]struct {
		NPM    string `json:"npm"`
		Models map[string]struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			ToolCall   bool   `json:"tool_call"`
			Reasoning  bool   `json:"reasoning"`
			Attachment bool   `json:"attachment"`
			NPM        string `json:"npm"`
			Provider   struct {
				NPM string `json:"npm"`
			} `json:"provider"`
			Limit struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			// Cost 用指针以区分"cost 键缺失"与"cost 全为 0"：缺失时不能当作
			// 免费（fail-closed），否则目录漏填价格字段的模型会被误判入免费池。
			Cost *struct {
				Input  float64 `json:"input"`
				Output float64 `json:"output"`
			} `json:"cost"`
		} `json:"models"`
	}
	// 体积上限：目录正常约几百 KB，恶意/异常响应不应无界读入内存。
	// 超限即视为不可达（fail-open 的调用方保留上次目录）。
	if json.NewDecoder(io.LimitReader(oresp.Body, maxRegistryBytes+1)).Decode(&payload) != nil {
		return overlay, freeGate, false
	}
	prov, ok := payload["opencode"]
	if !ok {
		return overlay, freeGate, false
	}
	for id, m := range prov.Models {
		if m.ID != "" {
			id = m.ID
		}
		// npm 解析优先级 = 官方 CLI fromModelsDevModel：
		// 模型 provider.npm > 模型顶层 npm > 提供方 npm > 默认（openai-compatible）。
		npm := m.Provider.NPM
		if npm == "" {
			npm = m.NPM
		}
		if npm == "" {
			npm = prov.NPM
		}
		if npm == "" {
			npm = "@ai-sdk/openai-compatible"
		}
		overlay[id] = zenModelOverlay{
			Context: m.Limit.Context, Output: m.Limit.Output,
			ToolCall: m.ToolCall, Reasoning: m.Reasoning, Attachment: m.Attachment,
			NPM: npm,
		}
		if m.Cost != nil && m.Cost.Input == 0 && m.Cost.Output == 0 &&
			!strings.Contains(strings.ToLower(m.Status), "deprecat") {
			freeGate[id] = true
		}
	}
	return overlay, freeGate, true
}

// zenNPMToUpstream 把 SDK 包名映射到上游路由种子（与官方 CLI 的 resolveSDK
// 语义一致）。返回 (upstream, known)：
//   - "@ai-sdk/openai"          → "responses"（原生 POST {base}/responses）
//   - "@ai-sdk/openai-compatible" → ""（默认 POST {base}/chat/completions）
//   - "@ai-sdk/anthropic" / "@ai-sdk/google" → ("", false)：免费层目前没有
//     存活的这类模型；映射记下来但不做原生分发，交端点学习器自行纠正，
//     日志留 warning（将来出现原生模型再补真分发）。
//   - 其他未知值 → ("", false)：绝不猜，让学习器用上游拒绝信号决定。
func zenNPMToUpstream(npm string) (string, bool) {
	switch npm {
	case "@ai-sdk/openai":
		return "responses", true
	case "@ai-sdk/openai-compatible":
		return "", true
	case "@ai-sdk/anthropic", "@ai-sdk/google":
		return "", false
	default:
		return "", false
	}
}

// zenDesiredFromLive 推导可服务的免费模型集（纯函数，便于测试）：
// 目录可达时以价格门为唯一权威（= 官方 CLI 读的同一份数据的 cost 0/0 子集）；
// 目录不可达时返回错误，调用方保留旧表（种子条目在首次成功同步前仍可服务）。
// 收割机时代的 CLI 成员表兜底已随内嵌 CLI 一起移除——离线启动不再有第三方
// 成员表来源。
func zenDesiredFromLive(registryOK bool, freeGate map[string]bool) (map[string]bool, error) {
	if registryOK && len(freeGate) > 0 {
		desired := map[string]bool{}
		for id := range freeGate {
			desired[id] = true
		}
		return desired, nil
	}
	return nil, fmt.Errorf("model catalog unavailable: registry unreachable")
}

// syncZenModels 同步免费模型（live 为准，种子只做冷启动兜底）。判定链：
//  1. 免费资格（唯一权威）：公共目录 models.opencode.ai 的 opencode 条目
//     cost.input==0 && cost.output==0 且 status 无 deprecated 字样。无 free
//     后缀但价格为 0 的模型（big-pickle）因此入选；status=deprecated 的
//     *-free 条目即使价格为 0 也排除（上游已报 Model unavailable）。
//     官方 CLI 读的是同一份目录，价格门就是"真实模型列表"本身。
//  2. 目录不可达时：返回错误并保留旧表（冷启动前种子仍可服务）。
//     内嵌 CLI 的成员表兜底已随收割机一起移除。
//  3. 限额 overlay：目录 limit.context/output + tool_call / reasoning /
//     attachment 旗标 + per-model provider.npm（端点种子，见 applyZenCatalog）。
// 增删一律以本次 live 结果为准：表中的种子条目若不再通过价格门会被移除
//（种子只在首次成功同步之前的冷启动/离线状态提供兜底，不参与日常增删）。
// 返回新增模型数。
func syncZenModels() (int, error) {
	initZenModels()
	overlay, freeGate, registryOK := fetchZenRegistry()
	desired, derr := zenDesiredFromLive(registryOK, freeGate)
	if derr != nil {
		return 0, derr
	}
	added := applyZenCatalog(desired, overlay)
	pruned := pruneZenModelsTo(desired)
	// 重新敷用已学习的端点覆盖：新出现的模型（以及被 prune 后重建的条目）不在
	// 启动时 loadZenEndpoints 的视野里，不敷用就会每次重启重新探测一遍。
	reapplyLearnedEndpoints()
	// 目录刷新成功：解除上游 400 "Model is unavailable" 的死亡标记（此前可能
	// 只是目录滞后），并清理指向已不存在模型的别名/标记。
	zenClearDeadModels()
	pruneZenDeadAndAliases(desired)
	if added > 0 {
		log.Printf("zen model sync: %d new free model(s) from live catalog", added)
	}
	if pruned > 0 {
		log.Printf("zen model sync: pruned %d model(s) no longer free/online", pruned)
	}
	return added, nil
}

// applyZenCatalog 把 live 结果写进模型表：存量条目就地更新（保留别名；
// Upstream 只在学习器未曾定论时按目录 npm 重播种），新条目按 overlay 限额
// 与 npm 路由建立。返回新增数量。
func applyZenCatalog(desired map[string]bool, overlay map[string]zenModelOverlay) int {
	zenModelsMu.Lock()
	defer zenModelsMu.Unlock()
	added := 0
	for id := range desired {
		ov := overlay[id]
		if cur, ok := zenModels[id]; ok {
			// 存量条目：overlay 限额覆盖种子/上次估算；别名沿用，Source 升级
			// 为 live。改副本再挂回（copy-on-write）：resolveZenModel 返回的
			// 指针在释放 RLock 后仍被请求路径读取，就地改写会与其裸读竞争。
			next := *cur
			if ov.Context > 0 {
				next.Context = ov.Context
			}
			if ov.Output > 0 {
				next.Output = ov.Output
			}
			if ov != (zenModelOverlay{}) {
				next.ToolCall, next.Reasoning, next.Attach = ov.ToolCall, ov.Reasoning, ov.Attachment
			}
			// npm 路由重播种：仅当该模型尚无明确端点决策时按目录 npm 刷新。
			// "决策"= Upstream 非空，或学习器已定论（Source=learned，含 chat
			// 定论：learning 后 Upstream 归零但不得再被 npm 播回 responses）。
			// load/reapplyLearnedEndpoints 恢复学习条目，这里不得回翻；
			// muse-spark 等种子提示同样保留。
			if next.Upstream != "" || next.Source == "learned" {
				// 已有决策，保持
			} else if ov.NPM != "" {
				if up, known := zenNPMToUpstream(ov.NPM); known && up != "" {
					next.Upstream = up
				}
			}
			// Source 保留"learned"标记：它既表示"学习器已定论，npm 不得重播种"，
			// 也是 saveZenEndpoints 写盘时的筛选条件（只存 learned 条目）。这里若
			// 无条件抹成 "live"，sync 结束前并发的 saveZenEndpoints 会把该条目从
			// 学习文件里丢掉，reapplyLearnedEndpoints 也因此失去依据——学习决策
			// 只在 reapply 恰好晚于本函数的时序下才存活。保留标记使契约显式成立。
			if next.Source != "learned" {
				next.Source = "live"
			}
			zenModels[id] = &next
			continue
		}
		// 跳过与免费别名冲突的 ID（如付费 id 撞别名），保证别名解析不被覆盖
		if _, conflict := zenAliases[id]; conflict {
			continue
		}
		ctxN, outN := ov.Context, ov.Output
		if ctxN <= 0 {
			ctxN = 200000
		}
		if outN <= 0 {
			outN = 32768
		}
		// 端点种子：按目录 npm 确定性路由（muse-spark 类 responses 模型不再
		// 靠名字启发式）。未知/暂不支持的原生 npm 交给端点学习器，绝不猜。
		upstream := ""
		if npm := ov.NPM; npm != "" {
			if up, known := zenNPMToUpstream(npm); known {
				upstream = up
			} else {
				log.Printf("zen catalog: model=%s has unsupported provider npm %q; endpoint learner will decide", id, npm)
			}
		}
		zenModels[id] = &ZenModel{
			ID:        id,
			Context:   ctxN,
			Output:    outN,
			Source:    "live",
			Upstream:  upstream,
			ToolCall:  ov.ToolCall,
			Reasoning: ov.Reasoning,
			Attach:    ov.Attachment,
		}
		added++
	}
	return added
}

// pruneZenModelsTo 修剪掉不在 live 结果中的条目（含种子）：种子只是首次成功
// 同步前的冷启动兜底，同步成功后以 live 为准，避免已下线/已转付费的模型继续
// 挂出（别名一并移除）。防御：live 残表（接口抖动/改版）不清空本地列表 ——
// 要求 live 结果不少于现有条数的一半。返回移除数量。
func pruneZenModelsTo(desired map[string]bool) int {
	zenModelsMu.Lock()
	defer zenModelsMu.Unlock()
	current := len(zenModels)
	if len(desired) == 0 || len(desired)*2 < current {
		if current > 0 {
			log.Printf("zen model sync: live list too small (%d live vs %d known), skipping prune", len(desired), current)
		}
		return 0
	}
	pruned := 0
	for id, m := range zenModels {
		if desired[id] {
			continue
		}
		delete(zenModels, id)
		for _, a := range m.Aliases {
			delete(zenAliases, a)
		}
		pruned++
	}
	return pruned
}

// startZenModelsRefresher 定时同步 zen 模型列表(默认 10 分钟)
func startZenModelsRefresher() {
	go func() {
		if _, err := syncZenModels(); err != nil {
			log.Printf("zen model sync: failed (%v), keeping the current list", err)
		}
		ticker := time.NewTicker(10 * time.Minute)
		for range ticker.C {
			cfg := getZenConfig()
			if !cfg.Enabled {
				continue
			}
			if added, err := syncZenModels(); err != nil {
				log.Printf("zen model sync: failed (%v)", err)
			} else if added > 0 {
				log.Printf("zen model sync: %d new models from official feed", added)
			}
		}
	}()
}
