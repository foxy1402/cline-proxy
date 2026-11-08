package app

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ============ 审计修复的回归测试 ============

// API_KEY_FILE / ADMIN_PASSWORD_FILE 读不到时绝不能回落到"未配置"：
// AdminAuthRequired()=false 会让公网面板静默免认证，APIKeyEnv()="" 会让 /v1
// 回落到"动态 key 列表为空则放行"。两者都必须 fail closed。
func TestSecretFileFailClosed(t *testing.T) {
	// 目录名而非文件：ReadFile 必然失败，且不依赖权限位（Windows 上不可靠）
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope")

	t.Setenv("API_KEY_FILE", missing)
	t.Setenv("API_KEY", "fallback-should-not-be-used")
	if got := APIKeyEnv(); got == "" || got == "fallback-should-not-be-used" {
		t.Fatalf("unreadable API_KEY_FILE must seal the value, got %q", got)
	}

	// 空文件同理：不能当成"没配密码"
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("   \n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIN_PASSWORD_FILE", empty)
	t.Setenv("ADMIN_PASSWORD", "fallback-should-not-be-used")
	if got := AdminPasswordEnv(); got == "" || got == "fallback-should-not-be-used" {
		t.Fatalf("empty ADMIN_PASSWORD_FILE must seal the value, got %q", got)
	}
	if !AdminAuthRequired() {
		t.Fatal("auth must stay required when the password file is broken")
	}

	// 有效的文件仍然正常读取（不能为了 fail-closed 把正常路径弄坏）
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("  s3cret  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ADMIN_PASSWORD_FILE", good)
	if got := AdminPasswordEnv(); got != "s3cret" {
		t.Fatalf("valid password file truncated wrong: %q", got)
	}
}

// 未配置 *_FILE 时必须回落到普通环境变量（哨兵只替换"已配置但坏了"的情况）。
func TestSecretFileFallsBackWhenUnset(t *testing.T) {
	t.Setenv("API_KEY_FILE", "")
	t.Setenv("ADMIN_PASSWORD_FILE", "")
	t.Setenv("API_KEY", "plain-key")
	t.Setenv("ADMIN_PASSWORD", "plain-pw")
	if got := APIKeyEnv(); got != "plain-key" {
		t.Fatalf("APIKeyEnv = %q, want plain-key", got)
	}
	if got := AdminPasswordEnv(); got != "plain-pw" {
		t.Fatalf("AdminPasswordEnv = %q, want plain-pw", got)
	}
}

// SSE 帧里的 error 事件必须让聚合失败：只凭"没有 choices"就 continue 会把上游
// 故障变成 200 + 空内容。纯 usage 收尾帧（无 error）仍要正常放过。
func TestCollectStreamResponsePropagatesSSEError(t *testing.T) {
	sse := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5}}\n\n" +
		"data: {\"error\":{\"message\":\"upstream exploded\"}}\n\n"
	_, err := collectStreamResponse(sseResp(sse))
	if err == nil {
		t.Fatal("an SSE error event must fail the aggregation, not yield an empty 200")
	}
	if !strings.Contains(err.Error(), "upstream exploded") {
		t.Fatalf("error message must reach the caller, got %v", err)
	}

	// 无 error 的 usage-only 收尾帧照旧正常返回
	out, err := collectStreamResponse(sseResp("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5}}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"))
	if err != nil {
		t.Fatalf("usage-only frames must not be treated as errors: %v", err)
	}
	if got, _ := getNested(out, "choices", 0, "message", "content").(string); got != "hi" {
		t.Fatalf("content = %q, want hi", got)
	}
}

// 非 SSE 的 JSON 错误体同样必须上抛（既有行为，回归保护）。
func TestCollectStreamResponseRejectsJSONErrorBody(t *testing.T) {
	_, err := collectStreamResponse(sseResp(`{"error":{"message":"busy","type":"server_error"}}`))
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("JSON error body must surface as an error, got %v", err)
	}
}

// emitChatAsSSE 分块时不得切断多字节字符：半个 rune 会被消费端替换成 U+FFFD。
func TestEmitChatAsSSEPreservesUTF8AcrossChunks(t *testing.T) {
	// 4000 个三字节字符 = 12000 字节 > 2048 的分块边界，且边界必然落在字符中间
	content := strings.Repeat("水", 4000)
	chat := map[string]any{
		"model": "test-model",
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
	}
	rec := &flushRecorder{header: http.Header{}}

	emitChatAsSSE(rec, chat, nil)

	var rebuilt strings.Builder
	sc := bufio.NewScanner(strings.NewReader(rec.body.String()))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		if c, ok := getNested(obj, "choices", 0, "delta", "content").(string); ok {
			rebuilt.WriteString(c)
		}
	}
	if rebuilt.String() != content {
		t.Fatalf("content corrupted across chunks: got %d bytes, want %d (valid utf8=%v)",
			rebuilt.Len(), len(content), utf8.ValidString(rebuilt.String()))
	}
	if strings.ContainsRune(rebuilt.String(), '\uFFFD') {
		t.Fatal("replacement character in reassembled content: a rune was split")
	}
}

