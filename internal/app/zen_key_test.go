package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// zen 探测（面板 per-key "Test" 按钮）的行为契约：
//   - 整个探测固定在被探测的 key 上（绝不中途换 key、绝不影响正常轮转）；
//   - 2xx  → active，且清除该 key 的冷却；
//   - 429  → cooldown，回报上游 Retry-After 决定的预计恢复时间；
//   - 403  → error（会话已死），收割机被顺带触发。
//
// 上游用 httptest 假服务（与 zen_learn_confirm_test.go 同一模式）。

func setupZenProbeTest(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	// 默认 ZEN_HARVEST_BIN (/app/bin/opencode) 在测试机上不存在，harvestEnabled()
	// 为 false 会让 harvestOnForbidden 在计数前就返回；指向测试二进制自身
	// （只做 os.Stat 存在性检查，503 计数路径不会真执行它）。
	if exe, err := os.Executable(); err == nil {
		t.Setenv("ZEN_HARVEST_BIN", exe)
	}
	// 清掉其他测试留下的冷却/连败状态：这些是包级 map，串行测试间会渗漏
	//（sk-pin333 的 60s 冷却曾渗进后续用例）。
	zenKeyMu.Lock()
	zenKeyCool = map[string]time.Time{}
	zenKeyMu.Unlock()
	harvestMu.Lock()
	harvestFails = map[string]int{}
	harvestLastTry = map[string]time.Time{}
	harvestMu.Unlock()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)

	savedCfg := getZenConfig()
	cfgCopy := *savedCfg
	cfgCopy.Enabled = true
	cfgCopy.BaseURL = upstream.URL
	cfgCopy.Keys = []string{"sk-aaa111", "sk-bbb222", "sk-pin333"}
	cfgCopy.Proxies = nil
	cfgCopy.Retries = 0
	setZenConfig(&cfgCopy)
	t.Cleanup(func() { setZenConfig(savedCfg) })

	savedModels, savedAliases := zenModels, zenAliases
	setZenModelForTest("aaa-probe-model", "")
	t.Cleanup(func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	})
}

func TestUncoolZenKey(t *testing.T) {
	key := "sk-uncool-test"
	cooldownZenKey(key, time.Hour)
	if !zenKeyCooling(key) {
		t.Fatal("precondition: key should be cooling")
	}
	uncoolZenKey(key)
	if zenKeyCooling(key) {
		t.Fatal("uncoolZenKey did not clear the cooldown")
	}
	uncoolZenKey("") // 无害 no-op，不得 panic
}

func TestZenKeyTestProbeSuccessClearsCooldown(t *testing.T) {
	var hits int32
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	})
	key := "sk-aaa111"
	cooldownZenKey(key, time.Hour) // 预置冷却：探测成功必须把它清掉
	if !zenKeyCooling(key) {
		t.Fatal("precondition: key should be cooling")
	}

	result, status := testZenKey(key, 0, "")
	if status != "active" {
		t.Fatalf("status = %q (reason=%v), want active", status, result["reason"])
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits)
	}
	if zenKeyCooling(key) {
		t.Fatal("successful probe did not clear the cooldown")
	}
	if got := result["model"]; got != "aaa-probe-model" {
		t.Fatalf("probe model = %v", got)
	}
	if result["latencyMs"] == nil {
		t.Fatal("latencyMs missing from result")
	}
}

