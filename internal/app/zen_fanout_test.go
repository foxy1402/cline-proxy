package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 2026-10-30 实测事故回归：一个 291 条消息的大请求在 4 秒内把池里 5 个 key
// 全部冷却到 24h。根因是 429 分支的换 key 用 `continue` 绕过了 `attempt < retries`
// 判断，而循环头无上界。现在换 key 有独立预算 max429KeySwitches。
//
// 断言：一次请求最多冷却 1 + max429KeySwitches 个 key，即使池里远不止这么多。
func TestZen429KeySwitchIsBounded(t *testing.T) {
	// 12 个 key：远超预算，用来证明"不是被池大小限制"而是被预算限制。
	keys := make([]string, 12)
	for i := range keys {
		keys[i] = "sk-" + strings.Repeat("k", 2) + string(rune('a'+i))
	}

	var hits int32
	seen := map[string]int{}
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen[k]++
		mu.Unlock()
		// 恒定 429，Retry-After 给最大值（模拟 FreeUsageLimitError 打到日窗口边界）
		w.Header().Set("Retry-After", "78410")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached."}}`))
	}))
	defer upstream.Close()

	savedCfg := getZenConfig()
	cfgCopy := *savedCfg
	cfgCopy.Enabled = true
	cfgCopy.BaseURL = upstream.URL
	cfgCopy.Keys = keys
	cfgCopy.Proxies = nil
	cfgCopy.Retries = 3
	setZenConfig(&cfgCopy)
	t.Cleanup(func() { setZenConfig(savedCfg) })
	setZenModelForTest("aaa-429-model", "")
	t.Cleanup(func() {
		zenKeyMu.Lock()
		zenKeyCool = map[string]time.Time{}
		zenKeyMu.Unlock()
		zenSessFailedMu.Lock()
		zenSessFailed = map[string]int{}
		zenSessFailedMu.Unlock()
	})

	params := map[string]any{
		"model":    "aaa-429-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	_, _, err := callZenAPI(t.Context(), params, true)
	var he *zenHTTPError
	if !errors.As(err, &he) || he.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want zenHTTPError 429", err)
	}

	mu.Lock()
	distinct := len(seen)
	mu.Unlock()

	// 预算 = 1 个初始 key + max429KeySwitches 次切换。
	wantMax := 1 + max429KeySwitches
	if distinct > wantMax {
		t.Fatalf("one request cooled %d keys (%v); budget allows at most %d — this is the pool-wipe regression",
			distinct, seen, wantMax)
	}
	// 必须真的用上预算（不是退化成只打 1 个 key 就放弃）：证明换 key 逻辑仍在工作。
	if distinct < 2 {
		t.Fatalf("only %d key(s) tried; key switching should still happen within budget", distinct)
	}
	// 冷却的 key 数量必须与尝试过的 key 数量一致（每个 429 都冷却自己的 key）。
	cooled := 0
	zenKeyMu.Lock()
	for _, k := range keys {
		if until, ok := zenKeyCool[k]; ok && time.Now().Before(until) {
			cooled++
		}
	}
	zenKeyMu.Unlock()
	if cooled > wantMax {
		t.Fatalf("%d keys cooled, budget allows at most %d", cooled, wantMax)
	}
	if cooled < distinct {
		t.Fatalf("tried %d keys but only %d cooled; every 429'd key must be cooled", distinct, cooled)
	}
}

