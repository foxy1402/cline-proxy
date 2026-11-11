package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// 2026-11-11：x-opencode-org-id 必须按 key 解析，不能是全局常量。
// 这组测试锁定三件事：
//  1. 不同 key 拿到各自不同的 org（用户明确要求的行为）；
//  2. 发现不到时省略该头，而不是套用别的 key 的值；
//  3. 全局 cfg.OrgID 只作兜底，key 自己的解析结果永远优先。

// resetZenOrgState 清空进程内缓存与持久化，避免测试互相污染。
func resetZenOrgState(t *testing.T) {
	t.Helper()
	t.Setenv("DATA_DIR", t.TempDir())
	zenOrgMu.Lock()
	zenOrgLoaded = true // 阻止从磁盘加载
	zenOrgData = zenOrgStore{Version: zenOrgFileVersion, Orgs: map[string]string{}, Failed: map[string]int64{}}
	zenOrgFlight = map[string]*sync.WaitGroup{}
	zenOrgMu.Unlock()
	t.Cleanup(func() {
		zenOrgMu.Lock()
		zenOrgData = zenOrgStore{Version: zenOrgFileVersion, Orgs: map[string]string{}, Failed: map[string]int64{}}
		zenOrgFlight = map[string]*sync.WaitGroup{}
		zenOrgLoaded = false
		zenOrgMu.Unlock()
	})
}

// setZenOrgConfigFallback 设置全局兜底 org（cfg.OrgID）与 key 池，测试结束还原。
func setZenOrgConfigFallback(t *testing.T, org string, keys ...string) {
	t.Helper()
	prev := *getZenConfig()
	next := prev
	next.OrgID = org
	if len(keys) > 0 {
		next.Keys = keys
	}
	setZenConfig(&next)
	t.Cleanup(func() { restore := prev; setZenConfig(&restore) })
}

// fakeZenOrgServer 起一个假的 console/api/orgs：按 Bearer key 返回不同 org。
type fakeZenOrgServer struct {
	srv   *httptest.Server
	calls map[string]int
	mu    sync.Mutex
}

func newFakeZenOrgServer(t *testing.T, byKey map[string][]map[string]string) *fakeZenOrgServer {
	t.Helper()
	f := &fakeZenOrgServer{calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/console/api/orgs" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.calls[auth]++
		f.mu.Unlock()
		orgs, ok := byKey[auth]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"_tag":"ApiKeyExpired"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(orgs)
	}))
	t.Cleanup(f.srv.Close)
	old := zenOrgDiscoverBase
	zenOrgDiscoverBase = func() string { return f.srv.URL }
	t.Cleanup(func() { zenOrgDiscoverBase = old })
	return f
}

// 核心需求：每把 key 解析出自己的 org，绝不共用同一个值。
func TestZenKeyOrgPerKeyNotGlobal(t *testing.T) {
	resetZenOrgState(t)
	f := newFakeZenOrgServer(t, map[string][]map[string]string{
		"key-alpha": {{"id": "wrk_ALPHA", "name": "Alpha"}},
		"key-beta":  {{"id": "wrk_BETA", "name": "Beta"}},
		"key-gamma": {{"id": "wrk_GAMMA", "name": "Gamma"}},
	})

	got := map[string]string{}
	for _, k := range []string{"key-alpha", "key-beta", "key-gamma"} {
		got[k] = zenKeyOrg(k)
	}
	want := map[string]string{
		"key-alpha": "wrk_ALPHA",
		"key-beta":  "wrk_BETA",
		"key-gamma": "wrk_GAMMA",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("key %q -> org %q, want %q", k, got[k], w)
		}
	}

	// 三把 key 的 org 必须互不相同（这条测试的全部意义）。
	if got["key-alpha"] == got["key-beta"] || got["key-beta"] == got["key-gamma"] || got["key-alpha"] == got["key-gamma"] {
		t.Fatalf("different keys resolved to the same org: %v", got)
	}

	// 每把 key 只探测一次（结果被记住，不重复打上游）。
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range []string{"key-alpha", "key-beta", "key-gamma"} {
		if f.calls[k] != 1 {
			t.Errorf("key %q probed %d times, want exactly 1 (result must be cached)", k, f.calls[k])
		}
	}
}