func TestZenKeyTestProbe429ReportsCooldown(t *testing.T) {
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"quota exceeded"}`))
	})
	key := "sk-bbb222"
	result, status := testZenKey(key, 1, "")
	if status != "cooldown" {
		t.Fatalf("status = %q, want cooldown", status)
	}
	if !zenKeyCooling(key) {
		t.Fatal("429 probe did not cool the key (cooldownZenKey side effect missing)")
	}
	until, ok := zenKeyCooldownUntil(key)
	if !ok {
		t.Fatal("cooldownUntil missing after 429 probe")
	}
	if rem := time.Until(until); rem < 110*time.Second || rem > 121*time.Second {
		t.Fatalf("cooldown remaining = %v, want ~2m (Retry-After: 120)", rem)
	}
	if result["cooldownUntil"] == nil || result["remaining"] == nil {
		t.Fatalf("result missing cooldown fields: %v", result)
	}
}

func TestZenKeyTestProbe403ReportsSessionDeadAndTriggersHarvest(t *testing.T) {
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		// 纯 FreeTier 403：body 不得含限流关键词（否则 isRateLimited 会把它
		// 当 429 分支处理，测不到会话死亡路径）
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"type":"free_tier_error","message":"session check failed"}}`))
	})
	key := "sk-aaa111"
	result, status := testZenKey(key, 0, "")
	if status != "error" {
		t.Fatalf("status = %q, want error", status)
	}
	if got := result["httpStatus"]; got != http.StatusForbidden {
		t.Fatalf("httpStatus = %v, want 403", got)
	}
	// harvestOnForbidden 是异步触发的（go ...），给它一点时间落账
	deadline := time.Now().Add(2 * time.Second)
	fails := 0
	for {
		harvestMu.Lock()
		fails = harvestFails[key]
		harvestMu.Unlock()
		if fails == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fails != 1 {
		t.Fatalf("harvest fail counter = %d, want 1 (probe must trigger the harvester)", fails)
	}
	harvestMu.Lock()
	delete(harvestFails, key)
	harvestMu.Unlock()
}

// pin 语义：429 时绝不换 key、绝不重试——一次探测恰好一次上游调用，且始终是
// 被探测的 key。非 pin 路径在同样输入下会切换到下一个 key（对照组）。
func TestZenCallPinKeepsSingleKey(t *testing.T) {
	var hits int32
	var seenKeys = map[string]int{}
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		seenKeys[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]++
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"quota"}`))
	})

	params := map[string]any{
		"model":    "aaa-probe-model",
		"messages": []any{map[string]any{"role": "user", "content": "Reply with exactly: OK"}},
		"max_tokens": 64,
	}

	_, _, err := callZenAPI(t.Context(), params, true, zenCallOpts{pinKey: "sk-pin333"})
	var he *zenHTTPError
	if !errors.As(err, &he) || he.Status != http.StatusTooManyRequests {
		t.Fatalf("pinned probe err = %v, want zenHTTPError 429", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("pinned probe made %d upstream calls, want exactly 1 (no key switch, no retry)", hits)
	}
	if n := seenKeys["sk-pin333"]; n != 1 {
		t.Fatalf("pinned key usage = %d, want 1; seen=%v", n, seenKeys)
	}
	for k := range seenKeys {
		if k != "sk-pin333" {
			t.Fatalf("pinned probe touched another key: %q (seen=%v)", k, seenKeys)
		}
	}
}

func TestZenKeyTestHandlerRejectsBadIndex(t *testing.T) {
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for an invalid index")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/api/zen/keys/test", strings.NewReader(`{"index":99}`))
	handleZenKeyTest(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("index out of range -> %d, want 404", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/admin/api/zen/keys/test", strings.NewReader(`{invalid`))
	handleZenKeyTest(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("invalid JSON -> %d, want 400", rec2.Code)
	}
}

// ===== 审计修复的回归守卫（2026-09-19 审计 624bec2/6e9bb5e）=====

// P1 回归守卫：面板 JS 读 r.status（与 cline testAccount 同契约）——Data 里
// 必须有 status 字段，否则每个 toast 都渲染成 "— undefined" 并套错误样式。
func TestZenKeyTestHandlerPutsStatusInData(t *testing.T) {
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/api/zen/keys/test", strings.NewReader(`{"index":0}`))
	handleZenKeyTest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handler -> %d, want 200", rec.Code)
	}
	var body struct {
		Success bool           `json:"success"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Success || body.Message != "active" {
		t.Fatalf("success=%v message=%q, want true/active", body.Success, body.Message)
	}
	if body.Data["status"] != "active" {
		t.Fatalf("data.status = %v, want \"active\" (the panel reads r.status)", body.Data["status"])
	}
}

