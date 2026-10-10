package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fetchZenRegistry 的目录解码 + npm 解析优先级（官方 CLI fromModelsDevModel：
// 模型 provider.npm > 模型顶层 npm > 提供方 npm > 默认 openai-compatible）。
func TestFetchZenRegistryNPMPrecedence(t *testing.T) {
	payload := `{
  "opencode": {
    "npm": "@ai-sdk/openai-compatible",
    "models": {
      "m-per-model":     {"id": "m-per-model",     "cost": {"input": 0, "output": 0}, "provider": {"npm": "@ai-sdk/openai"}},
      "m-top-level":     {"id": "m-top-level",     "cost": {"input": 0, "output": 0}, "npm": "@ai-sdk/openai"},
      "m-provider-level":{"id": "m-provider-level","cost": {"input": 0, "output": 0}},
      "m-empty-npm":     {"id": "m-empty-npm",     "cost": {"input": 0, "output": 0}, "npm": "", "provider": {"npm": ""}}
    }
  }
}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	old := zenRegistryURL
	zenRegistryURL = srv.URL
	defer func() { zenRegistryURL = old }()

	overlay, freeGate, ok := fetchZenRegistry()
	if !ok {
		t.Fatal("registry unreachable")
	}
	if !freeGate["m-per-model"] || !freeGate["m-top-level"] || !freeGate["m-provider-level"] || !freeGate["m-empty-npm"] {
		t.Fatalf("free gate broken: %v", freeGate)
	}
	cases := map[string]string{
		"m-per-model":      "@ai-sdk/openai",            // 模型 provider.npm 最优先
		"m-top-level":      "@ai-sdk/openai",            // 模型顶层 npm 次之
		"m-provider-level": "@ai-sdk/openai-compatible", // 提供方 npm 兜底
		"m-empty-npm":      "@ai-sdk/openai-compatible", // 全空走默认
	}
	for id, want := range cases {
		if got := overlay[id].NPM; got != want {
			t.Errorf("%s npm = %q, want %q", id, got, want)
		}
	}
}

// 价格门回归：deprecated 即便 cost 0/0 也被排除；付费（cost>0）被排除。
func TestFetchZenRegistryFreeGate(t *testing.T) {
	payload := `{
  "opencode": {"models": {
    "free-ok":        {"id": "free-ok",        "cost": {"input": 0, "output": 0}},
    "deprecated":     {"id": "deprecated",     "cost": {"input": 0, "output": 0}, "status": "deprecated"},
    "paid":           {"id": "paid",           "cost": {"input": 1, "output": 2}},
    "alpha":          {"id": "alpha",          "cost": {"input": 0, "output": 0}, "status": "alpha"}
  }}
}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	old := zenRegistryURL
	zenRegistryURL = srv.URL
	defer func() { zenRegistryURL = old }()

	_, freeGate, ok := fetchZenRegistry()
	if !ok {
		t.Fatal("registry unreachable")
	}
	if !freeGate["free-ok"] {
		t.Error("free-ok must pass the gate")
	}
	if freeGate["deprecated"] {
		t.Error("deprecated must not pass the gate")
	}
	if freeGate["paid"] {
		t.Error("paid must not pass the gate")
	}
	// alpha 未 deprecated：与现有价格门一致放行（CLI 的 alpha 过滤属于展示层，
	// 本网关从未实现 alpha 过滤，行为不变）
	if !freeGate["alpha"] {
		t.Error("alpha must pass the existing gate (unchanged behavior)")
	}
}

// zenNPMToUpstream 的映射表：已知→路由，原生 npm→未知（学习器决定）。
func TestZenNPMToUpstream(t *testing.T) {
	if up, known := zenNPMToUpstream("@ai-sdk/openai"); !known || up != "responses" {
		t.Errorf("openai npm: up=%q known=%v, want responses/true", up, known)
	}
	if up, known := zenNPMToUpstream("@ai-sdk/openai-compatible"); !known || up != "" {
		t.Errorf("compatible npm: up=%q known=%v, want empty/true", up, known)
	}
	for _, npm := range []string{"@ai-sdk/anthropic", "@ai-sdk/google", "@ai-sdk/bogus", ""} {
		if up, known := zenNPMToUpstream(npm); known {
			t.Errorf("npm %q: up=%q known=%v, want known=false (learner decides)", npm, up, known)
		}
	}
}