// 发现不到时：返回空串（该头省略），且不借用别的 key 的值。
func TestZenKeyOrgUnresolvedOmitted(t *testing.T) {
	resetZenOrgState(t)
	newFakeZenOrgServer(t, map[string][]map[string]string{
		"good-key": {{"id": "wrk_GOOD", "name": "Good"}},
	})

	if org := zenKeyOrg("good-key"); org != "wrk_GOOD" {
		t.Fatalf("resolved org = %q, want wrk_GOOD", org)
	}
	// 未知 key：401 -> 无结论 -> 空串。
	if org := zenKeyOrg("expired-key"); org != "" {
		t.Errorf("unresolved key -> org %q, want empty (header must be omitted)", org)
	}

	// 头部层面验证：未解析的 key 必须完全不带该 header。
	req, _ := http.NewRequest("POST", "https://opencode.ai/inference/openai/v1/chat/completions", nil)
	setZenGateHeaders(req, "ses_x", "msg_x", "expired-key")
	if v := headerOrg(req); v != "" {
		t.Errorf("x-opencode-org-id = %q for unresolved key, want header absent", v)
	}

	// 已解析的 key 必须带上它自己的值。
	req2, _ := http.NewRequest("POST", "https://opencode.ai/inference/openai/v1/chat/completions", nil)
	setZenGateHeaders(req2, "ses_x", "msg_x", "good-key")
	if v := headerOrg(req2); v != "wrk_GOOD" {
		t.Errorf("x-opencode-org-id = %q, want wrk_GOOD", v)
	}
}

// 全局 cfg.OrgID 只在**单 key** 部署里兜底；多 key 时绝不套用。
//
// 依据（2026-11-11 实测）：该头被上游按 workspace 校验，发错直接 403
// "Workspace access denied"。单 key 下管理员填的值只可能是这把 key 的 org，
// 安全；多 key 下同一个值必然与部分 key 不匹配，套上去就是自伤。
func TestZenKeyOrgFallbackOnlyForSingleKeyPool(t *testing.T) {
	resetZenOrgState(t)
	newFakeZenOrgServer(t, map[string][]map[string]string{
		"key-alpha": {{"id": "wrk_ALPHA", "name": "Alpha"}},
	})

	// 能解析的 key：永远用自己的值，与 key 数量无关。
	setZenOrgConfigFallback(t, "wrk_CONFIG", "key-alpha")
	if org, src := zenKeyOrgForRequest("key-alpha"); org != "wrk_ALPHA" || src != "discovered" {
		t.Errorf("key-alpha -> (%q,%q), want (wrk_ALPHA, discovered)", org, src)
	}

	// 单 key 池 + 该 key 解析不到 -> 回退到配置值。
	if org, src := zenKeyOrgForRequest("unknown-key"); org != "wrk_CONFIG" || src != "config-fallback" {
		t.Errorf("single-key fallback -> (%q,%q), want (wrk_CONFIG, config-fallback)", org, src)
	}

	// 多 key 池 -> 解析不到就不发（不能把某把 key 的 org 套给其它 key）。
	setZenOrgConfigFallback(t, "wrk_CONFIG", "key-alpha", "key-beta")
	if org, src := zenKeyOrgForRequest("unknown-key"); org != "" || src != "" {
		t.Errorf("multi-key pool -> (%q,%q), want no org (header must be omitted)", org, src)
	}
}

// oc_sk_ 这类 key 已知无法发现（console 对它们恒 401），必须直接跳过探测，
// 不能为它白花一次请求路径上的往返。
func TestZenKeyOrgSkipsProbeForServiceAccountKey(t *testing.T) {
	resetZenOrgState(t)
	f := newFakeZenOrgServer(t, map[string][]map[string]string{
		"oc_sk_test_key": {{"id": "wrk_SHOULD_NOT_BE_USED", "name": "X"}},
	})
	if org := zenKeyOrg("oc_sk_test_key"); org != "" {
		t.Errorf("org = %q, want empty (service-account keys cannot be resolved)", org)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls["oc_sk_test_key"] != 0 {
		t.Errorf("probed %d times, want 0 (must skip known-unsupported key class)", f.calls["oc_sk_test_key"])
	}
}

// 并发：同一 key 的并发请求只触发一次发现（单飞）。
func TestZenKeyOrgSingleFlight(t *testing.T) {
	resetZenOrgState(t)
	f := newFakeZenOrgServer(t, map[string][]map[string]string{
		"key-alpha": {{"id": "wrk_ALPHA", "name": "Alpha"}},
	})

	const n = 8
	var wg sync.WaitGroup
	var mismatches int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if org := zenKeyOrg("key-alpha"); org != "wrk_ALPHA" {
				atomic.AddInt64(&mismatches, 1)
			}
		}()
	}
	wg.Wait()
	if mismatches != 0 {
		t.Errorf("%d goroutines got a wrong org", mismatches)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls["key-alpha"] != 1 {
		t.Errorf("upstream probed %d times, want 1 (single-flight)", f.calls["key-alpha"])
	}
}