// P2：限流型 403（错误体带限流关键词 → isRateLimited 命中）必须按"冷却"回报，
// 绝不能说"会话已死/收割机已触发"——那个分支根本没有触发收割机。
func TestZenKeyTestRateLimitShaped403ReportsCooldown(t *testing.T) {
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down: rate limit exceeded"}}`))
	})
	key := "sk-aaa111"
	result, status := testZenKey(key, 0, "")
	if status != "cooldown" {
		t.Fatalf("status = %q (%v), want cooldown — keyword-403 must take the rate-limit branch", status, result["reason"])
	}
	if !zenKeyCooling(key) {
		t.Fatal("keyword-403 probe did not cool the key")
	}
	harvestMu.Lock()
	fails := harvestFails[key]
	harvestMu.Unlock()
	if fails != 0 {
		t.Fatalf("harvest fail counter = %d, want 0 (rate-limited 403 must not trigger the harvester)", fails)
	}
	if result["cooldownUntil"] == nil || result["remaining"] == nil {
		t.Fatalf("cooldown fields missing: %v", result)
	}
	if strings.Contains(result["reason"].(string), "session") {
		t.Fatalf("reason claims a session problem: %v", result["reason"])
	}
}

// 覆盖 responses 上游的探测路径：Upstream=="responses" 的模型走
// callZenResponsesAPI（此前该分支零测试覆盖）。
func TestZenKeyTestResponsesUpstreamPath(t *testing.T) {
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("upstream path = %q, want /responses", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\"}}\n\n"))
	})
	// 把模型表整体换成 responses 上游模型（setup 的 cleanup 会恢复原表）
	setZenModelForTest("aaa-probe-model", "responses")

	result, status := testZenKey("sk-bbb222", 1, "")
	if status != "active" {
		t.Fatalf("status = %q (%v), want active", status, result["reason"])
	}
	if result["model"] != "aaa-probe-model" {
		t.Fatalf("model = %v", result["model"])
	}
}

// P2 修复守卫：pinned 探测的上游 5xx 不得推进全局故障转移——否则连点几次
// Test 撞上上游 500，会把全部正常 free-zen 流量切去 cline 池 5 分钟。
func TestZenProbeDoesNotPolluteFailover(t *testing.T) {
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Internal server error"}`))
	})
	zenStateMu.Lock()
	savedCount, savedUntil := zenFailCount, zenFailUntil
	zenFailCount, zenFailUntil = 0, time.Time{}
	zenStateMu.Unlock()
	defer func() {
		zenStateMu.Lock()
		zenFailCount, zenFailUntil = savedCount, savedUntil
		zenStateMu.Unlock()
	}()

	_, status := testZenKey("sk-aaa111", 0, "")
	if status != "error" {
		t.Fatalf("status = %q, want error", status)
	}
	zenStateMu.Lock()
	cnt, until := zenFailCount, zenFailUntil
	zenStateMu.Unlock()
	if cnt != 0 || !until.IsZero() {
		t.Fatalf("pinned probe polluted failover state: count=%d until=%v", cnt, until)
	}
}

