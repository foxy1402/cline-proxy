package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// zenFreeUsageBody 上游日额度耗尽的真实错误体（2026-10-30/31 实测原文）。
const zenFreeUsageBody = `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"}}`

// retryAfterToMidnight 返回"距下一个 00:00 UTC 的秒数"，与上游行为一致。
func retryAfterToMidnight() time.Duration {
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	return midnight.Sub(now)
}

func resetZenParkState(t *testing.T) {
	t.Helper()
	zenKeyMu.Lock()
	zenKeyPark = map[string]zenParkState{}
	zenKeyCool = map[string]time.Time{}
	zenKeyMu.Unlock()
	t.Cleanup(func() {
		zenKeyMu.Lock()
		zenKeyPark = map[string]zenParkState{}
		zenKeyCool = map[string]time.Time{}
		zenKeyMu.Unlock()
	})
}

// 只有"额度类错误 + RA 指向午夜"才算日额度耗尽。
// 这里逐条钉住判据，防止以后放宽到"任何 429 都停放"——那会让一次
// 瞬时节流把 key 停到第二天，正是 2026-10-30 踩过的坑。
func TestZenDailyExhaustedDetection(t *testing.T) {
	midnight := retryAfterToMidnight()
	cases := []struct {
		name       string
		status     int
		body       string
		retryAfter time.Duration
		want       bool
	}{
		{"free usage limit + RA to midnight", 429, zenFreeUsageBody, midnight, true},
		{"free usage limit, RA not midnight", 429, zenFreeUsageBody, 30 * time.Second, false},
		{"free usage limit, no RA", 429, zenFreeUsageBody, 0, false},
		// FreeTierError 是门禁/策略拒绝，不是额度耗尽——绝不能停放
		{"freetier gate error", 429, `{"error":{"type":"FreeTierError","message":"can only be used from within OpenCode"}}`, midnight, false},
		// 403 不是 429
		{"403 freetier", 403, `{"error":{"type":"FreeTierError"}}`, midnight, false},
		// 瞬时节流（无额度措辞）
		{"generic rate limit", 429, `{"error":{"message":"rate limit exceeded"}}`, midnight, false},
		// 503 是过载，不是额度
		{"503 overloaded", 503, zenFreeUsageBody, midnight, false},
	}
	for _, c := range cases {
		got := zenDailyExhausted(c.status, c.body, c.retryAfter)
		if got != c.want {
			t.Errorf("%s: zenDailyExhausted = %v, want %v", c.name, got, c.want)
		}
	}
}

// 停放必须指数退避并封顶：额度整天不恢复时，一天的探针次数必须是常数级，
// 而不是每分钟一个。
func TestZenParkBackoffIsBounded(t *testing.T) {
	resetZenParkState(t)
	saved := getZenConfig()
	cfg := *saved
	cfg.Keys = []string{"k1"}
	setZenConfig(&cfg)
	t.Cleanup(func() { setZenConfig(saved) })

	var durs []time.Duration
	for i := 0; i < 8; i++ {
		durs = append(durs, parkZenKey("k1", "test"))
	}
	for i := 1; i < len(durs); i++ {
		if durs[i] < durs[i-1] {
			t.Fatalf("backoff must not shrink: %v then %v", durs[i-1], durs[i])
		}
	}
	last := durs[len(durs)-1]
	if last > zenParkMaxBackoff {
		t.Fatalf("backoff %v exceeds cap %v", last, zenParkMaxBackoff)
	}
	if durs[0] != zenParkInitial {
		t.Fatalf("first park = %v, want %v", durs[0], zenParkInitial)
	}
	// 8 次停放（含首次）应已到达上限：30m,1h,2h,4h,6h,6h,...
	if last != zenParkMaxBackoff {
		t.Fatalf("after 8 parks backoff = %v, want cap %v", last, zenParkMaxBackoff)
	}
	// 一天的探针数：从 30m 起翻倍到 6h，24h 内不超过 8 次
	if total := simulateDayProbes(); total > 10 {
		t.Fatalf("a fully exhausted day would spend %d probes, want <= 10", total)
	}
}