// responses 路径同一事故面：换 key 也必须受同一个预算约束。
func TestZenResponses429KeySwitchIsBounded(t *testing.T) {
	keys := make([]string, 12)
	for i := range keys {
		keys[i] = "sk-" + strings.Repeat("r", 2) + string(rune('a'+i))
	}

	seen := map[string]int{}
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen[k]++
		mu.Unlock()
		w.Header().Set("Retry-After", "78410")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"limit"}}`))
	}))
	defer upstream.Close()

	savedCfg := getZenConfig()
	cfgCopy := *savedCfg
	cfgCopy.Enabled = true
	cfgCopy.BaseURL = upstream.URL
	cfgCopy.Keys = keys
	cfgCopy.Proxies = nil
	cfgCopy.Retries = 3
	setZenConfig(&cfgCopy)
	t.Cleanup(func() { setZenConfig(savedCfg) })
	setZenModelForTest("resp-429-model", "responses")
	t.Cleanup(func() {
		zenKeyMu.Lock()
		zenKeyCool = map[string]time.Time{}
		zenKeyMu.Unlock()
		zenSessFailedMu.Lock()
		zenSessFailed = map[string]int{}
		zenSessFailedMu.Unlock()
	})

	params := map[string]any{
		"model":    "resp-429-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	_, _, err := callZenResponsesAPI(t.Context(), params, true)
	var he *zenHTTPError
	if !errors.As(err, &he) || he.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want zenHTTPError 429", err)
	}

	mu.Lock()
	distinct := len(seen)
	mu.Unlock()
	if distinct > 1+max429KeySwitches {
		t.Fatalf("responses path cooled %d keys (%v), budget allows %d", distinct, seen, 1+max429KeySwitches)
	}
	if distinct < 2 {
		t.Fatalf("only %d key(s) tried; switching should still work within budget", distinct)
	}
}

// 压缩必须在客户端上限之内触发。OpenClaw 等把输入封顶在 128k，而旧默认
// buffer=20000 对 200k/32k 模型给出 168k 阈值 —— 压缩永不触发，满载请求原样
// 送上游（2026-10-30 那次 429 的直接成因）。
func TestCompactionFiresBelowTypicalClientCap(t *testing.T) {
	// 200k 窗口 / 32k 输出的典型免费模型
	got := compactThreshold(200000, 32000, defaultCompactBuffer)
	if got >= 128000 {
		t.Fatalf("threshold = %d; compaction would not fire below a 128k client cap", got)
	}
	// 摘要输出预算仍要留得下：阈值不能挤到 keepTokens 之下
	if got < 8000 {
		t.Fatalf("threshold = %d is too low to leave a usable tail budget", got)
	}
	// 大窗口模型不应被新默认值拖动（max(output,buffer) 由 output 主导）
	if got := compactThreshold(262144, 262144, defaultCompactBuffer); got != 131072 {
		t.Fatalf("large-output model threshold = %d, want unchanged 131072", got)
	}
	// 下限保护仍然生效
	if got := compactThreshold(2000, 32768, defaultCompactBuffer); got < 1000 {
		t.Fatalf("small-window floor broken: %d", got)
	}
}

// Retry-After 是"距下一个 00:00 UTC"的倒计时，不是真实恢复时间。
// 2026-10-30 实测：429 报 RA=20h46m，而同一 key 在 18 分钟后即恢复 200。
// 按 RA 冷藏多 key 池会让 key 白停一整天，故 zen key 冷却必须封顶。
func TestZenCooldownIsCappedDespiteHugeRetryAfter(t *testing.T) {
	zenKeyMu.Lock()
	zenKeyCool = map[string]time.Time{}
	zenKeyMu.Unlock()
	t.Cleanup(func() {
		zenKeyMu.Lock()
		zenKeyCool = map[string]time.Time{}
		zenKeyMu.Unlock()
	})

	const key = "sk-cap-test"
	// 上游实测值：75020s ≈ 20h50m
	applied := cooldownZenKey(key, 75020*time.Second)

	if applied > zenRateLimitCooldown {
		t.Fatalf("applied cooldown %v exceeds cap %v — a 20-minute throttle would park the key for a day",
			applied, zenRateLimitCooldown)
	}
	if applied != zenRateLimitCooldown {
		t.Fatalf("applied = %v, want the cap %v for an oversized Retry-After", applied, zenRateLimitCooldown)
	}
	// 必须真的生效（不能退化成不冷却）
	if !zenKeyCooling(key) {
		t.Fatal("key must still be cooled after a 429")
	}
	// 短冷却不受影响：15s 的 Retry-After 应原样生效
	zenKeyMu.Lock()
	zenKeyCool = map[string]time.Time{}
	zenKeyMu.Unlock()
	if got := cooldownZenKey("sk-short", 15*time.Second); got != 15*time.Second {
		t.Fatalf("short cooldown = %v, want 15s unchanged", got)
	}
	// 缺头（0）仍回落到 1 分钟
	if got := cooldownZenKey("sk-none", 0); got != time.Minute {
		t.Fatalf("missing Retry-After = %v, want 1m fallback", got)
	}
}

// 封顶后整池冷却不应导致"无 key 可用"：pickZenKey 仍须轮转返回一个 key，
// 否则一次瞬时节流就会让网关完全停止发请求，只能等冷却过期。
func TestPickZenKeyStillReturnsWhenAllCooled(t *testing.T) {
	zenKeyMu.Lock()
	zenKeyCool = map[string]time.Time{}
	zenKeyMu.Unlock()

	savedCfg := getZenConfig()
	cp := *savedCfg
	cp.Keys = []string{"sk-c1", "sk-c2"}
	cp.Proxies = nil
	setZenConfig(&cp)
	t.Cleanup(func() { setZenConfig(savedCfg) })
	t.Cleanup(func() {
		zenKeyMu.Lock()
		zenKeyCool = map[string]time.Time{}
		zenKeyMu.Unlock()
	})

	cooldownZenKey("sk-c1", zenRateLimitCooldown)
	cooldownZenKey("sk-c2", zenRateLimitCooldown)

	if got := pickZenKey(); got == "" {
		t.Fatal("pickZenKey returned empty while every key was cooling; requests would stop entirely")
	}
}
