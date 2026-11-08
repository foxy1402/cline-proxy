package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件固定 2026-11-08 全库审计里修掉的那批缺陷。每个测试都对应一个
// "改回旧实现就会红"的具体行为，不是覆盖率填充。

// ---------- 1. 自愈：一次失败只能记 +1 ----------

// collectStreamResponse 记一次失败后，调用方不得再记一次。
// 旧实现两处都记，failStreak 一次请求 +2，阈值 3 实际两次就冷却。
func TestStreamFailureCountedOncePerRequest(t *testing.T) {
	modelsMu.Lock()
	saved := modelsCache
	modelsCache = map[string]*ModelInfo{
		"probe/double-count:free": {ID: "probe/double-count:free", Status: ModelActive},
	}
	modelsMu.Unlock()
	t.Cleanup(func() {
		modelsMu.Lock()
		modelsCache = saved
		modelsMu.Unlock()
	})

	// 上游 200 + 帧内 error：collectStreamResponse 内部记一次失败。
	body := "data: {\"model\":\"probe/double-count:free\",\"error\":{\"message\":\"model not found\"}}\n\n"
	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
	if _, err := collectStreamResponse(resp, "probe/double-count:free"); err == nil {
		t.Fatal("in-band error frame must surface as an error")
	}

	modelsMu.Lock()
	streak := modelsCache["probe/double-count:free"].FailStreak
	modelsMu.Unlock()
	if streak != 1 {
		t.Fatalf("FailStreak = %d after ONE failed request, want 1 "+
			"(double-counting makes the 3-failure threshold fire after 2)", streak)
	}
}

// ---------- 2. 自愈：冷却中的模型不得被继续解析 ----------

// 只从列表里隐藏不够：IDE 客户端把模型名写死在配置里，根本不看 /v1/models。
// 解析路径必须同样拒绝冷却中的模型，否则持续流量会把冷却无限续期。
func TestCoolingModelIsNotRoutable(t *testing.T) {
	modelsMu.Lock()
	saved := modelsCache
	modelsCache = map[string]*ModelInfo{
		"probe/cooling:free": {ID: "probe/cooling:free", Status: ModelActive,
			CooldownTil: time.Now().Add(30 * time.Minute)},
		"probe/healthy:free": {ID: "probe/healthy:free", Status: ModelActive},
	}
	modelsMu.Unlock()
	t.Cleanup(func() {
		modelsMu.Lock()
		modelsCache = saved
		modelsMu.Unlock()
	})

	if modelInFreeList("probe/cooling:free") {
		t.Fatal("a cooling model must not be considered in the free list")
	}
	if got := normalizeRequestModel("probe/cooling:free"); got == "probe/cooling:free" {
		t.Fatal("normalizeRequestModel must not route to a cooling model")
	}
	if !modelInFreeList("probe/healthy:free") {
		t.Fatal("a healthy model must still resolve")
	}
}

// ---------- 3. 自愈：残缺 feed 不得清空目录 ----------

// catalog 拉取失败时 live 只剩 recommended 那几项。若照旧修剪，十几个只从
// catalog 发现的免费模型会被删掉，下一轮再以 FailStreak=0 重建 —— 自愈计数
// 被静默清零。
func TestPruneSkipsPartialFeedWhenCatalogFailed(t *testing.T) {
	modelsMu.Lock()
	saved := modelsCache
	modelsCache = map[string]*ModelInfo{
		"reflection/beam-free":        {ID: "reflection/beam-free", FailStreak: 2},
		"cline-free/solar-mini4":      {ID: "cline-free/solar-mini4"},
		"upstream/flint-free":         {ID: "upstream/flint-free", FailStreak: 1},
	}
	// 残缺的 live：只有 recommended 一项（catalog 那一路没拉到）。
	live := map[string]bool{"cline-free/solar-mini4": true}
	pruned := pruneModelsLocked(live, false)
	beamKept := modelsCache["reflection/beam-free"] != nil
	streak := modelsCache["reflection/beam-free"].FailStreak
	modelsMu.Unlock()
	modelsCache = saved

	if pruned != 0 {
		t.Fatalf("pruned %d model(s) from a PARTIAL feed; a transient catalog failure must not empty the catalog", pruned)
	}
	if !beamKept {
		t.Fatal("catalog-only model deleted on a partial feed")
	}
	if streak != 2 {
		t.Fatalf("failStreak reset to %d by a partial-feed prune; self-healing becomes unreachable", streak)
	}

	// 正常 feed（catalog ok）仍然要能真的修剪死模型。
	modelsMu.Lock()
	modelsCache = map[string]*ModelInfo{
		"live/ok":    {ID: "live/ok"},
		"dead/one":   {ID: "dead/one"},
	}
	pruned2 := pruneModelsLocked(map[string]bool{"live/ok": true}, true)
	deadGone := modelsCache["dead/one"] == nil
	modelsCache = saved
	modelsMu.Unlock()

	if pruned2 != 1 || !deadGone {
		t.Fatalf("healthy feed must still prune dead models (pruned=%d deadGone=%v)", pruned2, deadGone)
	}
}