// simulateDayProbes 用固定时长序列模拟 24 小时内的探针次数。
func simulateDayProbes() int {
	elapsed := time.Duration(0)
	n := 0
	backoff := zenParkInitial
	for elapsed < 24*time.Hour {
		n++
		elapsed += backoff
		backoff *= 2
		if backoff > zenParkMaxBackoff {
			backoff = zenParkMaxBackoff
		}
	}
	return n
}

// 停放期内 pickZenKey 必须完全跳过该 key（这是"不再逐请求打上游"的核心）。
func TestPickZenKeySkipsParkedKey(t *testing.T) {
	resetZenParkState(t)
	saved := getZenConfig()
	cfg := *saved
	cfg.Keys = []string{"k1", "k2"}
	setZenConfig(&cfg)
	t.Cleanup(func() { setZenConfig(saved) })

	parkZenKey("k1", "quota")
	for i := 0; i < 8; i++ {
		if got := pickZenKey(); got == "k1" {
			t.Fatalf("pickZenKey returned parked key k1 (call %d); parked keys must not be selected", i)
		}
	}
}

// 200 必须解除停放：上游提前恢复时（实测确实会提前），不该继续停到退避结束。
func TestSuccessUnparksKey(t *testing.T) {
	resetZenParkState(t)
	parkZenKey("k1", "quota")
	if _, parked := zenKeyParked("k1"); !parked {
		t.Fatal("setup: key should be parked")
	}
	markZenKeySuccess("k1")
	if _, parked := zenKeyParked("k1"); parked {
		t.Fatal("a 200 must clear the park")
	}
}

// 跨日必须自动失效：昨天的退避不能拖到今天。
func TestParkExpiresAcrossDays(t *testing.T) {
	resetZenParkState(t)
	saved := getZenConfig()
	cfg := *saved
	cfg.Keys = []string{"k1"}
	setZenConfig(&cfg)
	t.Cleanup(func() { setZenConfig(saved) })

	zenKeyMu.Lock()
	zenKeyPark["k1"] = zenParkState{
		Until:   time.Now().Add(5 * time.Hour),
		Backoff: 5 * time.Hour,
		Day:     time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02"), // 昨天
		Probes:  3,
	}
	zenKeyMu.Unlock()

	if _, parked := zenKeyParked("k1"); parked {
		t.Fatal("yesterday's park must not apply today")
	}
	if got := pickZenKey(); got != "k1" {
		t.Fatalf("cross-day park must not block selection, got %q", got)
	}
	if _, ok := zenAllParkedUntil(); ok {
		t.Fatal("a stale cross-day park must not mark the pool as fully parked")
	}
}

// 全池停放时：不发上游请求，直接给出带恢复时刻的 429。
func TestAllParkedBailsOutWithoutUpstreamCall(t *testing.T) {
	resetZenParkState(t)

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(zenFreeUsageBody))
	}))
	defer upstream.Close()

	saved := getZenConfig()
	cfg := *saved
	cfg.Enabled = true
	cfg.BaseURL = upstream.URL
	cfg.Keys = []string{"k1"}
	cfg.Proxies = nil
	setZenConfig(&cfg)
	setZenModelForTest("zzz-parked-model", "")
	t.Cleanup(func() { setZenConfig(saved) })
	zenSessFailedMu.Lock()
	zenSessFailed = map[string]int{}
	zenSessFailedMu.Unlock()

	parkZenKey("k1", zenFreeUsageBody)

	params := map[string]any{
		"model":    "zzz-parked-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	start := time.Now()
	_, _, err := callZenAPI(t.Context(), params, true)
	elapsed := time.Since(start)

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("upstream was called %d time(s) while every key is parked; must be 0", got)
	}
	if elapsed > time.Second {
		t.Fatalf("rejection took %v; a parked pool must fail immediately", elapsed)
	}
	var he *zenHTTPError
	if !errors.As(err, &he) || he.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want zenHTTPError 429", err)
	}
	if !strings.Contains(he.Body, "next automatic probe") {
		t.Fatalf("error must state when the next probe happens, got %q", he.Body)
	}
}