// public 哨兵没有可探测的凭据。
func TestZenKeyTestHandlerRejectsPublicKey(t *testing.T) {
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for the public sentinel")
	})
	saved := getZenConfig()
	cfg := *saved
	cfg.Keys = []string{"public"}
	setZenConfig(&cfg)
	t.Cleanup(func() { setZenConfig(saved) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/api/zen/keys/test", strings.NewReader(`{"index":0}`))
	handleZenKeyTest(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("public key probe -> %d, want 400", rec.Code)
	}
}

// 探测模型的优先级：big-pickle（免费层默认别名）> live 同步条目 > 种子兜底。
func TestZenProbeModelPreferenceOrder(t *testing.T) {
	savedModels, savedAliases := zenModels, zenAliases
	defer func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	}()

	set := func(models ...ZenModel) {
		zenModelsMu.Lock()
		zenModels, zenAliases = make(map[string]*ZenModel), make(map[string]*ZenModel)
		for _, m := range models {
			cp := m
			zenModels[cp.ID] = &cp
		}
		zenModelsMu.Unlock()
	}

	// big-pickle 优先于 id 更小的 live 模型（默认别名的地位最高）。
	set(
		ZenModel{ID: "aaa-live-model", Source: "live"},
		ZenModel{ID: "big-pickle", Source: "seed"},
	)
	if got := zenProbeModel(); got == nil || got.ID != "big-pickle" {
		t.Fatalf("got %v, want big-pickle (default free-tier alias wins)", got)
	}

	// 没有 big-pickle 时取 live 同步条目（上游当前确实在供），即使种子模型
	// 的 id 更小。
	set(
		ZenModel{ID: "aaa-seed-model", Source: "seed"},
		ZenModel{ID: "zz-live-model", Source: "live"},
	)
	if got := zenProbeModel(); got == nil || got.ID != "zz-live-model" {
		t.Fatalf("got %v, want zz-live-model (live beats smaller-id seed)", got)
	}

	// 纯种子兜底（冷启动、同步不可达）：最小 id。
	set(
		ZenModel{ID: "bbb-seed-model", Source: "seed"},
		ZenModel{ID: "aaa-seed-model", Source: "seed"},
	)
	if got := zenProbeModel(); got == nil || got.ID != "aaa-seed-model" {
		t.Fatalf("got %v, want aaa-seed-model (deterministic fallback)", got)
	}

	// 目录被清空时 initZenModels 会重新播种（冷启动契约）：探测返回
	// big-pickle，而不是 nil——"目录空"在生产里只在首启瞬间出现。
	set()
	if got := zenProbeModel(); got == nil || got.ID != "big-pickle" {
		t.Fatalf("got %v, want big-pickle re-seeded on an empty table", got)
	}
}

// 显式指定探测模型（面板下拉）：指定的模型必须生效，优先于自动选择。
func TestZenKeyTestHonorsExplicitModel(t *testing.T) {
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	// 两模型表：自动选择会挑 live 的 zzz-…，显式指定必须仍用 aaa-…。
	zenModelsMu.Lock()
	savedModels, savedAliases := zenModels, zenAliases
	zenModels = map[string]*ZenModel{
		"aaa-probe-model": {ID: "aaa-probe-model", Source: "seed"},
		"zzz-probe-model": {ID: "zzz-probe-model", Source: "live"},
	}
	zenAliases = map[string]*ZenModel{}
	zenModelsMu.Unlock()
	t.Cleanup(func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	})

	result, status := testZenKey("sk-aaa111", 0, "aaa-probe-model")
	if status != "active" {
		t.Fatalf("status = %q (%v), want active", status, result["reason"])
	}
	if result["model"] != "aaa-probe-model" {
		t.Fatalf("model = %v, want aaa-probe-model (explicit pick beats auto)", result["model"])
	}

	// 未指定的模型：解析失败必须报错，绝不悄悄退回自动选择。
	result, status = testZenKey("sk-aaa111", 0, "no-such-model")
	if status != "error" {
		t.Fatalf("status = %q, want error for an unknown model", status)
	}
	if !strings.Contains(result["reason"].(string), "no-such-model") {
		t.Fatalf("reason = %v, want it to name the bad model", result["reason"])
	}
}

