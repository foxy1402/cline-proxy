package app

import (
	"cline-go-proxy/internal/kit"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 这是 cline "必须流式"自学习的端到端回归测试：真实 callClineAPI（唯一的生产
// 调用路径）+ 本地假上游。
//
// 为什么必须有这一层：callClineAutoStream 的早先实现只在"callClineAPI 返回
// 500 响应"时才嗅探错误体，而 callClineAPI 对非 200 一律返回 nil 响应 +
// error（响应体已读尽关闭）。单测用一个假上游塞 (500 响应, nil error) —— 那是
// 生产里不可能出现的形态，于是测试全绿而功能在生产里永远不触发。
//
// 本测试固定住两件事：
//  1. callClineAPI 对非 200 返回 *clineAPIError（携带状态码 + body）；
//  2. callClineAutoStream 因此能真的学到该模型并改用 stream=true 重试。
func TestAutoStreamLearnThroughRealCallee(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	clineStreamMu.Lock()
	clineStreamLearned = nil
	clineStreamMu.Unlock()

	var sawStream atomic.Value
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		stream, _ := body["stream"].(bool)
		sawStream.Store(stream)
		if !stream {
			// cline 对"必须流式"模型的非流式调用固定答复
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"empty response content","success":false}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	// 假上游接管上游基址；APIToken 账号不需要刷新（ensureAccountToken 直接
	// 返回静态 key），因此整个真实调用链无需网络。
	origBase := clineAPIBase
	clineAPIBase = upstream.URL
	defer func() { clineAPIBase = origBase }()

	poolMu.Lock()
	savedPool := pool
	pool = &AccountPool{Accounts: []*Account{{
		AccountID: "probe-1", Email: "probe@test", APIToken: "sk-test-static-key", Status: "active",
	}}}
	poolMu.Unlock()
	defer func() {
		poolMu.Lock()
		pool = savedPool
		poolMu.Unlock()
	}()

	params := map[string]any{
		"model":    "poolside/laguna-s-2.1:free", // 含 ":" → 命名约定判定"无需流式"
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}

	// 前置条件：约定判定该模型不需要流式，否则测不到学习路径
	if modelNeedsStream("poolside/laguna-s-2.1:free") {
		t.Fatal("precondition failed: model is already force-streamed by the naming convention")
	}

	resp, _, streamed, err := callClineAutoStream(context.Background(), params, false, false)
	if err != nil {
		t.Fatalf("learn-and-retry should succeed, got err=%v", err)
	}
	if resp == nil {
		t.Fatal("no response returned")
	}
	defer resp.Body.Close()
	if !streamed {
		t.Fatal("streamed must be true so the caller aggregates the SSE body")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (non-stream probe + streamed retry)", got)
	}
	if s, _ := sawStream.Load().(bool); !s {
		t.Fatal("the retry must have used stream=true")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "\"ok\"") {
		t.Fatalf("retry body not returned: %s", body)
	}
	if !clineStreamRequired("poolside/laguna-s-2.1:free") {
		t.Fatal("model must be learned as stream-required")
	}
	// 后续请求直接命中，无需再付一次非流式探测的代价
	if !modelNeedsStream("poolside/laguna-s-2.1:free") {
		t.Fatal("learned model must be force-streamed on later requests")
	}
}

// callClineAPI 的非 200 契约：nil 响应 + 携带状态码的类型化错误。
// 端点学习/流式学习都依赖它；这条断言是那两个功能的立足点。
func TestCallClineAPINon200ReturnsTypedError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"bad gateway"}`))
	}))
	defer upstream.Close()

	origBase := clineAPIBase
	clineAPIBase = upstream.URL
	defer func() { clineAPIBase = origBase }()

	poolMu.Lock()
	savedPool := pool
	pool = &AccountPool{Accounts: []*Account{{
		AccountID: "probe-2", Email: "probe2@test", APIToken: "sk-test-static-key", Status: "active",
	}}}
	poolMu.Unlock()
	defer func() {
		poolMu.Lock()
		pool = savedPool
		poolMu.Unlock()
	}()

	resp, _, err := callClineAPI(context.Background(), map[string]any{
		"model":    "z-ai/glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, false, false)

	if resp != nil {
		t.Fatalf("non-200 must not hand back a response (body is consumed): %+v", resp)
	}
	if err == nil {
		t.Fatal("non-200 must be an error")
	}
	var apiErr *clineAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error must be *clineAPIError so callers can classify it by status, got %T", err)
	}
	if apiErr.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", apiErr.Status)
	}
	if !strings.Contains(apiErr.Body, "bad gateway") {
		t.Fatalf("body not preserved: %q", apiErr.Body)
	}
	// 502 是瞬时错误：绝不能触发端点学习
	if isWrongEndpoint(err) {
		t.Fatal("a 502 must never be classified as a wrong endpoint")
	}
}

// 跨账号 failover：一个账号撞 429 时换下一个健康账号重试，而不是把整请求判死。
// 假上游按 Authorization 里的 key 区分账号：key-A 恒 429，key-B 恒 200。
func TestCallClineAPIFailoverOn429(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "key-A") {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limit reached"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
	}))
	defer upstream.Close()

	origBase := clineAPIBase
	clineAPIBase = upstream.URL
	defer func() { clineAPIBase = origBase }()

	poolMu.Lock()
	savedPool := pool
	pool = &AccountPool{Accounts: []*Account{
		{AccountID: "a-1", Email: "a1@test", APIToken: "sk-key-A", Status: "active"},
		{AccountID: "a-2", Email: "a2@test", APIToken: "sk-key-B", Status: "active"},
	}}
	poolMu.Unlock()
	defer func() {
		poolMu.Lock()
		pool = savedPool
		poolMu.Unlock()
	}()

	resp, acc, err := callClineAPI(context.Background(), map[string]any{
		"model":    "z-ai/glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, false, false)
	if err != nil {
		t.Fatalf("failover should land on the healthy account, got err=%v", err)
	}
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("expected 200 from the healthy account, got %+v", resp)
	}
	defer resp.Body.Close()
	if acc == nil || acc.AccountID != "a-2" {
		t.Fatalf("returned account = %+v, want the healthy a-2", acc)
	}

	// 429 的那个账号必须已被冷却（换账号的前提就是它被移出 active 集合）
	poolMu.Lock()
	var a1 *Account
	for _, a := range pool.Accounts {
		if a.AccountID == "a-1" {
			a1 = a
		}
	}
	poolMu.Unlock()
	if a1 == nil || a1.Status != "cooldown" {
		t.Fatalf("the 429 account must be cooled, got %+v", a1)
	}
}

// 429 且错误体没有任何可解析时长时：绝不落到 markAccountCooldown 的 18h 兜底，
// 除非错误体含明确配额措辞。此测断言"无配额措辞 → 短冷却"。
func TestCallClineAPI429UnparseableShortCooldown(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`)) // 无 Try again in / quota 措辞
	}))
	defer upstream.Close()

	origBase := clineAPIBase
	clineAPIBase = upstream.URL
	defer func() { clineAPIBase = origBase }()

	poolMu.Lock()
	savedPool := pool
	pool = &AccountPool{Accounts: []*Account{
		{AccountID: "only", Email: "only@test", APIToken: "sk-only", Status: "active"},
	}}
	poolMu.Unlock()
	defer func() {
		poolMu.Lock()
		pool = savedPool
		poolMu.Unlock()
	}()

	_, _, err := callClineAPI(context.Background(), map[string]any{
		"model":    "z-ai/glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, false, false)
	if err == nil {
		t.Fatal("429 must still surface as an error when no healthy account remains")
	}

	poolMu.Lock()
	acc := pool.Accounts[0]
	until := acc.CooldownUntil
	poolMu.Unlock()
	if until.IsZero() {
		t.Fatal("the 429 account must be put on cooldown")
	}
	if d := time.Until(until); d > 2*time.Hour {
		t.Fatalf("unparseable 429 must get a short cooldown, got %v", d)
	}
}

// 429 且错误体含明确配额措辞（quota exceeded）时：给足 18h，避免健康账号被
// 一分钟一次地反复打回上游。
func TestCallClineAPI429QuotaKeywordLongCooldown(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"daily quota exceeded, upgrade your plan"}`))
	}))
	defer upstream.Close()

	origBase := clineAPIBase
	clineAPIBase = upstream.URL
	defer func() { clineAPIBase = origBase }()

	poolMu.Lock()
	savedPool := pool
	pool = &AccountPool{Accounts: []*Account{
		{AccountID: "quota", Email: "quota@test", APIToken: "sk-quota", Status: "active"},
	}}
	poolMu.Unlock()
	defer func() {
		poolMu.Lock()
		pool = savedPool
		poolMu.Unlock()
	}()

	_, _, err := callClineAPI(context.Background(), map[string]any{
		"model":    "z-ai/glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, false, false)
	if err == nil {
		t.Fatal("quota-exhausted 429 must surface as an error")
	}

	poolMu.Lock()
	until := pool.Accounts[0].CooldownUntil
	poolMu.Unlock()
	if d := time.Until(until); d < 17*time.Hour {
		t.Fatalf("quota-exhausted 429 must get a long cooldown, got %v", d)
	}
}
// clineHeaders 契约：官方 CLI 身份头必须齐全、大小写精确、取值与最新 CLI
// 一致（audit 2026-10-10，核对自 sdk request-headers.ts / cline-client-headers.ts：
// CLI 3.0.70，X-CLIENT-TYPE cline-cli，core 0.0.92，platform cli）。Cline 网关对
// 受限免费模型校验这些头，缺失/错值可能 403。
func TestClineHeadersContract(t *testing.T) {
	h := clineHeaders("tok-123", "sess_abc")

	// 全量取值对照 audit 表格；用 raw 键读取（Get 会规范化，读不到大写变体）
	want := map[string]string{
		"Authorization":      "Bearer tok-123",
		"Content-Type":       "application/json",
		"User-Agent":         "Cline/3.0.70",
		"HTTP-Referer":       "https://cline.bot",
		"X-Title":            "Cline",
		"X-IS-MULTIROOT":     "false",
		"X-CLIENT-TYPE":      "cline-cli",
		"X-CLIENT-VERSION":   "3.0.70",
		"X-PLATFORM":         "cli",
		"X-PLATFORM-VERSION": "3.0.70",
		"X-CORE-VERSION":     "0.0.92",
		"X-Task-ID":          "sess_abc",
	}
	for k, v := range want {
		vals, ok := h[k]
		if !ok {
			t.Errorf("header %q missing (raw key absent; check Header.Set canonicalization)", k)
			continue
		}
		if len(vals) != 1 || vals[0] != v {
			t.Errorf("header %q = %v, want [%q]", k, vals, v)
		}
	}
	// 常量与 audit 表格字面一致（防止只改常量不改断言时静默漂移）
	if clineClientVersion != "3.0.70" || clineCoreVersion != "0.0.92" {
		t.Errorf("version constants drifted from the audit spec: client=%q core=%q", clineClientVersion, clineCoreVersion)
	}
	// 空 session 不得带 X-Task-ID
	if _, ok := clineHeaders("tok", "")["X-Task-ID"]; ok {
		t.Error("X-Task-ID must be omitted when there is no session id")
	}

	// 不得出现大小写变体重复键：Go 的 http.Header 是 map[大小写敏感 key]，
	// 同名字段出现两个变体时网络层会发出重复头（X-CLIENT-VERSION 与
	// X-Client-Version 并存）。默认 config 的 Set 路径与 raw 赋值混用会触发。
	seen := map[string]bool{}
	for k := range h {
		c := textproto.CanonicalMIMEHeaderKey(k)
		if seen[c] {
			t.Errorf("duplicate header (case variants): %q collides with an existing canonical %q", k, c)
		}
		seen[c] = true
	}
}

// zen 上游身份头与 UA 必须与 audit 规格一致：UA 为
// "opencode/<ver> ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"（ver 随最新
// CLI，2026-10-10 为 1.18.35）；gate 头用官方的小写键名。
func TestZenHeadersMatchAuditSpec(t *testing.T) {
	if zenNativeUA != "opencode/1.18.35 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14" {
		t.Errorf("zenNativeUA drifted from the audit spec: %q", zenNativeUA)
	}
	// 轮换列表里的原生 UA 条目也必须是同一版本
	found := false
	for _, ua := range kit.ZenUserAgents {
		if ua == zenNativeUA {
			found = true
		}
	}
	if !found {
		t.Errorf("ZenUserAgents must contain the native ai-sdk UA %q (got %v)", zenNativeUA, kit.ZenUserAgents)
	}

	req, err := http.NewRequest("POST", "https://opencode.ai/zen/v1/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	setZenGateHeaders(req, "ses_abcdef012345ABCDef012345", "msg_abcdefghijklmnopqrstuvwxyz12")
	// 官方 CLI 发小写键名；raw map 必须是小写条目
	for k, v := range map[string]string{
		"x-opencode-session-id": "ses_abcdef012345ABCDef012345",
		"x-opencode-session":    "ses_abcdef012345ABCDef012345",
		"x-opencode-request":    "msg_abcdefghijklmnopqrstuvwxyz12",
		"x-opencode-client":     "cli",
		"x-opencode-project":    "global",
	} {
		vals, ok := req.Header[k]
		if !ok {
			t.Errorf("zen gate header %q missing (must be lowercase raw key)", k)
			continue
		}
		if len(vals) != 1 || vals[0] != v {
			t.Errorf("zen gate header %q = %v, want [%q]", k, vals, v)
		}
	}
}

// 线级验证：真实 callClineAPI 打到假上游时，请求头必须与 audit 表格逐字一致
// （大小写敏感）。这是"我们确实按最新 CLI 指纹发包"的端到端证据，
// 单测 header map 只能证明构造正确，这里证明它真的上了线。
func TestClineHeadersOnTheWire(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())

	got := make(chan http.Header, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
	}))
	defer upstream.Close()

	origBase := clineAPIBase
	clineAPIBase = upstream.URL
	defer func() { clineAPIBase = origBase }()

	poolMu.Lock()
	savedPool := pool
	pool = &AccountPool{Accounts: []*Account{{
		AccountID: "hdr-1", Email: "hdr@test", APIToken: "sk-static-key", Status: "active",
	}}}
	poolMu.Unlock()
	defer func() {
		poolMu.Lock()
		pool = savedPool
		poolMu.Unlock()
	}()

	resp, _, err := callClineAPI(context.Background(), map[string]any{
		"model":    "cline-free/muse-spark-1.3-contributor",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, false, false)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	defer resp.Body.Close()

	h := <-got
	// net/http canonicalizes the wire header names on receipt, so read via Get.
	clientVer := "3.0.70"
	want := map[string]string{
		"User-Agent":         "Cline/" + clientVer,
		"Http-Referer":       "https://cline.bot", // Go stores the canonical form of HTTP-Referer
		"X-Title":            "Cline",
		"X-Is-Multiroot":     "false",
		"X-Client-Type":      "cline-cli",
		"X-Client-Version":   clientVer,
		"X-Platform":         "cli",
		"X-Platform-Version": clientVer,
		"X-Core-Version":     "0.0.92",
		"Content-Type":       "application/json",
	}
	for k, v := range want {
		if g := h.Get(k); g != v {
			t.Errorf("wire header %s = %q, want %q", k, g, v)
		}
	}
	if got := h.Get("Authorization"); got != "Bearer sk-static-key" {
		t.Errorf("Authorization = %q", got)
	}
	if got := h.Get("X-Task-Id"); got == "" {
		t.Error("X-Task-ID must be present on a chat request")
	}
}
