package app

import (
	"cline-go-proxy/internal/kit"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// ============================================================================
// 请求级统计与日志入库
// 落盘: zen-stats.jsonl 追加式(每请求一行)
// 内存: 今日/累计聚合,管理后台展示
// ============================================================================

type zenStatsRecord struct {
	TS               int64  `json:"ts"`
	Upstream         string `json:"upstream"` // zen / cline
	Model            string `json:"model"`    // 上游实际模型 ID
	Stream           bool   `json:"stream"`
	Compacted        bool   `json:"compacted"`
	OK               bool   `json:"ok"`
	Status           int    `json:"status"`
	LatencyMs        int64  `json:"latencyMs"`
	PromptTokens     int    `json:"promptTokens"`     // 入站估算
	// UpstreamPromptTokens 上游 usage 回报的**真实**计费输入 token 数。
	// PromptTokens 只是入站请求体的字节估算，不含网关注入的 gate 工具
	// schema，因此与真实扣额度差一个量级（实测一个 64 工具的 IDE 请求约
	// 12.8k，而估算值只有几千）。排查"为什么一次请求就打爆日额度"必须
	// 看这个字段，否则会误判成内容被扫描。
	UpstreamPromptTokens int `json:"upstreamPromptTokens,omitempty"`
	// CachedTokens 其中命中前缀缓存的部分（上游 prompt_tokens_details）。
	CachedTokens     int    `json:"cachedTokens,omitempty"`
	CompletionTokens int    `json:"completionTokens"` // 上游 usage 或估算
	CompactionTokens int    `json:"compactionTokens"` // 摘要生成消耗
	RateLimited      int    `json:"rateLimited"`      // 本次请求触发限流的次数
}

type zenStatsAgg struct {
	Date        string                    `json:"date"`
	Requests    int64                     `json:"requests"`
	PromptTok   int64                     `json:"promptTokens"`
	CompleteTok int64                     `json:"completionTokens"`
	Compaction  int64                     `json:"compactionTokens"`
	RateLimited int64                     `json:"rateLimited"` // 限流命中次数
	ByModel     map[string]*zenStatsModel `json:"byModel"`
}

type zenStatsModel struct {
	Requests    int64 `json:"requests"`
	PromptTok   int64 `json:"promptTokens"`
	CompleteTok int64 `json:"completionTokens"`
}

var (
	statsFile     *os.File
	statsFileMu   sync.Mutex
	statsToday    *zenStatsAgg
	statsTotal    *zenStatsAgg
	statsAggMu    sync.Mutex
	statsFileInit sync.Once
)

type zenStatsTracker struct {
	rec      zenStatsRecord
	started  time.Time
	finished bool
}

func newZenStatsTracker(rec zenStatsRecord) *zenStatsTracker {
	return &zenStatsTracker{rec: rec, started: time.Now()}
}

// zenUsageFn 统计回调：从上游 usage 取 completion_tokens 记进本次记录。
// 供没有 accountUsageFn（那是 cline 账号记账）的 zen 聚合路径使用 ——
// 这类路径此前传 nil，token 用量从不入统计。
func zenUsageFn(t *zenStatsTracker) func(map[string]any) {
	if t == nil {
		return nil
	}
	return func(u map[string]any) { applyZenUsage(&t.rec, u) }
}

// applyZenUsage 把上游 usage 里的真实计费数字记进统计。
//
// 这是唯一能看到"一次请求实际花了多少额度"的地方：入站估算（PromptTokens）
// 只算客户端请求体，完全不含网关注入的 gate 工具 schema（12 个工具约
// 13.5KB ≈ 3.8k token），所以估算与真实计费差一个量级。日额度是按真实
// 用量扣的，因此排查额度问题时必须看 UpstreamPromptTokens。
func applyZenUsage(rec *zenStatsRecord, u map[string]any) {
	if v, ok := u["prompt_tokens"].(float64); ok {
		rec.UpstreamPromptTokens = int(v)
	}
	if v, ok := u["completion_tokens"].(float64); ok {
		rec.CompletionTokens = int(v)
	}
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := d["cached_tokens"].(float64); ok {
			rec.CachedTokens = int(v)
		}
	}
}

func (t *zenStatsTracker) finish(ok bool, status int) {
	if t.finished {
		return
	}
	t.finished = true
	t.rec.OK = ok
	t.rec.Status = status
	t.rec.LatencyMs = time.Since(t.started).Milliseconds()
	// 把真实计费打进日志：日额度是按 token 扣的，没有这一行，"为什么发一句话
	// 就把额度用完了"只能靠猜（客户端系统提示词？内容被扫描？）——实际上是
	// 工具 schema 的体积。每次请求一行，代价可忽略。
	if t.rec.UpstreamPromptTokens > 0 {
		log.Printf("  zen usage: prompt=%d cached=%d new=%d completion=%d status=%d",
			t.rec.UpstreamPromptTokens, t.rec.CachedTokens,
			t.rec.UpstreamPromptTokens-t.rec.CachedTokens,
			t.rec.CompletionTokens, status)
	}
	recordZenStats(t.rec)
}

