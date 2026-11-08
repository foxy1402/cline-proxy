package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 抓包默认必须关闭：它是排障工具，落盘的是完整会话内容，
// 未显式开启时绝不能写任何文件。
//
// 早先这个测试断言的是 t.TempDir()——一个与抓包路径毫无关系的目录，永远为空，
// 因此无论实现怎么写都会通过（零信息量）。现在断言真实判定：未配置时
// zenCaptureDir() 必须为空（实现据此直接返回，不落盘）。
func TestCaptureOffByDefault(t *testing.T) {
	t.Setenv("ZEN_CAPTURE_DIR", "")
	if dir := zenCaptureDir(); dir != "" {
		t.Fatalf("capture dir must be empty when unset, got %q", dir)
	}
	// 空目录时调用不得 panic、不得创建任何东西（现在它连 OpenFile 都不会走到）。
	captureZenRequest("chat", map[string]any{"model": "m"}, http.Header{})
}

// 开启后必须落盘，且完整记录 system prompt、工具数量与上游 body。
func TestCaptureWritesSystemPromptAndTools(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZEN_CAPTURE_DIR", dir)

	sysText := "you are zcode ide, an agentic coding assistant"
	params := map[string]any{
		"model":  "big-pickle",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "system", "content": sysText},
			map[string]any{"role": "user", "content": "hello"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "read_file", "description": "read",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}}},
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer oc_sk_supersecretvalue123456")
	h.Set("X-Opencode-Session-Id", "ses_abc")

	captureZenRequest("chat", params, h)

	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 capture file, got %d", len(files))
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var rec zenCaptureRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("capture is not valid JSON: %v", err)
	}
	if !strings.Contains(rec.SystemPrompt, "agentic coding assistant") {
		t.Fatalf("system prompt not captured: %q", rec.SystemPrompt)
	}
	if rec.MessageCount != 2 || rec.ToolCount != 1 {
		t.Fatalf("counts wrong: msgs=%d tools=%d", rec.MessageCount, rec.ToolCount)
	}
	if rec.UpstreamTool < 12 {
		t.Fatalf("upstream body must include the gate tools, got %d", rec.UpstreamTool)
	}
	if rec.UpstreamSize <= rec.ClientBodySize {
		t.Fatalf("upstream body (%d) should exceed client body (%d) due to gate tools",
			rec.UpstreamSize, rec.ClientBodySize)
	}
	if rec.Headers["Authorization"] != "<redacted>" {
		t.Fatalf("Authorization must be redacted, got %q", rec.Headers["Authorization"])
	}
	if strings.Contains(string(raw), "supersecretvalue") {
		t.Fatal("API key leaked into the capture file")
	}
}

// 配置中的 key 出现在任意字段里都必须被替换。
func TestCaptureRedactsConfiguredKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZEN_CAPTURE_DIR", dir)
	saved := getZenConfig()
	cfg := *saved
	cfg.Keys = []string{"oc_sk_LEAKME_abcdef123456"}
	setZenConfig(&cfg)
	t.Cleanup(func() { setZenConfig(saved) })

	params := map[string]any{
		"model":    "big-pickle",
		"messages": []any{map[string]any{"role": "user", "content": "key is oc_sk_LEAKME_abcdef123456 ok"}},
	}
	captureZenRequest("chat", params, http.Header{})

	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), "LEAKME") {
		t.Fatal("configured key leaked into the capture file")
	}
}
