package app

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"cline-go-proxy/internal/kit"
)

// withCachedModel 往模型缓存里注入一条测试模型并在测试结束后移除。
//
// 为什么不直接用种子表里的模型：种子表随官方 free feed 变化（2026-11 就整批
// 换过），而这些测试真正关心的是"命名约定判定漏判 → 学习 → 流式重试"这条
// 路径，需要一个不含 ":" 之外特征的 id。依赖线上列表会让测试莫名其妙地红。
func withCachedModel(t *testing.T, id string) {
	t.Helper()
	modelsMu.Lock()
	if modelsCache == nil {
		modelsCache = make(map[string]*ModelInfo)
	}
	saved, had := modelsCache[id]
	modelsCache[id] = &ModelInfo{ID: id, Source: "free", Provider: "probe", Cost: "free", Status: ModelActive}
	modelsMu.Unlock()
	t.Cleanup(func() {
		modelsMu.Lock()
		if had {
			modelsCache[id] = saved
		} else {
			delete(modelsCache, id)
		}
		modelsMu.Unlock()
	})
}

func fakeUpstream(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
}

// 假上游必须复刻 callClineAPI 的真实契约：非 200 时返回 nil 响应 + *clineAPIError
//（响应体在 callee 里已读尽并关闭）。早期版本返回 (500 响应, nil error) —— 那是
// 生产里不存在的形态，导致学习逻辑在真实上游面前永远不会触发。
func fakeUpstreamErr(status int, body string) (*http.Response, *Account, error) {
	return nil, &Account{Email: "err@x"}, &clineAPIError{Status: status, Body: body}
}

// 非流式请求撞上"500 empty response content"时：学习该模型 + 立刻改用流式重试，
// 并把 streamed=true 交回调用方（调用方据此走聚合路径）。这正是命名约定漏判时
// 用户唯一会碰到的失败形态，必须自动愈合。
func TestCallClineAutoStreamLearnsAndRetries(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	clineStreamMu.Lock()
	clineStreamLearned = nil
	clineStreamMu.Unlock()

	orig := clineCallFn
	defer func() { clineCallFn = orig }()

	var seenStream []bool
	clineCallFn = func(ctx context.Context, params map[string]any, stream, useProxies bool) (*http.Response, *Account, error) {
		seenStream = append(seenStream, stream)
		if !stream {
			return fakeUpstreamErr(500, `{"error":"empty response content","success":false}`)
		}
		return fakeUpstream(200, "data: {\"choices\":[]}\n\n"), &Account{Email: "second@x"}, nil
	}

	// 含 ":" 的 id 被约定判定"无需流式"，正是要学习的路径
	const probeModel = "probe/needs-stream:free"
	withCachedModel(t, probeModel)

	params := map[string]any{"model": probeModel}
	resp, acc, streamed, err := callClineAutoStream(context.Background(), params, false, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(seenStream) != 2 || seenStream[0] != false || seenStream[1] != true {
		t.Fatalf("upstream attempts = %v, want [false true]", seenStream)
	}
	if !streamed {
		t.Fatal("streamed must be true so the caller aggregates the SSE body")
	}
	if resp.StatusCode != 200 {
		t.Fatalf("retry response status = %d, want 200", resp.StatusCode)
	}
	if acc == nil || acc.Email != "second@x" {
		t.Fatalf("account of the retry must be returned, got %+v", acc)
	}
	if !clineStreamRequired(probeModel) {
		t.Fatal("model must be learned as stream-required")
	}
	if !modelNeedsStream(probeModel) {
		t.Fatal("learned model must be force-streamed on later requests")
	}
}

// 重试也失败时：交回第一次的错误（调用方照旧报上游错误），且不谎报 streamed；
// 模型仍被学习，因此后续请求不会再走这条非流式失败路径。
func TestCallClineAutoStreamRetryFailureKeepsOriginalError(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	clineStreamMu.Lock()
	clineStreamLearned = nil
	clineStreamMu.Unlock()

	orig := clineCallFn
	defer func() { clineCallFn = orig }()
	clineCallFn = func(ctx context.Context, params map[string]any, stream, useProxies bool) (*http.Response, *Account, error) {
		if !stream {
			return fakeUpstreamErr(500, `{"error":"empty response content"}`)
		}
		return nil, nil, context.DeadlineExceeded
	}

	const probeModel = "probe/retry-fail:free"
	withCachedModel(t, probeModel)
	params := map[string]any{"model": probeModel}
	resp, _, streamed, err := callClineAutoStream(context.Background(), params, false, false)
	if err == nil {
		t.Fatal("retry failure must surface the upstream error, got nil")
	}
	if streamed {
		t.Fatal("streamed must be false when the retry never produced a stream")
	}
	if resp != nil {
		t.Fatal("no response body exists for a non-200 upstream reply")
	}
	if !strings.Contains(err.Error(), "empty response content") {
		t.Fatalf("the first failure should be reported, got %v", err)
	}
	if !clineStreamRequired(probeModel) {
		t.Fatal("model must still be learned even when the retry failed")
	}
}

// 普通 500（过载/瞬时故障）不得被当作模型属性学习，也不得触发重试。
func TestCallClineAutoStreamIgnoresPlain500(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	clineStreamMu.Lock()
	clineStreamLearned = nil
	clineStreamMu.Unlock()

	orig := clineCallFn
	defer func() { clineCallFn = orig }()
	calls := 0
	clineCallFn = func(ctx context.Context, params map[string]any, stream, useProxies bool) (*http.Response, *Account, error) {
		calls++
		return fakeUpstreamErr(500, `{"error":"Internal server error"}`)
	}

	const probeModel = "probe/plain500:free"
	withCachedModel(t, probeModel)
	params := map[string]any{"model": probeModel}
	_, _, streamed, err := callClineAutoStream(context.Background(), params, false, false)
	if err == nil {
		t.Fatal("plain 500 must still be an error")
	}
	if streamed || calls != 1 {
		t.Fatalf("plain 500 must pass through untouched: streamed=%v calls=%d", streamed, calls)
	}
	if clineStreamRequired(probeModel) {
		t.Fatal("transient 500 must not be learned as a model property")
	}
}

// clineEmptyStreamErr 只认"500 + empty response content"这一条指纹：这是 cline
// 对非流式调用"必须流式"模型的固定答复。其他 5xx（过载、瞬时故障）绝不能触发
// 学习，否则会把普通故障误记成模型属性并持久化。
func TestClineEmptyStreamErrDetection(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"exact fingerprint", 500, `{"error":"empty response content","success":false}`, true},
		{"fingerprint upper case", 500, `{"error":"Empty Response Content"}`, true},
		{"500 other message", 500, `{"error":"Internal server error"}`, false},
		{"200 with that text", 200, `{"error":"empty response content"}`, false},
		{"502 transient", 502, `{"error":"empty response content"}`, false},
		{"503 overload", 503, `{"error":"empty response content"}`, false},
		{"empty body", 500, ``, false},
	}
	for _, c := range cases {
		if got := clineEmptyStreamErr(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s: clineEmptyStreamErr(%d, %q) = %v, want %v", c.name, c.status, c.body, got, c.want)
		}
	}
}

