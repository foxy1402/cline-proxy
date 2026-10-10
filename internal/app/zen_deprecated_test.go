package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 弃用迁移端到端（httptest 假上游，模式同 zen_learn_confirm_test.go）：
// mimo-v2.5-free 上游 410 + replacement → 本次请求透明改走继任模型并成功，
// 别名持久化，解析层兜底生效。
func TestDeprecated410RemapsAndPersists(t *testing.T) {
	var mu map[string]int // 占位避免误用
	_ = mu
	handlerCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalls++
		var body struct {
			Model string `json:"model"`
		}
		readJSONBody(t, r, &body)
		if body.Model == "mimo-v2.5-free" {
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte(`{"error":{"type":"model_deprecated","message":"This model has been removed","replacement":"mimo-v2.6-flash-free"}}`))
			return
		}
		// 继任模型：正常 chat 应答
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}],"model":"` + body.Model + `"}`))
	}))
	defer upstream.Close()

	setupZenDepTest(t, upstream)

	params := map[string]any{
		"model":    "mimo-v2.5-free",
		"messages": []any{map[string]any{"role": "user", "content": "Reply with exactly: OK"}},
	}
	resp, _, err := callZenAPI(t.Context(), params, true)
	if err != nil {
		t.Fatalf("migrated request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after migration", resp.StatusCode)
	}
	if got := params["model"]; got != "mimo-v2.6-flash-free" {
		t.Fatalf("params model = %v, want the replacement", got)
	}
	if handlerCalls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (original 410 + migrated 200)", handlerCalls)
	}
	if repl := zenDeprecatedReplacement("mimo-v2.5-free"); repl != "mimo-v2.6-flash-free" {
		t.Fatalf("alias not persisted: %q", repl)
	}
	// 解析层兜底：原始 ID 从模型表移除后落到继任模型
	zenModelsMu.Lock()
	delete(zenModels, "mimo-v2.5-free")
	zenModelsMu.Unlock()
	if m, ok := resolveZenModel("mimo-v2.5-free"); !ok || m.ID != "mimo-v2.6-flash-free" {
		t.Fatalf("resolveZenModel(mimo-v2.5-free) = %v, %v; want replacement", m, ok)
	}
}

// 410 迁移至多一跳：继任模型本身也是弃用别名时拒绝迁移，原始错误照常返回。
func TestDeprecated410RefusesChainedRemap(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		readJSONBody(t, r, &body)
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":{"message":"gone","replacement":"mimo-v3-free"}}`))
	}))
	defer upstream.Close()
	setupZenDepTest(t, upstream)

	// 预置别名 a→b，再请求 a 且上游声称 a→b（b 已是弃用别名）→ 拒绝
	if zenRecordDeprecation("mimo-v2.5-free", "mimo-v2.6-flash-free") == "" {
		t.Fatal("precondition: first alias must record")
	}
	params := map[string]any{
		"model":    "mimo-v2.5-free",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	_, _, err := callZenAPI(t.Context(), params, true)
	var he *zenHTTPError
	if err == nil || !asZenHTTPError(err, &he) || he.Status != http.StatusGone {
		t.Fatalf("err = %v, want the original 410 (chained remap refused)", err)
	}
	if got := params["model"]; got != "mimo-v2.5-free" {
		t.Fatalf("params model = %v, want unchanged", got)
	}
}

