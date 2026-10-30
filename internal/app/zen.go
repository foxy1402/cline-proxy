package app

import (
	"bufio"
	"bytes"
	"cline-go-proxy/internal/kit"
	"context"
	"encoding/json"
	"errors"
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

// zenAPIBase 上游基地址（编译期默认值）。
//
// 2026-10-30 抓包更正：官方目录（models.opencode.ai）里 opencode provider 的
// api 是 /inference/openai/v1，全部 8 个免费模型的 provider.api 也一致；真实
// CLI 2.1.4 发往 /inference/openai/v1/chat/completions。此前这里写死的
// /inference/v1 来自 2026-10-15 的一次观测，上游改了路径而仓库没跟——差一个
// 路径段。默认值已同步，并且 syncZenModels 会按目录里的 provider.api 自愈
// （见 applyZenRegistryBase），不再依赖某次人工观测。
const zenAPIBase = "https://opencode.ai/inference/openai/v1"

// zenStaleBase 2026-10-15 ~ 2026-10-30 期间的（错误的）编译期默认值。
// 仅用于识别并迁移存量的 .zen-config.json：用户没改过 baseURL 时它被写进了
// 配置文件，不迁移的话旧安装会一直停在短路径上。
const zenStaleBase = "https://opencode.ai/inference/v1"

// zenLegacyBase 已废弃的旧基地址，仅用于存量配置迁移识别。
const zenLegacyBase = "https://opencode.ai/zen/v1"

// max429KeySwitches 一次客户端请求因 429 最多切换几个 key 的硬上限。
// 换 key 是本次调用里唯一不可逆的操作：cooldownZenKey 会把该 key 冻结到
// zenRateLimitCooldown。若不设上限，一个大请求 429 后沿池顺序把每个 key 逐个
// 冷却，整池在几秒内报废——2026-10-30 实测：单个 291 条消息的请求 4 秒内冷却
// 了 5 个 key。上限取 4（实际生效值再与 retries 取小），保证一次坏请求最多
// 牺牲少数几个 key，而不是整个池。
const max429KeySwitches = 4

// zenRateLimitCooldown zen key 因 429 冷却的时长上限。
//
// 上游的 Retry-After 是"距下一个 00:00 UTC"的倒计时，不是真实恢复时间：实测
// 最后一个 429 报 RA=20h46m，而 18 分钟后同一 key 就恢复 200 了。按 RA 冷藏会
// 让多 key 池里的 key 白停一整天。取 20 分钟：足够把流量让给别的 key，又能在
// 一次瞬时节流过去后很快自愈。（整池都冷却时 pickZenKey 仍会轮转试探，不会
// 出现无 key 可用。）
const zenRateLimitCooldown = 20 * time.Minute

// legacyCompactBuffer 压缩 buffer 的历史默认值，仅用于存量配置迁移。
const legacyCompactBuffer = 20000

// defaultCompactBuffer 压缩预留缓冲（token）。20000 的旧默认值对"客户端自身
// 上限低于模型窗口"的场景无效：阈值 = ctx - max(output, buffer)，200k 窗口、
// 32k 输出时阈值 168k，而客户端（OpenClaw 等）可能在 128k 就封顶——压缩永远
// 不会触发，网关把满载请求原样送上游。
//
// 80000 让 200k/32k 这类模型的阈值降到 120k，落进 128k 客户端上限之内；
// 大窗口模型（beam 1M、nemotron-lightning 262k/262k）仍由 max(output, buffer)
// 主导，行为不变。压缩本身保留 8000 token 尾部（keepTokens），所以抬高 buffer
// 只是"更早开始摘要"，不会让上下文变浅。
const defaultCompactBuffer = 80000

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
//
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
	// OrgID：OAuth 账户的组织 ID（x-opencode-org-id）。CLI v2 在 OAuth
	// 账户流量中发该头，原始 API key 流量中没有。网关无法从 key 字符串
	// 推导它，空值即省略（正确）；使用 OAuth token 作 key 时，应从
	// 账户元数据填入，否则上游看到 OAuth token 却无 org 头，与官方
	// 客户端形态不一致。绝不编造。
	OrgID string `json:"orgID,omitempty"`
	BaseURL         string           `json:"baseURL"`
	Proxies         []string         `json:"proxies"`         // http(s)/socks5 代理,轮询出口
	ProxyStrategy   string           `json:"proxyStrategy"`   // round_robin / random / fill
	MaxConcurrency  int              `json:"maxConcurrency"`  // zen 上游最大并发,防 worker 瞬时超限,默认 8
	Retries         int              `json:"retries"`         // 限流/网络错误重试次数,默认 3
	Failover        bool             `json:"failover"`        // zen 连续失败后故障转移到 cline 账号池,默认 true
	FailoverCount   int              `json:"failoverCount"`   // 触发故障转移的连续失败次数,默认 3
	FailoverMinutes int              `json:"failoverMinutes"` // 故障转移窗口(分钟),默认 5
	Compaction      zenCompactConfig `json:"compaction"`
	// SessionRotateMinutes 粘性会话本地轮换周期（分钟），默认 120（2 小时）。
	// 0 = 永不轮换（沿用旧行为，长跑后首个请求会因会话老化而变慢）。
	SessionRotateMinutes int `json:"sessionRotateMinutes"`
	// PinProxyPerKey 每个 key/账号固定一个出口代理（默认 true）。指针类型：
	// nil = 旧配置文件没有该键 → 按默认开启处理（见 pinProxyPerKeyEnabled）。
	// 关闭后恢复按请求轮转（zen 按 key 的 IP 指纹会重新出现，仅排障用）。
	PinProxyPerKey *bool `json:"pinProxyPerKey,omitempty"`
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
		// 会话轮换周期：缺少该键的旧配置文件保持此默认（2 小时）。
		// 显式写 0 表示关闭轮换（见 SessionRotateMinutes 注释）。
		SessionRotateMinutes: defaultSessionRotateMinutes,
		// 每 key 固定出口：2026-10-21 起 zen 按 key 记录出口 IP，轮转会
		// 让整池 key 被判可疑，故默认开启（无代理时自动退回直连）。
		PinProxyPerKey: boolPtr(true),
		Compaction: zenCompactConfig{
			Auto:       true,
			Buffer:     defaultCompactBuffer,
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
	// 故障转移只在"有地方可去"时才有意义。cline 池为空时把 zen 判为 degraded
	// 会把请求路由到一个没有账号的池，客户端拿到 401 "No accounts in pool" ——
	// 比原来的 429 更糟且更难懂（2026-10-30 实测：8 并发打满 3 次失败阈值后，
	// 后续所有请求都变成 401，看起来像"zen 彻底坏了"）。
	// 池为空时直接不记失败：zen 自己的 429/冷却逻辑仍在工作，没有可转移的目标。
	if total, active := poolStatusSnapshot(); total == 0 || active == 0 {
		return
	}
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

// zenClientErrorStatus 把 zen 上游的错误映射成回给客户端的 HTTP 状态码。
//
// 默认 502：网关确实没能从上游拿到 2xx，语义是"坏网关"。
// 限流例外：上游 429 是"我这个 key 用完了，你等会再来"，原因在客户端可以
// 应对的一侧（退避重试、换模型）。回 502 会让 IDE 认为网关坏了——实测
// OpenCode/Claude 系客户端对 502 的处置是标记服务不可用，而对 429 会按
// Retry-After 退避重试。故 429 原样透传状态码。
func zenClientErrorStatus(err error) int {
	var he *zenHTTPError
	if errors.As(err, &he) && he.Status == http.StatusTooManyRequests {
		return http.StatusTooManyRequests
	}
	return http.StatusBadGateway
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
	// ZEN_SESSION_ROTATE_MINUTES 覆盖会话轮换周期（分钟，0=关闭）。
	// 与 POOL_STRATEGY 同语义：env 显式设置时总是覆盖持久化配置。
	if n, ok := envInt("ZEN_SESSION_ROTATE_MINUTES"); ok {
		cfg.SessionRotateMinutes = n
		log.Printf("zen session rotation interval set from env: %d min", n)
	}
	// ZEN_ORG_ID：OAuth 账户的组织 ID，写入 x-opencode-org-id 头。
	// 原始 API key 里没有这个信息，无法推导；只有用 OAuth token 作 key 时
	// 才需要显式配置。未设置即省略该头（上游对原始 key 流量也不发）。
	if org := envStr("ZEN_ORG_ID"); org != "" {
		cfg.OrgID = org
		log.Printf("zen org id set from env: %s", org)
	}
	normalizeZenKeys(cfg)
	if cfg.BaseURL == "" {
		cfg.BaseURL = zenAPIBase
	}
	// 存量配置迁移：旧默认 /zen/v1 对匿名免费层已失效（恒 403），
	// 只迁移恰好等于旧默认值的配置；用户显式改过的其它地址不动。
	if strings.TrimRight(cfg.BaseURL, "/") == zenLegacyBase {
		cfg.BaseURL = zenAPIBase
		log.Printf("zen baseURL migrated: %s -> %s (legacy zen endpoint rejects anonymous free-tier traffic)", zenLegacyBase, zenAPIBase)
	}
	// 存量配置迁移：2026-10-15 的错误默认值 /inference/v1 少了 /openai 一段。
	// 同样只迁移恰好等于该旧默认值的配置——显式填过自定义地址的不动。
	if strings.TrimRight(cfg.BaseURL, "/") == zenStaleBase {
		cfg.BaseURL = zenAPIBase
		log.Printf("zen baseURL migrated: %s -> %s (upstream moved the free-tier path under /openai)", zenStaleBase, zenAPIBase)
	}
	// 存量配置迁移：压缩 buffer 旧默认 20000 对"客户端上限低于模型窗口"的
	// 场景无效（200k/32k 模型 -> 阈值 168k，而客户端可能 128k 就封顶，
	// 压缩永不触发）。只迁移恰好等于旧默认值的配置；显式调过的不动。
	if cfg.Compaction.Buffer == legacyCompactBuffer {
		cfg.Compaction.Buffer = defaultCompactBuffer
		log.Printf("zen compaction buffer migrated: %d -> %d (old value never fired below a 128k client cap)",
			legacyCompactBuffer, defaultCompactBuffer)
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
	// key/账号/代理列表任一变化都要重算出口绑定：新 key 分配空闲代理、
	// 被移除的 key 释放代理、代理被删则相关 key 变回"无出口"（从而被过滤）。
	reconcileEgressBindings()
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
// 返回 "" 表示当前没有配置任何 key，或（绑定策略生效时）没有任何 key 分到了
// 出口代理——后者由调用方转成明确的容量错误，绝不退回直连或匿名。
//
// 绑定策略（egress.go）：一个 key 固定一个出口代理。代理数少于 key 数时，
// 多出来的 key 没有出口，必须被过滤掉——它们的流量若继续发出（直连或借用
// 别人的出口），上游就会把"一个 key 多个 IP"的指纹重新打上，这正是要消除的。
// "public"（无凭据的匿名哨兵）不受约束：没有 key 可关联，出口随便轮转。
func pickZenKey() string {
	keys := getZenConfig().Keys
	if len(keys) == 0 {
		return ""
	}
	// 先取绑定快照（egressMu），再取 live 会话集合（zenSessMu），最后进
	// zenKeyMu：三把锁不嵌套，避免与换新路径形成锁序反转。
	var bound map[string]bool
	if egressBindingActive() {
		bound = make(map[string]bool, len(keys))
		for _, k := range keys {
			if k == "public" {
				bound[k] = true
				continue
			}
			_, bound[k] = egressProxyForZen(k)
		}
	}
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
	eligible := func(k string) bool { return bound == nil || bound[k] }
	// 第一轮：非冷却 + 有 live 会话。本地铸造后所有 key 生而有合法会话，
	// 此轮几乎总是命中；仅在文件加载失败等异常时退化到第二轮。
	for i := 0; i < len(keys); i++ {
		idx := (zenKeyIdx + i) % len(keys)
		k := keys[idx]
		if _, cooling := zenKeyCool[k]; !cooling && live[k] && eligible(k) {
			zenKeyIdx = (idx + 1) % len(keys)
			return k
		}
	}
	// 第二轮：非冷却（与 live 无关——异常窗口内仍须发请求，
	// 上游 403 是唯一可用信号，恢复路径随后本地换新会话）
	for i := 0; i < len(keys); i++ {
		idx := (zenKeyIdx + i) % len(keys)
		k := keys[idx]
		if _, cooling := zenKeyCool[k]; !cooling && eligible(k) {
			zenKeyIdx = (idx + 1) % len(keys)
			return k
		}
	}
	// 全部冷却中：仍按轮转返回（保持请求流动，上游会再次限流）。
	// 绑定过滤仍然生效——没出口的 key 连"冷却重试"都不该发出去。
	for i := 0; i < len(keys); i++ {
		idx := (zenKeyIdx + i) % len(keys)
		k := keys[idx]
		if eligible(k) {
			zenKeyIdx = (idx + 1) % len(keys)
			return k
		}
	}
	return ""
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
	// Retry-After 不可信，必须封顶（2026-10-30 实测）。
	//
	// zen 的 Retry-After 恒等于"距下一个 00:00 UTC 的秒数"（实测 75020/74810/
	// 73790…，全部落在同日 23:59:5x），它是每日窗口的复位倒计时，不是这次限流
	// 的真实恢复时间。实际观测：最后一个 429（RA=20h46m）之后 **18 分钟**就恢复
	// 正常 200 了——按 RA 冷藏 key 会让 6 key 池里的 5 个 key 白停一整天，而池子
	// 其实已经可用。
	//
	// 因此 zen key 的冷却以 zenRateLimitCooldown 封顶：够久到把流量让给别的 key，
	// 又不至于让一次瞬时节流把整池停到午夜。若整个池都在冷却，pickZenKey 仍会
	// 轮转返回一个 key 继续试探，所以封顶不会造成"无 key 可用"。
	if d > zenRateLimitCooldown {
		d = zenRateLimitCooldown
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

// zenAnyKeyRotatable 是否还有"当前 key 之外、且未在冷却"的 key 可以马上换过去。
// 决定 429 之后该不该睡眠重试：没有可换的 key 时，睡 30s 再打同一个已冷却的
// key 只是在挂住客户端的连接（实测单 key 池一次请求挂 101s 才返回 429）。
// 与 pickZenKey 不同：这里不推进 round-robin 游标，只是只读探测。
func zenAnyKeyRotatable(cur string) bool {
	for _, k := range getZenConfig().Keys {
		if k != "" && k != cur && !zenKeyCooling(k) {
			return true
		}
	}
	return false
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
	// 出口绑定同样先快照：egress 路径取 zenConfigMu/egressMu，不能与 zenKeyMu 嵌套。
	egress := zenEgressSnapshotOf(keys)
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
		// 出口绑定：面板必须能看出"这个 key 为什么不在轮转里"。
		// 没有绑定的 key 在绑定策略下等于停用（见 pickZenKey）。
		// 快照已在进 zenKeyMu 之前算好（egress.go 的锁不与 zenKeyMu 嵌套）。
		if e := egress[i]; e.proxy != "" {
			st["proxy"] = maskProxyURL(e.proxy)
			st["proxyIndex"] = e.index
		} else if k == "public" {
			st["proxy"] = "rotating (no key)"
		} else if e.bound {
			st["proxy"] = ""
			st["noEgress"] = true
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
// CLI 一致）。校验规则 2026-10-15 在新 inference 后端重新实测：缺工具 →
// 403；工具在但 parameters 为空 schema → 403；描述文本、tool_choice、UA
// 均不参与校验。注入的定义（描述+schema）统一取 zen_gate_tools.go 的官方
// CLI 2.1.4 原文（2026-10-26 认证抓包），本表只保留名字与精简描述作兜底/名录用途。
// v2 与 v1 的区别：bash/task/todowrite 被移除，新增 question/subagent/
// execute，顺序为 CLI 发送顺序。单一来源：改名/增删必须同时满足两个
// 端点的校验，只改这里。
var zenGateToolSpecs = []struct{ name, desc string }{
	{"edit", "Edit a file"},
	{"glob", "Find files by glob pattern"},
	{"grep", "Search file contents"},
	{"question", "Ask the user a question"},
	{"read", "Read a file"},
	{"shell", "Execute a shell command"},
	{"skill", "Load a skill"},
	{"subagent", "Run a subagent"},
	{"webfetch", "Fetch a web page"},
	{"websearch", "Search the web"},
	{"write", "Write a file"},
	{"execute", "Run JavaScript in a confined Code Mode runtime"},
}

// zenGateTools chat 端点形态：tools[].{type,function:{name,description,parameters}}。
// 返回 []any 与客户端解码出来的 tools 同型（下游要拼接两者）。
// parameters 必须是官方真实 schema（空 schema 会被新后端 403，见
// zen_gate_tools.go）；嵌入定义缺失时才退回精简描述+空 schema 兜底。
func zenGateTools() []any {
	defs := zenGateToolDefs()
	out := make([]any, 0, len(zenGateToolSpecs))
	if len(defs) > 0 {
		for _, d := range defs {
			out = append(out, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        d.Name,
					"description": d.Description,
					"parameters":  d.ParametersChat,
					// CLI 2.1.4 抓包：12 个 gate 工具的 function 里都带
					// "strict": false。responses 路径一直有该字段
					// （zenMergeResponsesTools），chat 路径漏了——同一份工具集
					// 在两条路径上形态不一致，补齐。语义上 strict 缺省即 false，
					// 这里是形态对齐而非行为改变。
					"strict": false,
				},
			})
		}
		return out
	}
	for _, s := range zenGateToolSpecs {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        s.name,
				"description": s.desc,
				"parameters":  zenEmptySchema(),
				"strict":      false,
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
		// CLI 2.1.4 在 chat 路径恒发 {"include_usage":true}（2026-10-30 抓包）。
		// 上游只有收到该字段才会在 SSE 末尾补一帧纯 usage 块——不发的话网关的
		// 流式 token 统计恒为 0（onUsage 从来收不到数据）。这一帧不带 choices，
		// 客户端的 OpenAI 解析器会忽略它，形态安全。
		//
		// 在客户端给的对象上合并而不是整体替换：客户端若传了别的
		// stream_options 子键，不该因为网关要统计就丢掉。客户端显式写
		// include_usage:false 时仍改为 true —— 网关需要该帧做统计，而下发时
		// 会按客户端的原始意愿吞掉它（见 clientWantsStreamUsage）。
		so := map[string]any{"include_usage": true}
		if cs, ok := params["stream_options"].(map[string]any); ok {
			for k, v := range cs {
				if k != "include_usage" {
					so[k] = v
				}
			}
		}
		body["stream_options"] = so
	} else {
		// 非流式路径不该带上流式专有字段（passThroughKeys 会原样搬过来）。
		delete(body, "stream_options")
	}
	if model, ok := params["model"].(string); ok {
		if m, ok := resolveZenModel(model); ok {
			body["model"] = m.ID
			// 输出上限默认值：CLI 2.1.4 对 big-pickle 发 max_completion_tokens
			// =32000，正是目录里该模型的 limit.output（2026-10-30 抓包 + 目录
			// 核对一致）。网关此前完全不发该字段——上游用自己更大的缺省值，
			// 与真实客户端形态不同。客户端显式给了就尊重客户端的值。
			if _, has := body["max_completion_tokens"]; !has {
				if _, hasTok := body["max_tokens"]; !hasTok && m.Output > 0 {
					body["max_completion_tokens"] = m.Output
				}
			}
		}
	}
	delete(body, "reasoning_effort")
	delete(body, "reasoningEffort")
	// FreeTier gate（2026-09-18 实测解码）：zen 免费层 chat 端点校验请求体
	// 是否携带 opencode 工具集，缺失则 403（"can only be used from within
	// OpenCode"）。恒注入 12 个 v2 规范工具名保证过闸。
	// tool_choice：CLI v2 在 chat 路径完全不发该字段（2026-10-26 认证
	// 抓包，body keys 无 tool_choice），上游缺省即 auto，简单问答仍
	// 回文本（CLI hi 实测）。旧网关的 none/auto 强制值是 v1 遗留指纹，
	// 2026-10-26 实验：TLS/UA/头/工具全部对齐后仍 429，tool_choice 是
	// 剩余差异之一，故改为：客户端显式指定时原样透传，否则省略。
	delete(body, "functions")
	delete(body, "function_call")
	clientTools := zenClientTools(params)
	if len(clientTools) == 0 {
		body["tools"] = zenGateTools()
	} else {
		body["tools"] = zenMergeTools(clientTools, zenGateTools())
	}
	// tool_choice 分流（客户端必须拿到它声明的语义）：
	//  - 客户端无工具（纯文本问答）：恒定 none，即使客户端传了 auto/required。
	//    此时 tools[] 里只有 gate 占位 stub，客户端没有执行层；让上游按
	//    auto/required 处理会让模型返回 read/shell/execute 这类客户端从未
	//    声明的调用，IDE 解析失败（required 更会强制成一次必失败的工具调用）。
	//  - 客户端有工具但没指定：auto，让模型按需调用客户端真实工具。
	//  - 客户端显式指定且有工具：原样透传。
	if len(clientTools) == 0 {
		body["tool_choice"] = "none"
	} else if tc, ok := params["tool_choice"]; ok {
		body["tool_choice"] = tc
	} else {
		body["tool_choice"] = "auto"
	}
	ensureZenEnvMarkerChat(body)
	return body
}

// zenEnvBlock returns the CLI's <env> marker block. 2026-10-26 ablation
// (raw uTLS call, same IP/account/TLS/headers/tools, body varied one
// element at a time): removing the system prompt -> 429; truncating it
// to 8000 chars -> 429; last 1000 chars alone -> 200; env block alone
// -> 200; env block without "Working directory:" -> 429; "Working
// directory:" outside <env> -> 429; fake directory value -> 200.
// Upstream therefore checks, in a system message, for a line
// "Working directory:" inside <env>...</env> — the CLI always injects
// this block (working directory, platform, git status, date) at the
// end of its system prompt, and a generic OpenAI client does not.
// The value itself is not validated, so the gateway uses its own
// working directory (truthful for the gateway process) rather than
// fabricating a client's directory.
func zenEnvBlock() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		wd = "/tmp/opencode"
	}
	return "<env>\n  Working directory: " + wd + "\n</env>"
}

func zenTextHasEnvMarker(s string) bool {
	return strings.Contains(s, "<env>") && strings.Contains(s, "Working directory:")
}

// zenContentText 取出 message content 的纯文本，兼容三种客户端形态：
// 裸字符串、parts 数组 [{"type":"text","text":...}]（IDE 常见）、
// 单 part 对象。仅用于 marker 检测，不做内容改写。
func zenContentText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := pm["text"].(string); ok {
				b.WriteString(s)
				b.WriteString("\n")
			}
		}
		return b.String()
	case map[string]any:
		if s, ok := c["text"].(string); ok {
			return s
		}
	}
	return ""
}

// ensureZenEnvMarkerChat appends the env block to the first system
// message (or prepends a system message with just the block) when no
// system/developer message already carries the marker. It never
// removes or rewrites client content; it only appends the marker the
// free tier checks for, at the end, mirroring CLI's layout.
//
// 两点必须成立，否则会伤到客户端：
//  1. marker 检测必须看得懂 parts 数组形态。只认 string 时，已带
//     marker 的数组 system 会被判为"没有"，于是又 prepend 一个
//     system 消息，产生两条相邻 system。
//  2. 不得就地改写调用方的 messages。body["messages"] 与
//     params["messages"] 同底层切片（buildZenBody 只做浅拷贝），
//     就地改 map 会把 marker 写进客户端参数，端点自适应重试、
//     日志、token 估算随后读到的都是被污染的版本。
func ensureZenEnvMarkerChat(body map[string]any) {
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		if zenTextHasEnvMarker(zenContentText(mm["content"])) {
			return
		}
	}
	// 先复制消息 map（以及 parts 切片），再在副本上加 marker。
	out := make([]any, len(msgs))
	for i, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out[i] = m
			continue
		}
		cp := make(map[string]any, len(mm)+1)
		for k, v := range mm {
			cp[k] = v
		}
		if parts, ok := cp["content"].([]any); ok {
			nc := make([]any, len(parts), len(parts)+1)
			copy(nc, parts)
			cp["content"] = nc
		}
		out[i] = cp
	}
	for _, m := range out {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			mm["content"] = c + "\n\n" + zenEnvBlock()
		case []any:
			// 数组形态：追加一个 text part，保持客户端原有结构。
			mm["content"] = append(c, map[string]any{"type": "text", "text": zenEnvBlock()})
		default:
			mm["content"] = zenEnvBlock()
		}
		body["messages"] = out
		return
	}
	body["messages"] = append([]any{map[string]any{"role": "system", "content": zenEnvBlock()}}, out...)
}