// 学习结果是持久化 + 幂等的：重复学习不覆盖首次时间戳，modelNeedsStream 命中
// 学习集合后无需同步层标记（即命名约定漏判的模型也能被强制流式）。
func TestClineStreamLearnRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	loadClineStreamLearned() // 空文件：不 panic、不写入
	if clineStreamRequired("poolside/laguna-s-2.1:free") {
		t.Fatal("clean state should not require stream")
	}

	learnClineStreamRequired("poolside/laguna-s-2.1:free")
	if !clineStreamRequired("poolside/laguna-s-2.1:free") {
		t.Fatal("learned model should require stream in-process")
	}
	if !modelNeedsStream("poolside/laguna-s-2.1:free") {
		t.Fatal("modelNeedsStream should consult the learned set")
	}
	// 命名约定已判定的模型不受影响，未学习的模型仍为 false
	if modelNeedsStream("line-free/solar-pro4") {
		t.Fatal("unrelated model must not be affected")
	}
	if _, err := os.Stat(kit.ResolveDataPath(".cline-stream-required.json")); err != nil {
		t.Fatalf("learned set not persisted: %v", err)
	}

	// 重复学习幂等
	clineStreamMu.RLock()
	first := clineStreamLearned["poolside/laguna-s-2.1:free"]
	clineStreamMu.RUnlock()
	learnClineStreamRequired("poolside/laguna-s-2.1:free")
	clineStreamMu.RLock()
	second := clineStreamLearned["poolside/laguna-s-2.1:free"]
	clineStreamMu.RUnlock()
	if first != second {
		t.Fatalf("re-learning overwrote timestamp: %q -> %q", first, second)
	}

	// 重启：从磁盘恢复
	clineStreamMu.Lock()
	clineStreamLearned = nil
	clineStreamMu.Unlock()
	loadClineStreamLearned()
	if !clineStreamRequired("poolside/laguna-s-2.1:free") {
		t.Fatal("learned model lost after reload")
	}
}

// 空 model id 不写盘（调用方在拿不到 model 字段时不应污染学习结果）。
func TestClineStreamLearnEmptyIDIgnored(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	loadClineStreamLearned()
	learnClineStreamRequired("")
	if _, err := os.Stat(kit.ResolveDataPath(".cline-stream-required.json")); err == nil {
		t.Fatal("empty id must not create a learn file")
	}
	if clineStreamRequired("") {
		t.Fatal("empty id must not be learned")
	}
}