// ---------- 4. 429 文案不得把健康账号冻 18 小时 ----------

func TestQuotaDetectionIgnoresTransientRateLimit(t *testing.T) {
	transient := []string{
		"Rate limit exceeded",
		"Rate limit exceeded. Please slow down.",
		`{"error":"too many requests"}`,
		"limit reached for this window, retry shortly",
	}
	for _, b := range transient {
		if isClineQuotaExhausted(b) {
			t.Errorf("transient 429 body %q misclassified as quota exhaustion "+
				"(would cool a healthy account 18h instead of 20m)", b)
		}
	}
	quota := []string{
		"quota exceeded",
		"You have exceeded your quota",
		"insufficient credit",
		"out of credits",
		"daily limit reached",
		"monthly limit exceeded",
	}
	for _, b := range quota {
		if !isClineQuotaExhausted(b) {
			t.Errorf("real quota body %q not detected as exhaustion", b)
		}
	}
}

// ---------- 5. SSE 分块不得在 2048 整数倍上越界 ----------

func TestEmitChatAsSSEExactChunkBoundary(t *testing.T) {
	for _, n := range []int{2048, 4096, 6144} {
		rec := &flushRecorder{header: http.Header{}}
		msg := map[string]any{"role": "assistant", "content": strings.Repeat("a", n)}
		emitChatAsSSE(rec, map[string]any{
			"choices": []any{map[string]any{"message": msg, "finish_reason": "stop"}},
		}, nil)
		if !strings.Contains(rec.body.String(), "data: [DONE]") {
			t.Fatalf("len=%d: stream did not terminate cleanly (panic truncated it before [DONE])", n)
		}
	}
}

// ---------- 6. 多字节字符不得被切断 ----------

func TestEmitChatAsSSEMultibyteSafe(t *testing.T) {
	rec := &flushRecorder{header: http.Header{}}
	// 3 字节 rune 的整数倍，试探 rune 边界回退是否生效
	msg := map[string]any{"role": "assistant", "content": strings.Repeat("中", 1500)}
	emitChatAsSSE(rec, map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": "stop"}},
	}, nil)
	out := rec.body.String()
	if strings.Contains(out, "\uFFFD") {
		t.Fatal("content was split mid-rune: stream contains U+FFFD replacement characters")
	}
}

// ---------- 7. zen 别名必须跟随目录/学习器更新 ----------

// 别名表只在 initZenModels 里绑一次，而后续所有写入都是写时复制到
// zenModels[canonical] 上。别名若继续返回旧指针，上下文窗口与端点判定
// 全都停在 seed 快照（mimo/ling/pickle/muse-spark* 全中招）。
func TestZenAliasFollowsCatalogUpdates(t *testing.T) {
	zenModelsMu.Lock()
	savedModels, savedAliases := zenModels, zenAliases
	zenModels = map[string]*ZenModel{
		"canonical/model-free": {ID: "canonical/model-free", Context: 1000, Output: 100, Upstream: ""},
	}
	zenAliases = map[string]*ZenModel{
		"mimo": zenModels["canonical/model-free"],
	}
	zenModelsMu.Unlock()
	t.Cleanup(func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	})

	// 目录同步/学习器用写时复制替换 canonical 条目
	zenModelsMu.Lock()
	cp := *zenModels["canonical/model-free"]
	cp.Context = 200000
	cp.Upstream = "responses"
	zenModels[cp.ID] = &cp
	zenModelsMu.Unlock()

	m, ok := resolveZenModel("mimo")
	if !ok {
		t.Fatal("alias must resolve")
	}
	if m.Context != 200000 || m.Upstream != "responses" {
		t.Fatalf("alias returned the stale seed snapshot (ctx=%d upstream=%q); "+
			"it must follow the canonical entry", m.Context, m.Upstream)
	}
}

