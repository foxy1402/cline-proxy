package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// 端点学习必须"重试成功才落盘"。
//
// 裸 500 既是"走错端点"的信号，也是上游瞬时故障的形态（实测 muse-spark 在
// 正确的 responses 端点上也会偶发 {"error":"Internal server error"}）。旧实现
// 一见 500 就 learnZenEndpoint 并持久化：瞬时故障会把 chat 原生模型永久翻转到
// responses —— 目录同步不改写既有条目的 Upstream，只能人工删文件恢复。
func TestEndpointLearnOnlyPersistsWhenRetrySucceeds(t *testing.T) {
	// 不用 t.TempDir()：统计写入会一直占着 $DATA_DIR/zen-stats.jsonl，Windows 上
	// TempDir 的自动清理会因句柄未释放而失败（与测试断言无关的假失败）。
	dir, err := os.MkdirTemp("", "zenlearn")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	// 假 zen 上游：/chat/completions 一律 500；/responses 由开关控制成败
	var responsesOK atomic.Bool
	var responsesHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"error","message":"Internal server error"}}`))
		case "/responses":
			atomic.AddInt32(&responsesHits, 1)
			if !responsesOK.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"error","message":"Internal server error"}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}` + "\n\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	savedCfg := getZenConfig()
	cfg := savedCfg
	cfg.BaseURL = upstream.URL
	cfg.Keys = []string{"probe-zen-key"}
	cfg.Proxies = nil
	cfg.Enabled = true
	cfg.Retries = 0
	setZenConfig(cfg)
	defer setZenConfig(savedCfg)

	model := "audit-probe-chat-native"
	// 只注册这一个模型，避免污染其他测试的模型表
	savedModels := zenModels
	savedAliases := zenAliases
	setZenModelForTest(model, "")
	defer func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	}()

	params := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   false,
	}
	zm, ok := resolveZenModel(model)
	if !ok {
		t.Fatal("test model not registered")
	}

	// 情形 1：responses 也失败 → 绝不能学习（这是瞬时故障，不是端点信号）
	responsesOK.Store(false)
	rec := &flushRecorder{header: http.Header{}}
	if handled := handleZenResponsesNative(rec, httptest.NewRequest("POST", "/v1/chat/completions", nil), params, zm, newZenStatsTracker(zenStatsRecord{})); handled {
		t.Fatal("a failed retry must not be reported as handled")
	}
	// 直接验证"chat 500 + responses 500"这一轮的完整路径
	rec2 := &flushRecorder{header: http.Header{}}
	handleZenChat(rec2, httptest.NewRequest("POST", "/v1/chat/completions", nil), params)
	if got := currentZenUpstream(model); got == "responses" {
		t.Fatalf("transient 500 must not flip the model to responses (upstream=%q)", got)
	}
	if learned := loadZenEndpointsFile(); learned[model] != "" {
		t.Fatalf("transient 500 must not be persisted: %v", learned)
	}
	firstHits := atomic.LoadInt32(&responsesHits)
	if firstHits == 0 {
		t.Fatal("the retry should still have been attempted")
	}

	// 情形 2：responses 真的能服务 → 这时才学习并持久化
	responsesOK.Store(true)
	rec3 := &flushRecorder{header: http.Header{}}
	handleZenChat(rec3, httptest.NewRequest("POST", "/v1/chat/completions", nil), params)
	if got := currentZenUpstream(model); got != "responses" {
		t.Fatalf("a confirmed working responses endpoint must be learned (upstream=%q)", got)
	}
	if learned := loadZenEndpointsFile(); learned[model] != "responses" {
		t.Fatalf("confirmed endpoint not persisted: %v", learned)
	}
}

// setZenModelForTest 在测试里挂一个独立的模型表。
func setZenModelForTest(id, upstream string) {
	zenModelsMu.Lock()
	zenModels = map[string]*ZenModel{id: {ID: id, Context: 200000, Output: 32768, Source: "live", Upstream: upstream}}
	zenAliases = map[string]*ZenModel{}
	zenModelsMu.Unlock()
}