// 负缓存：失败后短期内不再探测。
func TestZenKeyOrgNegativeCache(t *testing.T) {
	resetZenOrgState(t)
	f := newFakeZenOrgServer(t, map[string][]map[string]string{})

	for i := 0; i < 3; i++ {
		if org := zenKeyOrg("bad-key"); org != "" {
			t.Fatalf("org = %q, want empty", org)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls["bad-key"] != 1 {
		t.Errorf("probed %d times, want 1 (negative cache must suppress retries)", f.calls["bad-key"])
	}
}

// 无 key（匿名路径）时只认显式配置。
func TestSetZenGateHeadersNoKeyOmitsOrg(t *testing.T) {
	resetZenOrgState(t)
	setZenOrgConfigFallback(t, "")
	req, _ := http.NewRequest("POST", "https://opencode.ai/inference/openai/v1/chat/completions", nil)
	setZenGateHeaders(req, "ses_x", "msg_x", "")
	if v := headerOrg(req); v != "" {
		t.Errorf("x-opencode-org-id = %q with no key and no config, want absent", v)
	}

	setZenOrgConfigFallback(t, "wrk_EXPLICIT", "only-key")
	req2, _ := http.NewRequest("POST", "https://opencode.ai/inference/openai/v1/chat/completions", nil)
	setZenGateHeaders(req2, "ses_x", "msg_x", "")
	if v := headerOrg(req2); v != "wrk_EXPLICIT" {
		t.Errorf("x-opencode-org-id = %q, want wrk_EXPLICIT", v)
	}
}

// headerOrg 读取小写键名写入的 x-opencode-org-id。
//
// 本仓库刻意用 map 直写小写头名（官方 CLI 就发小写；Header.Set 会规范化成
// X-Opencode-...，字节层面不同）。因此不能用 Header.Get（它按规范形式查找），
// 必须直接读 map。
func headerOrg(req *http.Request) string {
	if v := req.Header["x-opencode-org-id"]; len(v) > 0 {
		return v[0]
	}
	return ""
}

// 持久化文件绝不能包含明文 key（排障时 data 目录常被打包外发）。
func TestZenKeyOrgPersistHasNoRawKey(t *testing.T) {
	resetZenOrgState(t)
	newFakeZenOrgServer(t, map[string][]map[string]string{
		"sk-live-super-secret-value": {{"id": "wrk_SECRET_ORG", "name": "S"}},
	})
	if org := zenKeyOrg("sk-live-super-secret-value"); org != "wrk_SECRET_ORG" {
		t.Fatalf("org = %q", org)
	}
	// 触发一次失败记录，确保 failed 表也走哈希（负缓存路径）。
	_ = zenKeyOrg("sk-live-another-secret")

	data, err := os.ReadFile(zenOrgFile())
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	if strings.Contains(string(data), "sk-live-") {
		t.Errorf("cache file leaks a raw key: %s", data)
	}
	// 但 org 本身要能恢复出来（重启后不用再探测）。
	if !strings.Contains(string(data), "wrk_SECRET_ORG") {
		t.Errorf("cache file lost the resolved org: %s", data)
	}
}

// 回归护栏（2026-11-11 实测）：发错 org 会让上游回 403 "Workspace access denied"。
// 因此多 key 池里，任何一把解析不到 org 的 key 都必须**不带**该头，绝不能借用
// 别的 key 的 org 或某个全局值——那会把可成功的请求变成硬失败。
func TestZenGateHeadersNeverSendsBorrowedOrgInMultiKeyPool(t *testing.T) {
	resetZenOrgState(t)
	newFakeZenOrgServer(t, map[string][]map[string]string{
		"key-alpha": {{"id": "wrk_ALPHA", "name": "Alpha"}},
		"key-beta":  {{"id": "wrk_BETA", "name": "Beta"}},
	})
	setZenOrgConfigFallback(t, "wrk_CONFIG", "key-alpha", "key-beta", "key-expired")

	cases := map[string]string{
		"key-alpha":   "wrk_ALPHA",
		"key-beta":    "wrk_BETA",
		"key-expired": "", // 解析不到 + 多 key => 必须省略
	}
	for key, want := range cases {
		req, _ := http.NewRequest("POST", "https://opencode.ai/inference/openai/v1/chat/completions", nil)
		setZenGateHeaders(req, "ses_x", "msg_x", key)
		if got := headerOrg(req); got != want {
			t.Errorf("key %q -> x-opencode-org-id %q, want %q", key, got, want)
		}
	}
}
