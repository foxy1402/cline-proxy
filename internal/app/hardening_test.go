package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 被 413 拒绝的超大请求也必须进请求日志：滥用诊断的盲区正是这些被拒请求。
// 中间件此前在 413 处 return，绕过了统一日志块。
func TestRequestLogMiddlewareLogs413(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)
	t.Setenv("LOG_REQUESTS", "true")
	t.Setenv("MAX_BODY_MB", "1") // 1MB，便于用小 body 触发超限

	// 清理内存环形缓冲，避免其他测试的残留
	reqLogsMu.Lock()
	saved := reqLogs
	reqLogs = nil
	reqLogsMu.Unlock()
	t.Cleanup(func() {
		reqLogsMu.Lock()
		reqLogs = saved
		reqLogsMu.Unlock()
	})

	handler := requestLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("oversized request must not reach the wrapped handler")
		w.WriteHeader(http.StatusOK)
	}))

	body := strings.NewReader(`{"model":"x","pad":"` + strings.Repeat("a", 2<<20) + `"}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", body))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	logs := LoadRequestLogs()
	if len(logs) == 0 {
		t.Fatal("a rejected 413 request must still be logged")
	}
	if last := logs[len(logs)-1]; last.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("logged status = %d, want 413", last.Status)
	}
}

// envInt 对非空但无效的值必须打印警告（与 envBool 一致），静默回退会让
// 配置拼写错误无从察觉。
func TestEnvIntWarnsOnInvalid(t *testing.T) {
	t.Setenv("ZTEST_INT", "8s")
	if _, ok := envInt("ZTEST_INT"); ok {
		t.Fatal("invalid integer must report ok=false")
	}
	// 空值不警告（不算错误）
	t.Setenv("ZTEST_INT", "")
	if _, ok := envInt("ZTEST_INT"); ok {
		t.Fatal("empty must report ok=false")
	}
	// 合法值正常解析
	t.Setenv("ZTEST_INT", "42")
	if n, ok := envInt("ZTEST_INT"); !ok || n != 42 {
		t.Fatalf("envInt = %d,%v; want 42,true", n, ok)
	}
}

// zenExtractReplacement 稳健性：Go JSON 大小写不敏感本已覆盖 "Replacement"，
// 这里锁定 error.details/data、metadata 三种嵌套与"非 JSON body"的文本回退。
func TestZenExtractReplacementRobust(t *testing.T) {
	cases := map[string]string{
		`{"replacement":"mimo-v2.6-flash-free"}`:              "mimo-v2.6-flash-free",
		`{"Replacement":"x-free"}`:                            "x-free", // 大小写变体
		`{"error":{"replacement":"e-free"}}`:                  "e-free",
		`{"error":{"details":{"replacement":"d-free"}}}`:      "d-free",
		`{"error":{"data":{"replacement":"data-free"}}}`:      "data-free",
		`{"metadata":{"replacement":"meta-free"}}`:            "meta-free",
		`model gone, replacement: "raw-free" (upstream says)`: "raw-free", // 非 JSON
		`{"detail":"no replacement here"}`:                    "",
		`{"replacement":""}`:                                  "",
	}
	for body, want := range cases {
		if got := zenExtractReplacement(body); got != want {
			t.Errorf("zenExtractReplacement(%q) = %q, want %q", body, got, want)
		}
	}
}

// 会话轮换周期经管理配置往返：显式 0（关闭）必须存得下，缺省（未提交该字段）
// 不得被改写成 0——两者在 JSON 里都是 0，只有指针能区分。
func TestZenConfigSessionRotateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	cur := getZenConfig()
	base := *cur
	base.Keys = []string{"sk-roundtrip"}
	base.SessionRotateMinutes = 120
	setZenConfig(&base)
	t.Cleanup(func() { c := *cur; setZenConfig(&c) })

	post := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/admin/api/opencode/config/update", strings.NewReader(body))
		handleZenConfigUpdate(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("update status = %d body=%s", rec.Code, rec.Body.String())
		}
	}

	// 显式 0：关闭轮换，必须真的存进配置
	post(`{"sessionRotateMinutes":0}`)
	if got := getZenConfig().SessionRotateMinutes; got != 0 {
		t.Fatalf("explicit 0 must persist (rotation disabled), got %d", got)
	}
	if iv := sessionRotateInterval(); iv != 0 {
		t.Fatalf("interval with rotation disabled = %v, want 0", iv)
	}

	// 显式 30：更新为 30 分钟
	post(`{"sessionRotateMinutes":30}`)
	if got := getZenConfig().SessionRotateMinutes; got != 30 {
		t.Fatalf("rotate minutes = %d, want 30", got)
	}
	if iv := sessionRotateInterval(); iv != 30*time.Minute {
		t.Fatalf("interval = %v, want 30m", iv)
	}

	// 不提交该字段：保留现值（不是回落默认，也不是 0）
	post(`{}`)
	if got := getZenConfig().SessionRotateMinutes; got != 30 {
		t.Fatalf("omitted field must keep the current value, got %d", got)
	}
}