// 端到端：真上游 429（额度耗尽）→ 停放 → 第二次请求不再打上游。
func TestDailyExhaustedParksAndStopsRetrying(t *testing.T) {
	resetZenParkState(t)

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "80378")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(zenFreeUsageBody))
	}))
	defer upstream.Close()

	saved := getZenConfig()
	cfg := *saved
	cfg.Enabled = true
	cfg.BaseURL = upstream.URL
	cfg.Keys = []string{"k1"}
	cfg.Proxies = nil
	cfg.Retries = 3
	setZenConfig(&cfg)
	setZenModelForTest("zzz-quota-model", "")
	t.Cleanup(func() { setZenConfig(saved) })
	zenSessFailedMu.Lock()
	zenSessFailed = map[string]int{}
	zenSessFailedMu.Unlock()

	// 第一次：打到上游，拿到额度耗尽的 429 → 停放。
	first := retryAfterToMidnight()
	if !zenDailyExhausted(429, zenFreeUsageBody, first) {
		t.Fatalf("setup: midnight RA (%v) must be detected as daily exhaustion", first)
	}
	params := map[string]any{
		"model":    "zzz-quota-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}
	_, _, err := callZenAPI(t.Context(), params, true)
	var he *zenHTTPError
	if !errors.As(err, &he) || he.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want 429", err)
	}
	if _, parked := zenKeyParked("k1"); !parked {
		t.Fatal("daily exhaustion must park the key")
	}
	if _, cooling := zenKeyCooldownUntil("k1"); cooling {
		t.Fatal("a parked key must not also sit in the 20-minute cooldown table")
	}

	before := atomic.LoadInt32(&hits)
	// 第二次：全池停放 → 一个上游请求都不该发。
	_, _, _ = callZenAPI(t.Context(), params, true)
	if after := atomic.LoadInt32(&hits); after != before {
		t.Fatalf("second request hit upstream (%d -> %d) despite all keys being parked", before, after)
	}
}

// 多 key 池：一个 key 额度耗尽必须换到另一个，而不是整体失败。
func TestDailyExhaustedRotatesToHealthyKey(t *testing.T) {
	resetZenParkState(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("Authorization"), "bad") {
			w.Header().Set("Retry-After", "80378")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(zenFreeUsageBody))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer upstream.Close()

	saved := getZenConfig()
	cfg := *saved
	cfg.Enabled = true
	cfg.BaseURL = upstream.URL
	cfg.Keys = []string{"sk-bad", "sk-good"}
	cfg.Proxies = nil
	cfg.Retries = 3
	setZenConfig(&cfg)
	setZenModelForTest("zzz-multi-model", "")
	t.Cleanup(func() { setZenConfig(saved) })
	zenSessFailedMu.Lock()
	zenSessFailed = map[string]int{}
	zenSessFailedMu.Unlock()

	resp, _, err := callZenAPI(t.Context(), map[string]any{
		"model":    "zzz-multi-model",
		"messages": []any{map[string]any{"role": "user", "content": "x"}},
	}, true)
	if err != nil {
		t.Fatalf("a quota-exhausted key must rotate to the healthy one, got %v", err)
	}
	resp.Body.Close()
	if _, parked := zenKeyParked("sk-bad"); !parked {
		t.Fatal("the exhausted key must be parked")
	}
	if _, parked := zenKeyParked("sk-good"); parked {
		t.Fatal("the healthy key must not be parked")
	}
}

// 停放状态必须能在面板快照里看到（否则用户看不出 key 为什么不动）。
func TestZenKeyStatusReportsParked(t *testing.T) {
	resetZenParkState(t)
	saved := getZenConfig()
	cfg := *saved
	cfg.Keys = []string{"k1"}
	setZenConfig(&cfg)
	t.Cleanup(func() { setZenConfig(saved) })

	parkZenKey("k1", zenFreeUsageBody)
	var found bool
	for _, st := range zenKeyStatus() {
		if st["index"] == 0 {
			found = true
			if st["parked"] != true {
				t.Fatalf("status must report parked=true, got %v", st["parked"])
			}
			if st["parkedUntil"] == nil {
				t.Fatal("status must report parkedUntil")
			}
			b, _ := json.Marshal(st)
			if !strings.Contains(string(b), "parkProbes") {
				t.Fatal("status must expose parkProbes")
			}
		}
	}
	if !found {
		t.Fatal("key status missing")
	}
}