// ---------- 8. 别名环不得把进程递归到栈溢出 ----------

// 持久化别名文件是外部可编辑的。A→A 自环或 A→B→A 成环必须被安全拒绝：
// 没有深度上限时递归会 stack overflow，那是 fatal error，recover 接不住。
func TestZenAliasCycleDoesNotOverflowStack(t *testing.T) {
	zenDepMu.Lock()
	saved := zenDepAliases
	zenDepAliases = map[string]*zenDepEntry{
		"loop-a-free": {Replacement: "loop-b-free"},
		"loop-b-free": {Replacement: "loop-a-free"},
		"self-free":   {Replacement: "self-free"},
	}
	zenDepMu.Unlock()
	t.Cleanup(func() {
		zenDepMu.Lock()
		zenDepAliases = saved
		zenDepMu.Unlock()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, ok := resolveZenModel("loop-a-free"); ok {
			t.Error("a cyclic alias must not resolve to a model")
		}
		if _, ok := resolveZenModel("self-free"); ok {
			t.Error("a self-referential alias must not resolve to a model")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("alias resolution did not terminate: cyclic alias chain is recursing")
	}
}

// ---------- 9. 停放中的 key 不得被当作可轮转 ----------

// parkZenKey 会把 key 从 zenKeyCool 里删掉，所以"不在冷却"包含"已停放"。
// 漏判会让 429 睡眠重试打回同一个额度耗尽的 key（101s 挂起那个 bug）。
func TestZenAnyKeyRotatableSkipsParkedKeys(t *testing.T) {
	savedCfg := getZenConfig()
	cfgCopy := *savedCfg
	cfgCopy.Enabled = true
	cfgCopy.Keys = []string{"sk-parked-key", "sk-cur-key"}
	setZenConfig(&cfgCopy)

	zenKeyMu.Lock()
	savedCool, savedPark := zenKeyCool, zenKeyPark
	zenKeyCool = map[string]time.Time{}
	zenKeyPark = map[string]zenParkState{
		"sk-parked-key": {Day: time.Now().UTC().Format("2006-01-02"), Until: time.Now().Add(time.Hour), Backoff: time.Hour},
	}
	zenKeyMu.Unlock()
	t.Cleanup(func() {
		setZenConfig(savedCfg)
		zenKeyMu.Lock()
		zenKeyCool, zenKeyPark = savedCool, savedPark
		zenKeyMu.Unlock()
	})

	if zenAnyKeyRotatable("sk-cur-key") {
		t.Fatal("a parked key reported as rotatable: 429 handling will sleep-retry an " +
			"exhausted key and hold the client connection for ~100s")
	}
}

// ---------- 10. 管理面板：JSON 不得被缓存，跨站表单必须被拒 ----------

func TestWriteAPISetsNoStore(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAPI(rec, http.StatusOK, apiResponse{Success: true})
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q; admin payloads carry credentials and must not be cached", got)
	}
}

func TestAdminCORSRejectsCrossOriginPost(t *testing.T) {
	called := false
	h := adminCORS(func(w http.ResponseWriter, r *http.Request) { called = true })

	// 跨站 POST：Origin 与 Host 不同
	req := httptest.NewRequest("POST", "http://gateway.local/admin/api/config/update",
		strings.NewReader(`{}`))
	req.Host = "gateway.local"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	h(rec, req)
	if called {
		t.Fatal("cross-origin state-changing request reached the handler")
	}

	// text/plain 走 CORS 简单请求，必须被 Content-Type 关卡挡住
	called = false
	req2 := httptest.NewRequest("POST", "http://gateway.local/admin/api/accounts/delete-all",
		strings.NewReader(`{}`))
	req2.Host = "gateway.local"
	req2.Header.Set("Content-Type", "text/plain")
	rec2 := httptest.NewRecorder()
	h(rec2, req2)
	if called {
		t.Fatal("text/plain POST reached a state-changing handler (classic CSRF " +
			"simple-request bypass; json.Unmarshal does not care about Content-Type)")
	}

	// 同源正常请求必须放行
	called = false
	req3 := httptest.NewRequest("POST", "http://gateway.local/admin/api/config/update",
		strings.NewReader(`{}`))
	req3.Host = "gateway.local"
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Origin", "http://gateway.local")
	rec3 := httptest.NewRecorder()
	h(rec3, req3)
	if !called {
		t.Fatal("same-origin POST was rejected")
	}
}

// ---------- 11. baseURL 不得指向内网 ----------

func TestValidateZenUpstreamBase(t *testing.T) {
	bad := []string{
		"http://127.0.0.1:8080/v1",
		"http://localhost/v1",
		"http://169.254.169.254/latest/meta-data",
		"http://10.0.0.5/v1",
		"http://192.168.1.10/v1",
		"ftp://opencode.ai/v1",
		"not-a-url",
	}
	for _, b := range bad {
		if err := validateZenUpstreamBase(b); err == nil {
			t.Errorf("%q accepted; baseURL receives the zen key, so internal targets "+
				"are SSRF + credential exfiltration", b)
		}
	}
	good := []string{
		"https://opencode.ai/inference/openai/v1",
		"https://relay.example.com/v1",
		"http://203.0.113.10:8080/v1",
	}
	for _, g := range good {
		if err := validateZenUpstreamBase(g); err != nil {
			t.Errorf("%q rejected: %v", g, err)
		}
	}
}

// ---------- 12. 环境变量整数不得溢出 ----------

func TestEnvIntRejectsOverflow(t *testing.T) {
	t.Setenv("AUDIT_TEST_BIG", "99999999999999999999")
	if n, ok := envInt("AUDIT_TEST_BIG"); ok {
		t.Fatalf("overflowing value accepted as %d (wraps negative, inverting the guard)", n)
	}
	t.Setenv("AUDIT_TEST_OK", "32")
	if n, ok := envInt("AUDIT_TEST_OK"); !ok || n != 32 {
		t.Fatalf("valid value rejected: n=%d ok=%v", n, ok)
	}
}

// ---------- 13. 请求日志里的 model 必须有长度上限 ----------

func TestReqLogModelTruncated(t *testing.T) {
	t.Setenv("LOG_REQUESTS", "true")
	reqLogsMu.Lock()
	saved := reqLogs
	reqLogs = nil
	reqLogsMu.Unlock()
	t.Cleanup(func() {
		reqLogsMu.Lock()
		reqLogs = saved
		reqLogsMu.Unlock()
	})

	AppendReqLog(RequestLog{Model: strings.Repeat("A", 1<<20)})
	logs := LoadRequestLogs()
	if len(logs) == 0 {
		t.Fatal("log entry not recorded")
	}
	if len(logs[len(logs)-1].Model) > 256 {
		t.Fatalf("model field kept %d bytes; a huge model string in the request body "+
			"becomes an equally huge in-memory log entry (500-deep ring)", len(logs[len(logs)-1].Model))
	}
}

// ---------- 14. 并发学习不得丢更新 ----------

// 两个并发请求学不同模型时，早先会同时写同一个 .tmp 路径：一个 rename 成功、
// 另一个 ENOENT，最终文件可能是旧快照（少一个模型）甚至半截 JSON。
func TestClineStreamConcurrentLearnsPersistAll(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	clineStreamMu.Lock()
	saved := clineStreamLearned
	clineStreamLearned = nil
	clineStreamMu.Unlock()
	t.Cleanup(func() {
		clineStreamMu.Lock()
		clineStreamLearned = saved
		clineStreamMu.Unlock()
	})

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			learnClineStreamRequired("probe/model-" + string(rune('a'+i)) + ":free")
		}(i)
	}
	wg.Wait()
	// 等落盘队列排空（学习路径是同步写的，这里只是保险）
	time.Sleep(100 * time.Millisecond)

	raw, rerr := os.ReadFile(clineStreamFile())
	if rerr != nil {
		t.Fatalf("read learned file: %v", rerr)
	}
	var learned map[string]string
	if err := json.Unmarshal(raw, &learned); err != nil {
		t.Fatalf("learned file is not valid JSON (torn write): %v", err)
	}
	if len(learned) != n {
		t.Fatalf("persisted %d/%d learned models; concurrent saves clobbered each other "+
			"(lost update => the forgotten model pays the full non-stream retry again)", len(learned), n)
	}
}
