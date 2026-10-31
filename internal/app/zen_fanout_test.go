package app

import (
	"context"
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
	// 每个 429 都要让该 key 退出轮转。两种退出方式都算：
	// 瞬时节流 → cooldown（20 分钟）；日额度耗尽 → parked（30 分钟起退避）。
	// 本用例的错误体是真实的 FreeUsageLimitError + 指向午夜的 Retry-After，
	// 因此走的是 parked 分支——断言必须接受它，否则测的就不是"预算是否
	// 约束住扇出"，而是"退化成哪一种状态"。
	cooled := 0
	zenKeyMu.Lock()
	for _, k := range keys {
		if until, ok := zenKeyCool[k]; ok && time.Now().Before(until) {
			cooled++
			continue
		}
		if ps, ok := zenKeyPark[k]; ok && ps.Day == time.Now().UTC().Format("2006-01-02") {
			cooled++
		}
	}
	zenKeyMu.Unlock()
	if cooled > wantMax {
		t.Fatalf("%d keys taken out of rotation, budget allows at most %d", cooled, wantMax)
	}
	if cooled < distinct {
		t.Fatalf("tried %d keys but only %d were cooled/parked; every 429'd key must leave rotation", distinct, cooled)
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

// 限流不得推进故障转移。429 说明 zen 服务正常、只是当前窗口超了；把它计入
// failover 会让一次瞬时节流把整个 zen 路由切到 cline 池（池为空时客户端拿到
// 401 "No accounts in pool"，比原来的 429 更难懂）。2026-10-30 实测事故。
func TestRateLimitDoesNotTriggerFailover(t *testing.T) {
	zenStateMu.Lock()
	zenFailCount = 0
	zenFailUntil = time.Time{}
	zenStateMu.Unlock()
	t.Cleanup(func() {
		zenStateMu.Lock()
		zenFailCount = 0
		zenFailUntil = time.Time{}
		zenStateMu.Unlock()
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "73000")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"limit"}}`))
	}))
	defer upstream.Close()

	savedCfg := getZenConfig()
	cp := *savedCfg
	cp.Enabled = true
	cp.BaseURL = upstream.URL
	cp.Keys = []string{"sk-rl1"}
	cp.Proxies = nil
	cp.Retries = 0 // 不睡 30s 重试：本用例只验证 failover 状态不被限流推进
	cp.Failover = true
	cp.FailoverCount = 1 // 一次失败就足以触发 failover，便于验证
	setZenConfig(&cp)
	t.Cleanup(func() { setZenConfig(savedCfg) })
	setZenModelForTest("rl-model", "")
	t.Cleanup(func() {
		zenKeyMu.Lock()
		zenKeyCool = map[string]time.Time{}
		zenKeyMu.Unlock()
	})

	params := map[string]any{
		"model":    "rl-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	// 用短 ctx 打断重试睡眠：本用例只关心"429 分支有没有推进 failover"，
	// 不需要等完 3 次 ×30s 的重试（cfg.Retries<=0 会被内部回落成 3）。
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _, _ = callZenAPI(ctx, params, true)

	zenStateMu.Lock()
	cnt, until := zenFailCount, zenFailUntil
	zenStateMu.Unlock()
	if cnt != 0 || !until.IsZero() {
		t.Fatalf("a 429 advanced failover state (count=%d until=%v); rate limiting is not degradation",
			cnt, until)
	}
	if zenFailedNow() {
		t.Fatal("zen must not be marked degraded by a rate limit")
	}
}

// cline 池为空时不得标记 zen 降级：转移目标不存在，路由过去只会得到
// 401 "No accounts in pool"。
func TestNoFailoverWhenClinePoolEmpty(t *testing.T) {
	zenStateMu.Lock()
	zenFailCount = 0
	zenFailUntil = time.Time{}
	zenStateMu.Unlock()

	// 确保 cline 池为空
	savedPool := loadPool()
	empty := *savedPool
	empty.Accounts = nil
	poolMu.Lock()
	pool = &empty
	poolMu.Unlock()
	t.Cleanup(func() {
		poolMu.Lock()
		pool = savedPool
		poolMu.Unlock()
		zenStateMu.Lock()
		zenFailCount = 0
		zenFailUntil = time.Time{}
		zenStateMu.Unlock()
	})

	for i := 0; i < 5; i++ {
		markZenFail()
	}
	if zenFailedNow() {
		t.Fatal("must not mark zen degraded when the cline pool has no accounts to fail over to")
	}
}

// 2026-10-30 实测事故回归：单 key 池 + 恒定 429 时，一次客户端请求要挂 101 秒
// （4 次上游调用 × 30s 睡眠重试）才把 429 交还，IDE 侧表现为"网关卡死"。
// key 已被冷却 20 分钟，30s 后重试必然再撞同一个 key——这类等待只是挂住连接。
//
// 断言：无可轮转 key 时立即失败，耗时远小于一次 30s 睡眠。
func TestZen429FailsFastWhenNoRotatableKey(t *testing.T) {
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "78410")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached."}}`))
	}))
	defer upstream.Close()

	// 单 key 池：撞 429 后没有任何可换的 key。
	savedCfg := getZenConfig()
	cfgCopy := *savedCfg
	cfgCopy.Enabled = true
	cfgCopy.BaseURL = upstream.URL
	cfgCopy.Keys = []string{"sk-single-key"}
	cfgCopy.Proxies = nil
	cfgCopy.Retries = 3
	setZenConfig(&cfgCopy)
	setZenModelForTest("bbb-single-model", "")
	t.Cleanup(func() {
		setZenConfig(savedCfg)
		zenKeyMu.Lock()
		zenKeyCool = map[string]time.Time{}
		zenKeyMu.Unlock()
	})

	start := time.Now()
	_, _, err := callZenAPI(t.Context(), map[string]any{
		"model":    "bbb-single-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}, true)
	elapsed := time.Since(start)

	var he *zenHTTPError
	if !errors.As(err, &he) || he.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want zenHTTPError 429", err)
	}
	// 一次上游调用就够：没有可换的 key，也没有必要睡。
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("upstream hit %d times, want 1 (no rotatable key => no pointless retries)", got)
	}
	// 宽松上界：只允许一次网络往返的时间，绝不允许一次 30s 睡眠。
	if elapsed > 10*time.Second {
		t.Fatalf("429 took %v to surface; must fail fast when no key can be rotated to", elapsed)
	}
}