// 端点种子端到端：新条目按 npm 播种 responses/chat；已有条目不被回翻。
func TestApplyZenCatalogSeedsUpstreamFromNPM(t *testing.T) {
	zenModelsMu.Lock()
	savedModels, savedAliases := zenModels, zenAliases
	zenModels = map[string]*ZenModel{}
	zenAliases = map[string]*ZenModel{}
	zenModelsMu.Unlock()
	defer func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	}()

	// 预置一个 chat 模型且 Upstream==""（npm 应把它播成 responses），
	// 和一个已被学习器定为 responses 的模型（npm=compatible 不得回翻）。
	zenModelsMu.Lock()
	zenModels["existing-chat"] = &ZenModel{ID: "existing-chat", Source: "seed", Upstream: ""}
	zenModels["learned-responses"] = &ZenModel{ID: "learned-responses", Source: "seed", Upstream: "responses"}
	zenModelsMu.Unlock()

	desired := map[string]bool{"existing-chat": true, "learned-responses": true, "fresh-responses": true, "fresh-chat": true}
	overlay := map[string]zenModelOverlay{
		"existing-chat":     {NPM: "@ai-sdk/openai"},
		"learned-responses": {NPM: "@ai-sdk/openai-compatible"},
		"fresh-responses":   {NPM: "@ai-sdk/openai"},
		"fresh-chat":        {NPM: "@ai-sdk/openai-compatible"},
	}
	added := applyZenCatalog(desired, overlay)
	if added != 2 {
		t.Fatalf("added = %d, want 2 (the two fresh entries)", added)
	}

	zenModelsMu.RLock()
	defer zenModelsMu.RUnlock()
	if got := zenModels["existing-chat"].Upstream; got != "responses" {
		t.Errorf("existing-chat Upstream = %q, want responses (re-seeded from npm)", got)
	}
	if got := zenModels["learned-responses"].Upstream; got != "responses" {
		t.Errorf("learned-responses Upstream = %q, want responses (must NOT be overwritten)", got)
	}
	if got := zenModels["fresh-responses"].Upstream; got != "responses" {
		t.Errorf("fresh-responses Upstream = %q, want responses (npm seed)", got)
	}
	if got := zenModels["fresh-chat"].Upstream; got != "" {
		t.Errorf("fresh-chat Upstream = %q, want empty (default chat)", got)
	}
	// 种子的 muse-spark 提示（Upstream=responses + npm 缺失）不被 npm 重播种破坏
	if m := zenSeedModels[5]; m.ID != "muse-spark-1.3-contributor-free" || m.Upstream != "responses" {
		t.Errorf("muse-spark seed entry changed unexpectedly: %+v", m)
	}
}

// 学习器改过端点的条目（Source=learned）必须仍是免费模型：big-pickle 这类
// 无 -free 后缀的模型全靠 Source 入选，抹成 learned 后不能掉出免费表。
func TestLearnedSourceStaysFree(t *testing.T) {
	for _, src := range []string{"seed", "live", "learned"} {
		m := &ZenModel{ID: "big-pickle", Source: src}
		if !isZenFreeModel(m) {
			t.Errorf("Source=%q must be treated as free (no -free suffix)", src)
		}
	}
}

// applyZenCatalog 不得抹掉学习的 Source 标记：它是"npm 不再重播种"的契约，
// 也是 saveZenEndpoints 写盘时的筛选条件。保留标记后，即便 reapply 尚未运行，
// 学习决策也已在表里生效。
func TestApplyZenCatalogPreservesLearnedMarker(t *testing.T) {
	zenModelsMu.Lock()
	savedModels, savedAliases := zenModels, zenAliases
	zenModels = map[string]*ZenModel{
		"learned-chat": {ID: "learned-chat", Source: "learned", Upstream: ""},
	}
	zenAliases = map[string]*ZenModel{}
	zenModelsMu.Unlock()
	defer func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	}()

	// 目录说该模型是 @ai-sdk/openai（会播成 responses）——学习器的 chat 定论
	// 必须压住它，且 Source 标记要留下来。
	desired := map[string]bool{"learned-chat": true}
	overlay := map[string]zenModelOverlay{"learned-chat": {NPM: "@ai-sdk/openai"}}
	applyZenCatalog(desired, overlay)

	zenModelsMu.RLock()
	m := zenModels["learned-chat"]
	zenModelsMu.RUnlock()
	if m.Upstream != "" {
		t.Errorf("learned chat decision clobbered to %q by catalog npm seeding", m.Upstream)
	}
	if m.Source != "learned" {
		t.Errorf("Source = %q, want %q (marker must survive applyZenCatalog)", m.Source, "learned")
	}
	if !isZenFreeModel(m) {
		t.Error("learned-marker entry must remain a free model")
	}
}

// cost 键缺失必须 fail-closed（不入免费池）：目录漏填价格字段的模型若被当作
// 免费，网关会把它路由到 zen 而用户实际要付费/被拒。
func TestFetchZenRegistryMissingCostFailsClosed(t *testing.T) {
	payload := `{
  "opencode": {"models": {
    "no-cost":    {"id": "no-cost"},
    "has-cost":   {"id": "has-cost", "cost": {"input": 0, "output": 0}}
  }}
}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	old := zenRegistryURL
	zenRegistryURL = srv.URL
	defer func() { zenRegistryURL = old }()

	_, freeGate, ok := fetchZenRegistry()
	if !ok {
		t.Fatal("registry unreachable")
	}
	if freeGate["no-cost"] {
		t.Error("a model with no cost key must NOT pass the free gate (fail-closed)")
	}
	if !freeGate["has-cost"] {
		t.Error("has-cost (0/0) must still pass the gate")
	}
}

// 目录响应超过体积上限时按不可达处理，不把超大响应读进内存。
func TestFetchZenRegistryBodyCap(t *testing.T) {
	big := strings.Repeat("x", maxRegistryBytes+1024)
	payload := `{"opencode":{"models":{"m":{"id":"m","cost":{"input":0,"output":0},"pad":"` + big + `"}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	old := zenRegistryURL
	zenRegistryURL = srv.URL
	defer func() { zenRegistryURL = old }()

	if _, _, ok := fetchZenRegistry(); ok {
		t.Fatal("an over-cap registry body must be treated as unreachable")
	}
}
