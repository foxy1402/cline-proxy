package app

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

// 端点学习的判定必须只依据上游的 HTTP 状态码。
//
// 回归点（曾经的 P0）：isWrongEndpoint 早先用子串匹配整条错误文本，关键词表里
// 含 "no such" 与 "endpoint"。DNS 拨号失败的错误串是
// "dial tcp: lookup opencode.ai: no such host"，而 callZenAPI 会把它原样 wrap 后
// 返回 —— 于是一次瞬时断网就能把某个 chat 原生模型"学习"成 responses 并写进
// .zen-endpoints.json，目录同步不会改写既有条目的 Upstream，该模型从此永久 502。
func TestIsWrongEndpointIgnoresNetworkErrors(t *testing.T) {
	netErrs := []error{
		fmt.Errorf("zen request: %w", errors.New("dial tcp: lookup opencode.ai on 127.0.0.53:53: no such host")),
		fmt.Errorf("zen responses request: %w", errors.New("dial tcp 198.51.100.7:443: i/o timeout")),
		fmt.Errorf("zen request: %w", errors.New("read tcp: connection reset by peer")),
		errors.New("client aborted: context canceled"),
		errors.New("create zen request: unsupported protocol scheme \"\""),
	}
	for _, err := range netErrs {
		if isWrongEndpoint(err) {
			t.Errorf("network/lifecycle error must never trigger endpoint learning: %v", err)
		}
		if isWrongEndpointResponses(err) {
			t.Errorf("network/lifecycle error must never trigger reverse endpoint learning: %v", err)
		}
	}
	if isWrongEndpoint(nil) || isWrongEndpointResponses(nil) {
		t.Fatal("nil error must not be treated as a wrong endpoint")
	}
}

func TestIsWrongEndpointStatusDiscipline(t *testing.T) {
	cases := []struct {
		name string
		he   *zenHTTPError
		want bool
	}{
		// 唯一能识别 spark 端点的信号：chat 端点上的裸 500
		{"chat native model on chat endpoint", &zenHTTPError{Status: 500, Body: `{"error":"Internal server error"}`}, true},
		// 瞬时网关错误绝不能被当成模型属性（会持久化到错误端点）
		{"502 transient", &zenHTTPError{Status: 502, Body: `{"error":"endpoint not found"}`}, false},
		{"503 overload", &zenHTTPError{Status: 503, Body: `no such model`}, false},
		{"503 endpoint unavailable", &zenHTTPError{Status: 503, Body: `{"error":{"type":"server_error","message":"Upstream request failed: Endpoint is unavailable."}}`}, true},
		{"504 timeout", &zenHTTPError{Status: 504, Body: `unsupported endpoint`}, false},
		// 4xx 才看特征词
		{"404 with routing keyword", &zenHTTPError{Status: 404, Body: `{"error":"model not found"}`}, true},
		{"400 unsupported model", &zenHTTPError{Status: 400, Body: `{"error":"unsupported model"}`}, true},
		{"400 unrelated", &zenHTTPError{Status: 400, Body: `{"error":"max_tokens too large"}`}, false},
		// 限流/会话类虽然也是 4xx，但走换 key 重试，不做端点学习
		{"429 quota keyword", &zenHTTPError{Status: 429, Body: `{"error":"quota exceeded for endpoint"}`}, false},
		{"403 session keyword", &zenHTTPError{Status: 403, Body: `{"error":"session expired, use /v1/responses"}`}, false},
		{"400 freetier", &zenHTTPError{Status: 400, Body: `{"error":"FreeTier gate: no such model"}`}, false},
	}
	for _, c := range cases {
		if got := isWrongEndpoint(c.he); got != c.want {
			t.Errorf("%s: isWrongEndpoint(%d, %q) = %v, want %v", c.name, c.he.Status, c.he.Body, got, c.want)
		}
	}
}

func TestIsWrongEndpointResponsesReverse(t *testing.T) {
	if !isWrongEndpointResponses(&zenHTTPError{Status: 400, Body: `{"error":"use /chat/completions instead"}`}) {
		t.Fatal("reverse fingerprint must be detected")
	}
	if isWrongEndpointResponses(&zenHTTPError{Status: 400, Body: `{"error":"max_tokens too large"}`}) {
		t.Fatal("unrelated 400 must not flip back to chat")
	}
	if isWrongEndpointResponses(&zenHTTPError{Status: 502, Body: `use /chat/completions`}) {
		t.Fatal("transient 502 must not flip the learned endpoint")
	}
}

// 错误文本保持原样，日志与调用方对 "zen API <code>" 的可读性不变。
func TestZenHTTPErrorText(t *testing.T) {
	var err error = &zenHTTPError{Status: 502, Body: `{"error":"bad gateway"}`}
	if got, want := err.Error(), `zen API 502: {"error":"bad gateway"}`; got != want {
		t.Fatalf("error text = %q, want %q", got, want)
	}
	// 必须能被 errors.As 解出状态码（调用方仅有 error 接口）
	var he *zenHTTPError
	if !errors.As(fmt.Errorf("wrapped: %w", err), &he) || he.Status != 502 {
		t.Fatal("wrapped zenHTTPError must be recoverable via errors.As")
	}
}

func TestPruneLearnedEndpoints(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("DATA_DIR", tmpDir)

	data := []byte(`{"keep-model": "chat", "prune-model": "responses"}`)
	if err := os.WriteFile(zenEndpointFile(), data, 0600); err != nil {
		t.Fatal(err)
	}

	desired := map[string]bool{"keep-model": true}
	PruneLearnedEndpoints(desired)

	learned := loadZenEndpointsFile()
	if learned["keep-model"] != "chat" {
		t.Errorf("expected keep-model to be preserved, got %q", learned["keep-model"])
	}
	if _, ok := learned["prune-model"]; ok {
		t.Errorf("expected prune-model to be removed")
	}
}