// 有可轮转 key 时，睡眠重试的行为必须保持（只在真能换到健康 key 时才等）：
// 2 个 key，第一个恒 429、第二个恒 200 —— 请求必须成功，且不得睡 30s。
func TestZen429StillRotatesToHealthyKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("Authorization"), "bad") {
			w.Header().Set("Retry-After", "78410")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached."}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	savedCfg := getZenConfig()
	cfgCopy := *savedCfg
	cfgCopy.Enabled = true
	cfgCopy.BaseURL = upstream.URL
	cfgCopy.Keys = []string{"sk-bad", "sk-good"}
	cfgCopy.Proxies = nil
	cfgCopy.Retries = 3
	setZenConfig(&cfgCopy)
	setZenModelForTest("ccc-rotate-model", "")
	t.Cleanup(func() {
		setZenConfig(savedCfg)
		zenKeyMu.Lock()
		zenKeyCool = map[string]time.Time{}
		zenKeyMu.Unlock()
		zenKeyMu.Lock()
		zenKeyUsage = map[string]int64{}
		zenKeyMu.Unlock()
	})

	start := time.Now()
	resp, _, err := callZenAPI(t.Context(), map[string]any{
		"model":    "ccc-rotate-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}, true)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("rotation to the healthy key must succeed, got %v", err)
	}
	resp.Body.Close()
	if elapsed > 10*time.Second {
		t.Fatalf("healthy-key rotation took %v; switching keys must not sleep", elapsed)
	}
}

// 上游 429 必须原样透传为客户端 429。回 502 会让 IDE 判定网关不可用，
// 而 429 会让它按退避策略重试——语义完全不同。
func TestZenClientErrorStatusMapsRateLimit(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"upstream 429", &zenHTTPError{Status: http.StatusTooManyRequests, RateLimited: true}, http.StatusTooManyRequests},
		{"upstream 500", &zenHTTPError{Status: http.StatusInternalServerError}, http.StatusBadGateway},
		{"upstream 403 gate", &zenHTTPError{Status: http.StatusForbidden}, http.StatusBadGateway},
		{"plain error", errors.New("dial tcp: refused"), http.StatusBadGateway},
	}
	for _, c := range cases {
		if got := zenClientErrorStatus(c.err); got != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, got, c.want)
		}
	}
}

// zenAnyKeyRotatable 必须只统计"当前 key 之外、且未冷却"的 key。
func TestZenAnyKeyRotatable(t *testing.T) {
	savedCfg := getZenConfig()
	cfgCopy := *savedCfg
	cfgCopy.Keys = []string{"k1", "k2"}
	setZenConfig(&cfgCopy)
	t.Cleanup(func() {
		setZenConfig(savedCfg)
		zenKeyMu.Lock()
		zenKeyCool = map[string]time.Time{}
		zenKeyMu.Unlock()
	})

	if !zenAnyKeyRotatable("k1") {
		t.Fatal("k2 is idle and uncooled: rotation must be reported available")
	}
	cooldownZenKey("k2", time.Minute)
	if zenAnyKeyRotatable("k1") {
		t.Fatal("k2 is cooling: no key left to rotate to")
	}
	if !zenAnyKeyRotatable("k2") {
		t.Fatal("k1 is idle: rotation from k2 must be reported available")
	}
}