// getNested 是"取不到就 nil"的宽容语义，负数下标必须返回 nil 而不是 panic。
func TestGetNestedNegativeIndex(t *testing.T) {
	obj := map[string]any{"choices": []any{map[string]any{"a": 1}}}
	if got := getNested(obj, "choices", -1); got != nil {
		t.Fatalf("negative index must yield nil, got %v", got)
	}
	if got := getNested(obj, "choices", 5); got != nil {
		t.Fatalf("out-of-range index must yield nil, got %v", got)
	}
	if got := getNested(obj, "choices", 0, "a"); got != 1 {
		t.Fatalf("valid path broke: %v", got)
	}
}

// 退避延迟必须封顶：无上限的指数翻倍会在 retries 较大时溢出成负数，
// time.NewTimer(负值) 立即触发 → 重试退化成无退避热循环。
func TestZenRetryDelayCapped(t *testing.T) {
	d := time.Second
	for i := 0; i < 200; i++ {
		d = zenRetryDelay(d)
		if d <= 0 {
			t.Fatalf("delay went non-positive after %d doublings: %v", i, d)
		}
		if d > 30*time.Second {
			t.Fatalf("delay exceeded the 30s cap after %d doublings: %v", i, d)
		}
	}
	if d != 30*time.Second {
		t.Fatalf("delay should settle at the cap, got %v", d)
	}
}

// 压缩阈值必须有下限：output 声明不小于 context 时阈值会算成 0/负数，
// 于是每个请求都判定需要压缩，而压缩永远降不到阈值以下 → 每次请求都多跑一次摘要。
func TestCompactThresholdHasFloor(t *testing.T) {
	if got := compactThreshold(2000, 32768, 20000); got < 1000 {
		t.Fatalf("threshold = %d; a small-window model must keep at least half the window", got)
	}
	// 常规模型不受影响：200000 - max(32768, 20000) = 167232
	if got := compactThreshold(200000, 32768, 20000); got != 167232 {
		t.Fatalf("threshold = %d, want 167232", got)
	}
}

// fallbackTruncate 必须保留连续尾部：逐条 continue 会在会话中间挖洞，
// 丢掉最新一轮上下文却留着更古老的记忆。
func TestFallbackTruncateKeepsContiguousTail(t *testing.T) {
	m := &ZenModel{ID: "t", Context: 1000}
	msgs := []any{map[string]any{"role": "system", "content": "sys"}}
	// 中间放几条巨大的消息，尾部放几条小的（最新一轮）
	for i := 0; i < 6; i++ {
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": strings.Repeat("大", 400),
		})
	}
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": "final answer"},
		map[string]any{"role": "user", "content": "last question"},
	)
	params := map[string]any{"messages": msgs}
	out := fallbackTruncate(params, m)
	if !out.changed {
		t.Fatal("expected truncation to happen")
	}
	got, _ := params["messages"].([]any)
	if len(got) == 0 {
		t.Fatal("no messages kept")
	}
	// 最后两条必须保留（最新一轮），中间挖洞会在它们之前出现大消息
	if c, _ := getNested(got[len(got)-1].(map[string]any), "content").(string); c != "last question" {
		t.Fatalf("newest message dropped: %q", c)
	}
	// 保留的**非 system** 消息必须是原序列的一个连续后缀：逐条 continue 会在
	// 会话中间挖洞（留着更古老的记忆却丢掉紧邻的最新上下文）。
	//
	// 早先这里只算了 seenBig 就 break，从不断言 —— 等于没有检查。现在直接比对
	// 原始顺序：把 got 中非 system 的 content 序列，与"原序列去掉 system 后的
	// 尾部 len(gotNonSystem) 条"逐项对照。
	var orig []string
	for _, raw := range msgs {
		mm, _ := raw.(map[string]any)
		if mm == nil || strField(mm, "role") == "system" {
			continue
		}
		c, _ := mm["content"].(string)
		orig = append(orig, c)
	}
	var kept []string
	for _, raw := range got {
		mm, _ := raw.(map[string]any)
		if mm == nil || strField(mm, "role") == "system" {
			continue
		}
		c, _ := mm["content"].(string)
		kept = append(kept, c)
	}
	if len(kept) > len(orig) {
		t.Fatalf("kept %d messages but only %d existed", len(kept), len(orig))
	}
	tail := orig[len(orig)-len(kept):]
	for i := range kept {
		if kept[i] != tail[i] {
			t.Fatalf("retained messages are not a contiguous suffix of the original: "+
				"index %d kept %q, want %q (a hole was punched mid-conversation)",
				i, kept[i], tail[i])
		}
	}
}

// flushRecorder 满足 http.ResponseWriter + http.Flusher，收集 emit 出去的事件。
type flushRecorder struct {
	header http.Header
	body   strings.Builder
	status int
}

func (f *flushRecorder) Header() http.Header       { return f.header }
func (f *flushRecorder) WriteHeader(status int)    { f.status = status }
func (f *flushRecorder) Write(b []byte) (int, error) { return f.body.Write(b) }
func (f *flushRecorder) Flush()                    {}