func ensureZenEnvMarkerResponses(body map[string]any) {
	if s, ok := body["instructions"].(string); ok && zenTextHasEnvMarker(s) {
		return
	}
	if s, ok := body["instructions"].(string); ok && s != "" {
		body["instructions"] = s + "\n\n" + zenEnvBlock()
		return
	}
	body["instructions"] = zenEnvBlock()
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
				// 用 zenContentText 取文本：IDE 常发 parts 数组形态，只认
				// string 会把客户端的系统提示词整段静默丢掉。
				if s := zenContentText(content); s != "" {
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
	ensureZenEnvMarkerResponses(body)
	return body
}

// setZenGateHeaders 写入 zen 免费门要求的客户端身份头，两条上游路径（chat
// /responses）共用——此前两处逐行重复，改一处漏一处就会让某条路径掉头。
// 直接写 map（而非 Header.Set）以保留小写头名：官方 CLI 发的就是小写，
// Set 会规范化为 X-Opencode-...；HTTP 头大小写不敏感，但字节级对齐更安全。
// Authorization/Content-Type/User-Agent 因需承载变量，由调用方先行设置。
// 2026-10-26 实测（CLI v2.1.4 认证抓包）：
//   - CLI v2 在两条路径都发 x-opencode-session-id, x-opencode-session,
//     x-session-affinity, x-session-id，四个头同值（新鲜会话时四者
//     相等；父会话/分支会话时 q 规则见静态 RE 报告，网关粘性会话
//     属于新鲜会话情形，故四者同值正确）。
//   - CLI v2 完全不发 x-opencode-request（静态计数 0，抓包亦无）。
//     旧网关仍发该头是 v1 遗留指纹，必须去掉；requestID 参数保留
//     仅为调用点兼容，不再写入任何头。
//   - x-opencode-org-id 只在 OAuth 账户元数据有 orgID 时出现，网关
//     的原始 key 字符串中没有该信息，绝不编造，缺省即正确。
//   - x-opencode-project：网关没有客户端项目目录，global 是唯一诚实值。
func setZenGateHeaders(req *http.Request, sess, requestID string) {
	_ = requestID // v2 CLI does not send x-opencode-request; do not emit it
	req.Header["x-opencode-session-id"] = []string{sess}
	req.Header["x-opencode-session"] = []string{sess}
	req.Header["x-session-affinity"] = []string{sess}
	req.Header["x-session-id"] = []string{sess}
	req.Header["x-opencode-client"] = []string{"cli"}
	req.Header["x-opencode-project"] = []string{"global"}
	if org := getZenConfig().OrgID; org != "" {
		req.Header["x-opencode-org-id"] = []string{org}
	}
	// CLI v2 wire headers (2026-10-26 capture): Accept */*, and the
	// full Accept-Encoding list. Go's default is only "gzip" and no
	// Accept header, which is another non-CLI fingerprint marker.
	// Setting Accept-Encoding explicitly disables Go's automatic gzip
	// decompression; upstream SSE for stream=true is uncompressed in
	// practice, and the gateway parses SSE as raw bytes, so this is safe.
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	// CLI v2 tracing headers, same trace/span across b3 and traceparent.
	traceID := kit.RandHex(16)
	spanID := kit.RandHex(8)
	req.Header.Set("b3", traceID+"-"+spanID+"-1")
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
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
		// parameters 必须用 responses 形态的官方真实 schema：空 schema
		// 会被新后端 403（2026-10-15 实测，见 zen_gate_tools.go）。
		desc, params := s.desc, zenEmptySchema()
		if d := lookupZenGateToolDef(s.name); d != nil {
			desc, params = d.Description, d.ParametersResponses
		}
		merged = append(merged, map[string]any{
			"type":        "function",
			"name":        s.name,
			"description": desc,
			"parameters":  params,
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
//
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
//
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
	retryKey := ""          // 非空时重试沿用该 key（保持 key sess_ 一致）
	remapped := false       // 410 弃用迁移至多一次，防别名链循环
	sessionRetried := false // FreeTier 403 换新会话后同 key 重试至多一次
	// keySwitches 限制"一次客户端请求最多冷却几个 key"（同 chat 路径，
	// 见 callZenAPI 里的说明：换 key 会不可逆地冷却 key，必须单独设上限）。
	keySwitches := 0
	maxKeySwitches := retries
	if maxKeySwitches > max429KeySwitches {
		maxKeySwitches = max429KeySwitches
	}
	// 2026-10-27: fresh session per API call (zenCallIdentity). Same
	// session across retries of this call, fresh if the key switches
	// or a 403 forces a refresh. See zen_session.go.
	callIdent := newZenCallIdentity()

	for attempt := 0; ; attempt++ {
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
		// 绑定策略下"没有可用 key"只可能是一个原因：代理不够。不能退回
		// 匿名/直连——那会让上游继续看到无主流量，问题更难查。
		if key == "" && len(cfg.Keys) > 0 && egressBindingActive() {
			return nil, rateLimited, egressShortfallError("zen", egressNSZen, cfg.Keys)
		}
		// 出口必须在 key 之后选：绑定策略下一个 key 固定一个代理，
		// 按请求轮转会让同一个 key 从多个 IP 出去（上游据此判可疑）。
		// pinKey/ZEN_PIN_KEY 绕过轮转，这条同样挡住它们——否则面板 Test
		// 会以直连发出，给该 key 记下一个额外的 IP。
		proxyURL, pidx, perr := zenEgress(key)
		if perr != nil {
			return nil, rateLimited, perr
		}
		viaProxy := "direct"
		if proxyURL != "" {
			viaProxy = maskProxyURL(proxyURL)
		}
		// Fresh session per API call (2026-10-27): same session for
		// retries of this call, fresh for a new call / new key.
		sess, user, ua := callIdent.forKey(key)
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
		// 显式 Accept-Encoding 关闭了 net/http 的自动解压，必须自己解。
		if derr := kit.DecodeResponse(resp); derr != nil {
			resp.Body.Close()
			return nil, rateLimited, fmt.Errorf("zen responses decode: %w", derr)
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
			if keySwitches < maxKeySwitches {
				if next := pickZenKey(); next != "" && next != key && !zenKeyCooling(next) {
					// 必须同步改写 retryKey：循环头 key=retryKey，只改 key 不改
					// retryKey 会让下一次迭代继续用刚冷却的旧 key，无限 429 空转。
					log.Printf("  zen responses rate limited (%d), switching to next zen key (%d configured, switch %d/%d)",
						resp.StatusCode, len(cfg.Keys), keySwitches+1, maxKeySwitches)
					key = next
					retryKey = next
					keySwitches++
					continue
				}
			} else if len(cfg.Keys) > keySwitches+1 {
				log.Printf("  zen responses rate limited (%d): key-switch budget exhausted (%d/%d) with %d key(s) configured, not cooling the rest",
					resp.StatusCode, keySwitches, maxKeySwitches, len(cfg.Keys))
			}
			// 无 key 可换时快速失败，语义与 chat 路径一致（见那里的长注释）：
			// 睡 30s 后必然重撞同一个已冷却的 key，只是把客户端连接挂住。
			if attempt < retries && zenAnyKeyRotatable(key) {
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
			if attempt < retries {
				log.Printf("  zen responses rate limited (%d): no rotatable key, failing fast instead of sleeping %d more time(s)",
					resp.StatusCode, retries-attempt)
			}
			// 限流不是"zen 挂了"：上游明确回了 HTTP 429，服务正常，只是这个
			// key/身份当前超了窗口。把它计入故障转移会让一次瞬时节流把整个 zen
			// 路由切走（cline 池为空时客户端直接吃 401），而限流本来已由 key
			// 冷却 + 换 key + 重试处理。故此处不推进 failover。
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
				log.Printf("  zen responses session rejected (403) [%s] key#%d: minting fresh session for this call, retry same key",
					kit.Truncate(sess, 24), keyIndex(key))
				callIdent.refresh(key)
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
	// keySwitches 限制"一次客户端请求最多冷却几个 key"。
	//
	// 2026-10-30 实测事故：一个 291 条消息的大请求 429 后，下面的换 key 分支
	// 用 `continue` 绕过了 `attempt < retries` 判断，而循环头是 `for attempt := 0; ;`
	// 无上界——结果 4 秒内把池里 5 个 key 全部冷却到 24h（maxCooldown）。
	// 一次请求的规模问题因此被放大成"整池被封"的假象。
	//
	// 语义区分：睡眠重试（attempt < retries）是"等一等再用同一个 key"，成本低；
	// 换 key 会永久冷却一个 key，成本高且不可逆。故单独计数、单独设上限，
	// 不占用重试预算：keySwitches 上限 = retries（默认 3），即一次请求最多
	// 触碰 1 + 3 = 4 个 key，最多冷却 4 个。这与"重试次数"是两件事，
	// 不能共用一个计数器。
	keySwitches := 0
	maxKeySwitches := retries
	if maxKeySwitches > max429KeySwitches {
		maxKeySwitches = max429KeySwitches
	}
	// 2026-10-27: fresh session per API call (zenCallIdentity). Same
	// session across retries of this call, fresh if the key switches
	// or a 403 forces a refresh. See zen_session.go.
	callIdent := newZenCallIdentity()
	// retryKey pins the key for the duration of this call. Previously
	// this was an emergent property of the sticky session: pickZenKey
	// prefers keys with a live session, so a 403 retry naturally stayed
	// on the same key. With fresh sessions there is no live session to
	// prefer, so without this pin a 403 retry would round-robin to the
	// next key and fan a single session failure out across the pool.
	// Rate-limit switching (below) is the only branch that rewrites it,
	// mirroring callZenResponsesAPI.
	retryKey := ""

	for attempt := 0; ; attempt++ {
		// 客户端身份：每次 API 调用全新会话（2026-10-27，见 zen_session.go），
		// 同一调用的重试沿用同一会话，msg_ 请求 ID 每次随机。
		// ZEN_PIN_KEY=n 时固定用第 n 个 key（单 key 直测/排障），默认轮转。
		// pinKey（面板 Test）优先且不碰轮转指针：一次探测不该挪动 round-robin
		// 的游标，否则每点一次 Test 都会让下一次正常请求换 key。
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
		// 出口在 key 之后选：绑定策略下一个 key 固定一个代理，按请求轮转
		// 会让同一个 key 从多个 IP 出去——2026-10-21 起 zen 按 key 记录出口，
		// 这正是它判定"整池 key 可疑"的指纹。
		if key == "" && len(cfg.Keys) > 0 && egressBindingActive() {
			return nil, rateLimited, egressShortfallError("zen", egressNSZen, cfg.Keys)
		}
		proxyURL, pidx, perr := zenEgress(key)
		if perr != nil {
			return nil, rateLimited, perr
		}
		viaProxy := "direct"
		if proxyURL != "" {
			viaProxy = maskProxyURL(proxyURL)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyJSON))
		if err != nil {
			return nil, rateLimited, fmt.Errorf("create zen request: %w", err)
		}
		// Fresh session per API call (2026-10-27, see zenCallIdentity).
		sess, user, ua := callIdent.forKey(key)
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
		// 显式 Accept-Encoding 关闭了 net/http 的自动解压，必须自己解。
		if derr := kit.DecodeResponse(resp); derr != nil {
			resp.Body.Close()
			return nil, rateLimited, fmt.Errorf("zen decode: %w", derr)
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
			if keySwitches < maxKeySwitches {
				if next := pickZenKey(); next != "" && next != key && !zenKeyCooling(next) {
					log.Printf("  zen rate limited (%d), switching to next zen key (%d configured, switch %d/%d)",
						resp.StatusCode, len(cfg.Keys), keySwitches+1, maxKeySwitches)
					key = next
					retryKey = next
					keySwitches++
					continue
				}
			} else if len(cfg.Keys) > keySwitches+1 {
				// 还有没用过的 key，但本次请求的换 key 预算已用完。必须明确
				// 记一行：否则表现为"明明还有 key 却直接 429 返回"，排查时
				// 会误以为是 key 池配置问题。
				log.Printf("  zen rate limited (%d): key-switch budget exhausted (%d/%d) with %d key(s) configured, not cooling the rest",
					resp.StatusCode, keySwitches, maxKeySwitches, len(cfg.Keys))
			}
			// 睡眠重试只在"等一等就可能变好"时有价值。这个 key 刚被冷却
			// zenRateLimitCooldown（20 分钟），而这里的等待上限只有 30s ——
			// 睡醒后重试必然还是撞同一个已冷却的 key。实测单 key 池下，
			// 一次请求要挂 101 秒（4 次 upstream × 30s 睡眠）才把 429 交还，
			// IDE 侧看起来像"网关卡死"，而真实结论在第一秒就已确定。
			// 故：还有别的 key 可轮转 → 上面的换 key 分支已 continue；
			// 没有 → 立即返回 429，把退避权交还给客户端。
			if attempt < retries && zenAnyKeyRotatable(key) {
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
			if attempt < retries {
				log.Printf("  zen rate limited (%d): no rotatable key, failing fast instead of sleeping %d more time(s)",
					resp.StatusCode, retries-attempt)
			}
			// 限流不是"zen 挂了"：上游明确回了 HTTP 429，服务正常，只是这个
			// key/身份当前超了窗口。把它计入故障转移会让一次瞬时节流把整个 zen
			// 路由切走（cline 池为空时客户端直接吃 401），而限流本来已由 key
			// 冷却 + 换 key + 重试处理。故此处不推进 failover。
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
				log.Printf("  zen chat session rejected (403) [%s], key#%d (probe: no state change)", kit.Truncate(sess, 24), keyIndex(key))
				return nil, rateLimited, apiErr
			}
			if !sessionRetried {
				sessionRetried = true
				log.Printf("  zen chat session rejected (403) [%s] key#%d: minting fresh session for this call, retry same key",
					kit.Truncate(sess, 24), keyIndex(key))
				callIdent.refresh(key)
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
// provider 级 api 基地址、是否可达）。价格门 = cost.input==0 && cost.output==0
// 且 status 非 deprecated。模型 ID 以条目内 id 优先、map key 兜底。目录不可达
// 返回 (空, 空, "", false)，调用方按 fail-open 处理。
// providerAPI 是 opencode provider 的 api 字段——免费层真正的上游基地址。
// 它此前只存在于一次人工观测里（见 zenAPIBase 的注释），现在每次目录同步都
// 重新取一遍，供 applyZenRegistryBase 校正默认值。
// 唯一实现：syncZenModels 使用（会话铸造改本地后不再有第二消费者），
// 避免两处价格门逻辑漂移。
func fetchZenRegistry() (map[string]zenModelOverlay, map[string]bool, string, bool) {
	overlay := map[string]zenModelOverlay{}
	freeGate := map[string]bool{}
	oreq, err := http.NewRequest("GET", zenRegistryURL, nil)
	if err != nil {
		return overlay, freeGate, "", false
	}
	oreq.Header.Set("User-Agent", "opencode/latest/cli")
	client := &http.Client{Timeout: 25 * time.Second}
	oresp, err := client.Do(oreq)
	if err != nil {
		return overlay, freeGate, "", false
	}
	defer oresp.Body.Close()
	if oresp.StatusCode != 200 {
		return overlay, freeGate, "", false
	}
	var payload map[string]struct {
		NPM    string `json:"npm"`
		API    string `json:"api"`
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
		return overlay, freeGate, "", false
	}
	prov, ok := payload["opencode"]
	if !ok {
		return overlay, freeGate, "", false
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
	return overlay, freeGate, strings.TrimRight(strings.TrimSpace(prov.API), "/"), true
}

// applyZenRegistryBase 用目录里的 provider.api 校正 zen 上游基地址。
//
// 背景：zenAPIBase 是编译期常量，而免费层的路径已经改过一次（/zen/v1 →
// /inference/v1 → /inference/openai/v1），每次都靠人工观测再改代码，期间网关
// 一直打在已经不存在的旧路径上。目录本身就是这个地址的唯一权威（真实 CLI
// 也读同一份数据），所以每次目录同步都拿它校正一次。
//
// 三条纪律：
//   - 只在基地址仍等于"本项目的默认值"时校正。用户在面板里显式填过别的
//     地址（自建中转、抓包代理）就绝不动它——那是明确的意图，不是过时的
//     观测。
//   - 只接受 https://opencode.ai/inference/... 的值。目录是远端数据，不能让
//     一个被污染或中间人改写的响应把带 Authorization 的流量导去任意主机，也
//     不能把它指回已失效的 /zen/v1（匿名流量在那里恒 403）。
//   - 写盘时走 setZenConfig 的既有路径（含 saveZenConfig 的原子写），并留日志。
//
// 返回是否发生校正。
func applyZenRegistryBase(registryAPI string, current string) bool {
	want := strings.TrimRight(strings.TrimSpace(registryAPI), "/")
	if want == "" {
		return false
	}
	// 目录给的是顶级 provider api；免费模型都在 /inference/openai/v1 下。
	// 只接受 https://opencode.ai/inference/...：既避免把凭据送去别处，也避免
	// 被篡改的目录把基地址指回已失效的 /zen/v1（匿名流量在那里恒 403）。
	u, err := url.Parse(want)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "opencode.ai") ||
		!strings.HasPrefix(u.Path, "/inference/") {
		log.Printf("zen registry api ignored (not https://opencode.ai/inference/...): %q", kit.Truncate(want, 120))
		return false
	}
	cur := strings.TrimRight(strings.TrimSpace(current), "/")
	// 只覆盖"仍是默认值"的情形：zenAPIBase（当前默认）与 zenStaleBase
	// （上一版默认，已由 loadZenConfig 迁移，这里兜住运行期改目录的情况）。
	if cur != zenAPIBase && cur != zenStaleBase {
		return false
	}
	if cur == want {
		return false
	}
	cfg := *getZenConfig()
	cfg.BaseURL = want
	setZenConfig(&cfg)
	log.Printf("zen baseURL corrected from registry: %s -> %s", cur, want)
	return true
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
//  4. 上游基地址：目录 provider.api 与"仍是默认值"的配置不符时自动校正
//     （见 applyZenRegistryBase）——免费层路径改过一次，不该再靠人工观测。
//
// 增删一律以本次 live 结果为准：表中的种子条目若不再通过价格门会被移除
// （种子只在首次成功同步之前的冷启动/离线状态提供兜底，不参与日常增删）。
// 返回新增模型数。
func syncZenModels() (int, error) {
	initZenModels()
	overlay, freeGate, registryAPI, registryOK := fetchZenRegistry()
	desired, derr := zenDesiredFromLive(registryOK, freeGate)
	if derr != nil {
		return 0, derr
	}
	// 基地址先校正：后面 applyZenCatalog 之后马上可能有请求进来。
	applyZenRegistryBase(registryAPI, getZenConfig().BaseURL)
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
