package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 2026-10-30 抓包对齐：CLI 2.1.4 在 chat 路径恒发 stream_options.include_usage，
// 且 max_completion_tokens 取目录的 limit.output。两者都是上游可见的形态差异。
func TestBuildZenBodyStreamOptions(t *testing.T) {
	body := buildZenBody(map[string]any{"model": "big-pickle", "messages": []any{}}, true)
	so, ok := body["stream_options"].(map[string]any)
	if !ok {
		t.Fatal("streaming body must carry stream_options")
	}
	if v, _ := so["include_usage"].(bool); !v {
		t.Error("stream_options.include_usage must be true")
	}

	// 非流式：上游恒 stream=true（FreeTier gate），但 buildZenBody 的 stream
	// 参数为 false 时不加该字段——避免给非流式路径留下无意义的键。
	body = buildZenBody(map[string]any{"model": "big-pickle", "messages": []any{}}, false)
	if _, present := body["stream_options"]; present {
		t.Error("non-streaming body must not carry stream_options")
	}

	// 非流式路径即使客户端传了 stream_options（passThroughKeys 会搬过来），
	// 也必须被删掉：它是流式专有字段。
	body = buildZenBody(map[string]any{
		"model": "big-pickle", "messages": []any{},
		"stream_options": map[string]any{"include_usage": true},
	}, false)
	if _, present := body["stream_options"]; present {
		t.Error("non-streaming body must drop a client-supplied stream_options")
	}

	// 客户端带的其它 stream_options 子键要保留，不能被网关整体替换掉。
	body = buildZenBody(map[string]any{
		"model": "big-pickle", "messages": []any{},
		"stream_options": map[string]any{"some_future_key": "keep-me", "include_usage": false},
	}, true)
	so, _ = body["stream_options"].(map[string]any)
	if _, keep := so["some_future_key"]; !keep {
		t.Error("unknown client stream_options keys must survive the merge")
	}
	if v, _ := so["include_usage"].(bool); !v {
		t.Error("gateway needs include_usage=true upstream even if the client said false")
	}
}

// max_completion_tokens 默认取目录 limit.output；客户端显式给了就用客户端的。
func TestBuildZenBodyDefaultMaxCompletionTokens(t *testing.T) {
	initZenModels()
	m, ok := resolveZenModel("big-pickle")
	if !ok || m.Output <= 0 {
		t.Skip("big-pickle seed missing")
	}

	body := buildZenBody(map[string]any{"model": "big-pickle", "messages": []any{}}, true)
	got, _ := body["max_completion_tokens"].(float64) // int 经 map[string]any 保持 int
	if v, isInt := body["max_completion_tokens"].(int); isInt {
		got = float64(v)
	}
	if int(got) != m.Output {
		t.Errorf("max_completion_tokens = %v, want catalog output %d", body["max_completion_tokens"], m.Output)
	}

	// 客户端显式指定 → 不被覆盖
	body = buildZenBody(map[string]any{
		"model": "big-pickle", "messages": []any{},
		"max_completion_tokens": 1234,
	}, true)
	if v, _ := body["max_completion_tokens"].(int); v != 1234 {
		t.Errorf("client max_completion_tokens overwritten: %v", body["max_completion_tokens"])
	}

	// 客户端用旧名 max_tokens → 不额外塞 max_completion_tokens（两个都发会自相矛盾）
	body = buildZenBody(map[string]any{
		"model": "big-pickle", "messages": []any{}, "max_tokens": 999,
	}, true)
	if _, has := body["max_completion_tokens"]; has {
		t.Errorf("max_tokens present, must not also send max_completion_tokens: %v", body["max_completion_tokens"])
	}
}

// gate 工具在 chat 路径也要带 strict=false（CLI 抓包如此，responses 路径一直有）。
func TestZenGateToolsChatStrictField(t *testing.T) {
	tools := zenGateTools()
	if len(tools) == 0 {
		t.Fatal("no gate tools")
	}
	for _, raw := range tools {
		tm, _ := raw.(map[string]any)
		fn, _ := tm["function"].(map[string]any)
		if fn == nil {
			t.Fatalf("gate tool without function object: %v", raw)
		}
		v, present := fn["strict"]
		if !present {
			t.Errorf("gate tool %v missing strict", fn["name"])
			continue
		}
		if b, _ := v.(bool); b {
			t.Errorf("gate tool %v strict must be false", fn["name"])
		}
	}
}

// 上游按 include_usage 补的纯 usage 帧：客户端没要就不下发，要了就透传。
func TestUsageOnlyChunkSuppressedUnlessClientAsked(t *testing.T) {
	sse := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"

	run := func(keep bool) string {
		up := &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(sse)),
			Header:     make(http.Header),
		}
		rec := httptest.NewRecorder()
		handleStreamResponseUsage(rec, up, func(map[string]any) {}, keep)
		return rec.Body.String()
	}

	if out := run(false); strings.Contains(out, "\"usage\"") {
		t.Errorf("client did not ask for usage; the usage-only frame must be dropped, got:\n%s", out)
	}
	// 正文内容无论如何都要到达客户端
	if out := run(false); !strings.Contains(out, "\"hi\"") {
		t.Errorf("content chunk lost:\n%s", out)
	}
	if out := run(true); !strings.Contains(out, "\"usage\"") {
		t.Errorf("client asked for usage; frame must be forwarded, got:\n%s", out)
	}
}

func TestClientWantsStreamUsage(t *testing.T) {
	if clientWantsStreamUsage(map[string]any{}) {
		t.Error("empty params must not want usage")
	}
	if !clientWantsStreamUsage(map[string]any{"stream_options": map[string]any{"include_usage": true}}) {
		t.Error("explicit include_usage=true must want usage")
	}
	if clientWantsStreamUsage(map[string]any{"stream_options": map[string]any{"include_usage": false}}) {
		t.Error("include_usage=false must not want usage")
	}
	// JSON 解码后 stream_options 可能是 map[string]interface{} 的别名形态；
	// 非 map 值一律按"没要"处理，不得 panic。
	if clientWantsStreamUsage(map[string]any{"stream_options": "garbage"}) {
		t.Error("non-map stream_options must not want usage")
	}
}

// 用 json 往返确认 usage 帧判定对真实上游形态（choices 缺失 / 空数组）都成立。
func TestIsUsageOnlyChunkDecoded(t *testing.T) {
	for _, raw := range []string{
		`{"usage":{"total_tokens":3},"choices":[]}`,
		`{"usage":{"total_tokens":3}}`,
	} {
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			t.Fatal(err)
		}
		if !isUsageOnlyChunk(obj) {
			t.Errorf("%s must be recognised as a usage-only chunk", raw)
		}
	}
	var obj map[string]any
	_ = json.Unmarshal([]byte(`{"usage":{"total_tokens":3},"choices":[{"index":0}]}`), &obj)
	if isUsageOnlyChunk(obj) {
		t.Error("chunk carrying choices is not usage-only")
	}
}
