package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
	zenSessMu.Unlock()
	zenDepMu.Lock()
	zenDepAliases = map[string]*zenDepEntry{}
	zenDepLoaded = true
	zenDepPath = ""
	zenDeadModels = map[string]bool{}
	zenDepMu.Unlock()
	t.Cleanup(func() {
		zenDepMu.Lock()
		zenDepAliases = map[string]*zenDepEntry{}
		zenDeadModels = map[string]bool{}
		zenDepMu.Unlock()
	})
	zenRecoverMu.Lock()
	zenRecoverFails = map[string]int{}
	zenRecoverLastAt = map[string]int64{}
	zenRecoverMu.Unlock()
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