// 400 "Model is unavailable"：标记死亡（路由层拒绝、不再烧上游请求），
// 目录同步成功后解除。
func TestUnavailable400MarksDeadUntilSync(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Model is unavailable"}}`))
	}))
	defer upstream.Close()
	setupZenDepTest(t, upstream)

	params := map[string]any{
		"model":    "qwen3.6-plus-free",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	_, _, err := callZenAPI(t.Context(), params, true)
	if err == nil {
		t.Fatal("expected the original 400 to surface")
	}
	if !zenModelDead("qwen3.6-plus-free") {
		t.Fatal("model must be marked dead after a 400 unavailable")
	}
	// 路由层直接拒绝，不再消耗上游调用
	if got := routeModel("qwen3.6-plus-free"); got != "reject" {
		t.Fatalf("routeModel = %q, want reject while marked dead", got)
	}
	before := calls
	_ = before
	// 同步成功（registry 可达）解除标记
	markRegistryHealthyForTest(t)
	if _, err := syncZenModels(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if zenModelDead("qwen3.6-plus-free") {
		t.Fatal("dead mark must clear after a successful catalog sync")
	}
}

// 别名不许遮蔽真实模型：原始 ID 重新上线（目录里又出现）时优先解析原模型。
func TestAliasNeverShadowsRealModel(t *testing.T) {
	setupZenDepTest(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	if zenRecordDeprecation("old-free", "new-free") == "" {
		t.Fatal("record alias")
	}
	zenModelsMu.Lock()
	zenModels["old-free"] = &ZenModel{ID: "old-free", Source: "live"}
	zenModelsMu.Unlock()
	defer func() {
		zenModelsMu.Lock()
		delete(zenModels, "old-free")
		zenModelsMu.Unlock()
	}()
	m, ok := resolveZenModel("old-free")
	if !ok || m.ID != "old-free" {
		t.Fatalf("resolve = %v,%v; the real model must win over the alias", m, ok)
	}
}

// 死亡标记必须有 TTL：目录同步可能长期失败，只靠同步清空会让一个瞬时 400
// 把模型永久钉死。TTL 过期后标记自动失效（不依赖同步）。
func TestDeadMarkExpiresByTTL(t *testing.T) {
	setupZenDepTest(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	zenMarkModelDead("expiring-free")
	if !zenModelDead("expiring-free") {
		t.Fatal("freshly marked model must be dead")
	}
	// 把标记时间改到 TTL 之前（模拟时间流逝，无需 sleep）
	zenDepMu.Lock()
	zenDeadModels["expiring-free"] = time.Now().Add(-(zenDeadTTL + time.Minute)).Unix()
	zenDepMu.Unlock()
	if zenModelDead("expiring-free") {
		t.Fatal("dead mark must expire after the TTL even without a catalog sync")
	}
	// 过期即就地删除，不留残留
	zenDepMu.Lock()
	_, still := zenDeadModels["expiring-free"]
	zenDepMu.Unlock()
	if still {
		t.Fatal("expired dead mark must be pruned on read")
	}
}

// 已有 410 继任别名的 id 即使被 400 标记死亡，也应按别名放行（别名优先），
// 否则迁移后仍可用的请求会被一条陈旧标记挡住。
func TestAliasWinsOverDeadMark(t *testing.T) {
	setupZenDepTest(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	if zenRecordDeprecation("legacy-free", "successor-free") == "" {
		t.Fatal("record alias")
	}
	zenModelsMu.Lock()
	zenModels["successor-free"] = &ZenModel{ID: "successor-free", Source: "live"}
	zenModelsMu.Unlock()
	defer func() {
		zenModelsMu.Lock()
		delete(zenModels, "successor-free")
		zenModelsMu.Unlock()
	}()
	zenMarkModelDead("legacy-free")
	if got := routeModel("legacy-free"); got != "zen" {
		t.Fatalf("routeModel = %q, want zen (alias must win over a stale dead mark)", got)
	}
}

// ===== 测试辅助 =====

func setupZenDepTest(t *testing.T, upstream *httptest.Server) {
	t.Helper()
	t.Setenv("DATA_DIR", t.TempDir())
	savedCfg := getZenConfig()
	cfgCopy := *savedCfg
	cfgCopy.Enabled = true
	cfgCopy.BaseURL = upstream.URL
	cfgCopy.Keys = []string{"sk-dep-test"}
	cfgCopy.Proxies = nil
	cfgCopy.Retries = 0
	setZenConfig(&cfgCopy)
	t.Cleanup(func() { setZenConfig(savedCfg) })

	savedModels, savedAliases := zenModels, zenAliases
	zenModelsMu.Lock()
	zenModels = map[string]*ZenModel{
		"mimo-v2.5-free":       {ID: "mimo-v2.5-free", Source: "live"},
		"mimo-v2.6-flash-free": {ID: "mimo-v2.6-flash-free", Source: "live"},
	}
	zenAliases = map[string]*ZenModel{}
	zenModelsMu.Unlock()
	t.Cleanup(func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	})

	zenSessMu.Lock()
	zenSessions = map[string]*zenSessionEntry{}
	zenSessLoaded = true
	zenSessPath = ""
	zenSessSaveBlocked = false
	zenSessMu.Unlock()
	zenDepMu.Lock()
	zenDepAliases = map[string]*zenDepEntry{}
	zenDepLoaded = true
	zenDepPath = ""
	zenDeadModels = map[string]int64{}
	zenDepMu.Unlock()
	t.Cleanup(func() {
		zenDepMu.Lock()
		zenDepAliases = map[string]*zenDepEntry{}
		zenDeadModels = map[string]int64{}
		zenDepMu.Unlock()
	})
	zenSessFailedMu.Lock()
	zenSessFailed = map[string]int{}
	zenSessFailedMu.Unlock()
}

func readJSONBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Fatalf("decode upstream request body: %v", err)
	}
}

func markRegistryHealthyForTest(t *testing.T) {
	t.Helper()
	payload := `{"opencode":{"models":{"mimo-v2.5-free":{"id":"mimo-v2.5-free","cost":{"input":0,"output":0}}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)
	old := zenRegistryURL
	zenRegistryURL = srv.URL
	t.Cleanup(func() { zenRegistryURL = old })
}

func asZenHTTPError(err error, target **zenHTTPError) bool {
	if he, ok := err.(*zenHTTPError); ok {
		*target = he
		return true
	}
	return false
}

// 带前缀/大写变体的 ID 必须解析到同一模型：客户端发 "opencode/big-pickle" 或
// "Big-Pickle" 时，漏归一化会落空并误路由到 cline 池。
func TestResolveZenModelNormalizesID(t *testing.T) {
	zenModelsMu.Lock()
	savedModels, savedAliases := zenModels, zenAliases
	zenModels = map[string]*ZenModel{"big-pickle": {ID: "big-pickle", Source: "live"}}
	zenAliases = map[string]*ZenModel{}
	zenModelsMu.Unlock()
	defer func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	}()

	for _, in := range []string{"big-pickle", "Big-Pickle", "opencode/big-pickle", "opencode/BIG-PICKLE", "  Big-Pickle  "} {
		m, ok := resolveZenModel(in)
		if !ok || m.ID != "big-pickle" {
			t.Errorf("resolveZenModel(%q) = %v,%v; want big-pickle,true", in, m, ok)
		}
	}
	if got := routeModel("opencode/Big-Pickle"); got != "zen" {
		t.Errorf("routeModel(opencode/Big-Pickle) = %q, want zen", got)
	}
}

// dead 标记与 410 别名都以规范 ID 为键：带前缀/大写的查询必须命中。
func TestDeadMarkAndAliasNormalizeKeys(t *testing.T) {
	setupZenDepTest(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	zenMarkModelDead("opencode/Some-Model")
	if !zenModelDead("some-model") {
		t.Fatal("dead mark recorded with a prefixed/cased id must be findable via the canonical id")
	}
	if !zenModelDead("opencode/Some-Model") {
		t.Fatal("dead mark must also be findable via the original prefixed/cased id")
	}
	if repl := zenRecordDeprecation("OpenCode/Old-Free", "New-Free"); repl != "new-free" {
		t.Fatalf("record returned %q, want normalized new-free", repl)
	}
	if got := zenDeprecatedReplacement("old-free"); got != "new-free" {
		t.Fatalf("alias lookup = %q, want new-free", got)
	}
}