func TestResolveZenKeyIdentity(t *testing.T) {
	// 普通 Zen API key
	token, org, isConsole := resolveZenKeyIdentity("sk-plain-123456")
	if token != "sk-plain-123456" || org != "" || isConsole {
		t.Fatalf("resolve legacy key failed: token=%s org=%s isConsole=%v", token, org, isConsole)
	}

	// Console OAuth 令牌 (单 token)
	token, org, isConsole = resolveZenKeyIdentity("st_user_abc123")
	if token != "st_user_abc123" || org != "" || !isConsole {
		t.Fatalf("resolve console key failed: token=%s org=%s isConsole=%v", token, org, isConsole)
	}

	// Console 多账号带 org 后缀
	token, org, isConsole = resolveZenKeyIdentity("st_multi_user#org_dept_it")
	if token != "st_multi_user" || org != "org_dept_it" || !isConsole {
		t.Fatalf("resolve console multi-account failed: token=%s org=%s isConsole=%v", token, org, isConsole)
	}
}

func TestZenKeyTestConsoleTokenHeaders(t *testing.T) {
	var gotAuth, gotClient, gotOrg, gotSession string
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotClient = r.Header.Get("x-opencode-client")
		gotOrg = r.Header.Get("x-opencode-org-id")
		gotSession = r.Header.Get("x-opencode-session")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	})

	consoleKey := "st_test_access_token_123#org_custom_456"
	result, status := testZenKey(consoleKey, 0, "")
	if status != "active" {
		t.Fatalf("testZenKey status = %q (%v), want active", status, result["reason"])
	}

	if gotAuth != "Bearer st_test_access_token_123" {
		t.Fatalf("auth header = %q, want 'Bearer st_test_access_token_123'", gotAuth)
	}
	if gotClient != "cli" {
		t.Fatalf("client header = %q, want 'cli'", gotClient)
	}
	if gotOrg != "org_custom_456" {
		t.Fatalf("org header = %q, want 'org_custom_456'", gotOrg)
	}
	if !strings.HasPrefix(gotSession, "ses_") || len(gotSession) < 20 {
		t.Fatalf("session header = %q, want canonical ses_ format", gotSession)
	}
}

func TestZenKeyMultiAccountPoolRotation(t *testing.T) {
	var seenTokens []string
	setupZenProbeTest(t, func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		seenTokens = append(seenTokens, token)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	})

	savedCfg := getZenConfig()
	cfg := *savedCfg
	cfg.Keys = []string{"st_acc1#org_1", "st_acc2#org_2"}
	setZenConfig(&cfg)
	defer setZenConfig(savedCfg)

	params := map[string]any{
		"model":    "aaa-probe-model",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}

	// 连续调用 2 次，应该轮转使用两个不同账号
	_, _, err1 := callZenAPI(t.Context(), params, false)
	if err1 != nil {
		t.Fatalf("callZenAPI 1 failed: %v", err1)
	}
	_, _, err2 := callZenAPI(t.Context(), params, false)
	if err2 != nil {
		t.Fatalf("callZenAPI 2 failed: %v", err2)
	}

	if len(seenTokens) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(seenTokens))
	}
	if seenTokens[0] != "st_acc1" || seenTokens[1] != "st_acc2" {
		t.Fatalf("expected round-robin rotation [st_acc1, st_acc2], got %v", seenTokens)
	}
}

func TestCanonicalSessionIDFormat(t *testing.T) {
	re := regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	for i := 0; i < 50; i++ {
		sid := CanonicalSessionID()
		if !re.MatchString(sid) {
			t.Fatalf("session ID %q does not match required format", sid)
		}
	}
}

func TestConsoleAuthDiscovery(t *testing.T) {
	auth, err := GetConsoleAuth()
	if err != nil {
		t.Logf("Console auth not available: %v", err)
		return
	}
	if auth.AccessToken == "" {
		t.Errorf("expected non-empty access token")
	}
	if auth.ActiveOrgID == "" {
		t.Errorf("expected non-empty active org id")
	}
}