func initStats() {
	statsFileInit.Do(func() {
		f, err := os.OpenFile(kit.ResolveDataPath("zen-stats.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			log.Printf("zen stats file open failed: %v", err)
			return
		}
		statsFile = f
		statsToday = newZenStatsAgg()
		statsTotal = newZenStatsAgg()
		agg := loadStatsFromFile()
		if agg != nil {
			statsTotal = agg
		}
		go rollStatsDate()
	})
}

func newZenStatsAgg() *zenStatsAgg {
	return &zenStatsAgg{
		Date:    time.Now().Format("2006-01-02"),
		ByModel: map[string]*zenStatsModel{},
	}
}

// loadStatsFromFile 从 JSONL 重建累计统计(仅今日的计入今日)
func loadStatsFromFile() *zenStatsAgg {
	agg := newZenStatsAgg()
	data, err := os.ReadFile(kit.ResolveDataPath("zen-stats.jsonl"))
	if err != nil {
		return nil
	}
	today := time.Now().Format("2006-01-02")
	lines := splitLines(string(data))
	for _, line := range lines {
		var rec zenStatsRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		aggregateRecord(agg, &rec)
		if time.UnixMilli(rec.TS).Format("2006-01-02") == today {
			aggregateRecord(statsToday, &rec)
		}
	}
	return agg
}

// billedPromptTokens 取"该记多少输入 token"：优先用上游回报的真实计费值，
// 只有拿不到 usage 时才退回入站估算。
//
// 为什么必须优先真实值：日免费额度是按上游计费的输入 token 扣的，而入站
// 估算只算客户端请求体、不含工具 schema 的体积，两者差两个数量级（实测
// 一次 ZCode 形态请求真实 10887，估算只有几千）。按估算显示会让面板
// 严重低估消耗——正是"为什么发一句话就没额度了"的排查陷阱。
func billedPromptTokens(rec *zenStatsRecord) int {
	if rec.UpstreamPromptTokens > 0 {
		return rec.UpstreamPromptTokens
	}
	return rec.PromptTokens
}

func aggregateRecord(agg *zenStatsAgg, rec *zenStatsRecord) {
	if agg == nil || rec == nil {
		return
	}
	billed := billedPromptTokens(rec)
	agg.Requests++
	agg.PromptTok += int64(billed)
	agg.CompleteTok += int64(rec.CompletionTokens)
	agg.Compaction += int64(rec.CompactionTokens)
	agg.RateLimited += int64(rec.RateLimited)
	m := agg.ByModel[rec.Model]
	if m == nil {
		m = &zenStatsModel{}
		agg.ByModel[rec.Model] = m
	}
	m.Requests++
	m.PromptTok += int64(billed)
	m.CompleteTok += int64(rec.CompletionTokens)
}

func splitLines(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func recordZenStats(rec zenStatsRecord) {
	initStats()
	statsFileMu.Lock()
	if statsFile != nil {
		b, err := json.Marshal(rec)
		if err == nil {
			statsFile.Write(append(b, '\n'))
			// 大小上限（与 requests.jsonl 同一配置）：超出清空重建。
			// 累计口径在内存聚合里，清文件只影响重启后的重建输入。
			if st, err := statsFile.Stat(); err == nil && st.Size() > LogFileMaxBytes() {
				path := statsFile.Name()
				statsFile.Close()
				if nf, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600); err == nil {
					statsFile = nf
				} else {
					log.Printf("zen stats file truncate failed: %v", err)
					statsFile = nil
				}
			}
		}
	}
	statsFileMu.Unlock()

	statsAggMu.Lock()
	aggregateRecord(statsToday, &rec)
	aggregateRecord(statsTotal, &rec)
	statsAggMu.Unlock()
}

func rollStatsDate() {
	ticker := time.NewTicker(time.Minute)
	for range ticker.C {
		today := time.Now().Format("2006-01-02")
		statsAggMu.Lock()
		if statsToday.Date != today {
			statsToday = newZenStatsAgg()
		}
		statsAggMu.Unlock()
	}
}

// cloneZenStatsAgg 深拷贝聚合（含 ByModel map），避免快照在锁外被
// json 序列化时与 recordZenStats 的并发写发生 fatal 的 map 竞态。
func cloneZenStatsAgg(src *zenStatsAgg) *zenStatsAgg {
	if src == nil {
		return nil
	}
	cp := *src
	cp.ByModel = make(map[string]*zenStatsModel, len(src.ByModel))
	for k, v := range src.ByModel {
		m := *v
		cp.ByModel[k] = &m
	}
	return &cp
}

func zenStatsSnapshot() map[string]any {
	initStats()
	statsAggMu.Lock()
	defer statsAggMu.Unlock()
	return map[string]any{
		"today": cloneZenStatsAgg(statsToday),
		"total": cloneZenStatsAgg(statsTotal),
	}
}