// currentZenUpstream 测试辅助：读模型当前生效的端点。
func currentZenUpstream(id string) string {
	zenModelsMu.RLock()
	defer zenModelsMu.RUnlock()
	if m, ok := zenModels[id]; ok && m != nil {
		return m.Upstream
	}
	return ""
}

// 回归：/v1/responses 入口对"原生 responses 端点"模型（muse-spark 类）必须走
// callZenResponsesAPI，而不是无条件走 chat 端点——否则上游 400
// ModelProtocolUnsupported，该模型在 /v1/responses 上 100% 失败（实测）。
func TestResponsesEntryUsesNativeEndpointForResponsesModel(t *testing.T) {
	dir, err := os.MkdirTemp("", "zenrespnative")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	var chatHits, respHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			atomic.AddInt32(&chatHits, 1)
			// 原生模型走 chat 端点时的真实答复
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`))
		case "/responses":
			atomic.AddInt32(&respHits, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"OK"}` + "\n\n"))
			_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}` + "\n\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	savedCfg := getZenConfig()
	cfg := savedCfg
	cfg.BaseURL = upstream.URL
	cfg.Keys = []string{"probe-zen-key"}
	cfg.Proxies = nil
	cfg.Enabled = true
	cfg.Retries = 0
	setZenConfig(cfg)
	defer setZenConfig(savedCfg)

	model := "native-resp-model"
	savedModels, savedAliases := zenModels, zenAliases
	setZenModelForTest(model, "responses")
	defer func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	}()

	body := `{"model":"` + model + `","input":"say OK","stream":false}`
	rec := &flushRecorder{header: http.Header{}}
	handleResponses(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))

	if rec.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.status, rec.body.String())
	}
	if atomic.LoadInt32(&chatHits) != 0 {
		t.Fatalf("native responses model must not hit the chat endpoint (chat hits = %d)", chatHits)
	}
	if atomic.LoadInt32(&respHits) == 0 {
		t.Fatal("native responses model must hit the /responses endpoint")
	}
	if !strings.Contains(rec.body.String(), "OK") {
		t.Fatalf("response text missing from body: %s", rec.body.String())
	}
}

// 线级验证：真实 callZenAPI 打到假上游时，gate 头必须是官方的小写键名
// （x-opencode-*），UA 为最新 CLI 的 ai-sdk 形态。证明 zen 指纹真的上了线。
func TestZenHeadersOnTheWire(t *testing.T) {
	dir, err := os.MkdirTemp("", "zenhdr")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dir)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	got := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	savedCfg := getZenConfig()
	cfg := savedCfg
	cfg.BaseURL = upstream.URL
	cfg.Keys = []string{"wire-zen-key"}
	cfg.Proxies = nil
	cfg.Enabled = true
	cfg.Retries = 0
	setZenConfig(cfg)
	defer setZenConfig(savedCfg)

	savedModels, savedAliases := zenModels, zenAliases
	setZenModelForTest("wire-zen-model", "") // chat endpoint
	defer func() {
		zenModelsMu.Lock()
		zenModels, zenAliases = savedModels, savedAliases
		zenModelsMu.Unlock()
	}()

	_, _, err = callZenAPI(t.Context(), map[string]any{
		"model":    "wire-zen-model",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream":   true,
	}, true)
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	h := <-got
	want := map[string]string{
		"X-Opencode-Client":  "cli",
		"X-Opencode-Project": "global",
		"User-Agent":         "opencode/1.18.35 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14",
	}
	for k, v := range want {
		if g := h.Get(k); g != v {
			t.Errorf("wire header %s = %q, want %q", k, g, v)
		}
	}
	if h.Get("X-Opencode-Session-Id") == "" || h.Get("X-Opencode-Session") == "" {
		t.Error("both x-opencode-session-id and x-opencode-session must be sent")
	}
	if h.Get("X-Opencode-Request") == "" {
		t.Error("x-opencode-request must be sent")
	}
	if got := h.Get("Authorization"); got != "Bearer wire-zen-key" {
		t.Errorf("Authorization = %q", got)
	}
}
