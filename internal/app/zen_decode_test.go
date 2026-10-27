package app

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cline-go-proxy/internal/kit"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// 2026-10-27 回归：setZenGateHeaders 显式设置 Accept-Encoding，Go 的
// 自动解压随之失效（net/http 仅在客户端未设置该头时解压）。上游对大响应
// 会压（实测 models 目录返回 Content-Encoding: br），解压必须由网关完成。
func TestCompressedUpstreamBodyIsDecoded(t *testing.T) {
	want := `{"type":"error","error":{"type":"FreeUsageLimitError","message":"limit"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write([]byte(want))
		gz.Close()
	}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader("{}"))
	setZenGateHeaders(req, "ses_0123456789abAAAAAAAAAAAAAA", "msg_0123456789ab")
	if req.Header.Get("Accept-Encoding") == "" {
		t.Fatal("precondition: gateway must advertise Accept-Encoding")
	}

	resp, err := (&http.Client{Transport: &http.Transport{}}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if got := kit.ReadBody(resp); got != want {
		t.Fatalf("ReadBody returned undecoded/incorrect body:\n got %q\nwant %q", got, want)
	}
}

// 流式路径直接读 resp.Body，因此解码必须在拿到响应后立刻套上，
// 不能只依赖 ReadBody。
func TestCompressedStreamDecodedForSSE(t *testing.T) {
	payload := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write([]byte(payload))
		gz.Close()
	}))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if err := kit.DecodeResponse(resp); err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "data:") {
		t.Fatalf("SSE prefixes unreadable after transport decode: %q", string(body))
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Error("Content-Encoding must be cleared after decoding")
	}
	if resp.Header.Get("Content-Length") != "" {
		t.Error("Content-Length must be cleared after decoding (no longer accurate)")
	}
}

// 未压缩响应不能被动过；未知编码保持原样（fail-open，不比现状更差）。
func TestDecodeResponseIdentityAndUnknown(t *testing.T) {
	for _, enc := range []string{"", "identity", "snappy"} {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Encoding", enc)
		rec.WriteString("plain")
		resp := rec.Result()
		if err := kit.DecodeResponse(resp); err != nil {
			t.Fatalf("enc=%q: %v", enc, err)
		}
		got, _ := io.ReadAll(resp.Body)
		if string(got) != "plain" {
			t.Fatalf("enc=%q body = %q, want unchanged", enc, got)
		}
	}
}

// br 与 zstd 是上游实际会用的编码（models 目录实测 Content-Encoding: br），
// 用离线合成的压缩体覆盖，避免依赖网络。
func TestDecodeResponseBrotliAndZstd(t *testing.T) {
	want := `{"ok":true,"msg":"` + strings.Repeat("x", 4096) + `"}`

	t.Run("br", func(t *testing.T) {
		var buf bytes.Buffer
		bw := brotli.NewWriter(&buf)
		bw.Write([]byte(want))
		bw.Close()

		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Encoding", "br")
		rec.Write(buf.Bytes())
		resp := rec.Result()
		if err := kit.DecodeResponse(resp); err != nil {
			t.Fatalf("decode br: %v", err)
		}
		got, _ := io.ReadAll(resp.Body)
		if string(got) != want {
			t.Fatalf("br roundtrip mismatch (got %d bytes, want %d)", len(got), len(want))
		}
	})

	t.Run("zstd", func(t *testing.T) {
		var buf bytes.Buffer
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatalf("zstd writer: %v", err)
		}
		zw.Write([]byte(want))
		zw.Close()

		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Encoding", "zstd")
		rec.Write(buf.Bytes())
		resp := rec.Result()
		if err := kit.DecodeResponse(resp); err != nil {
			t.Fatalf("decode zstd: %v", err)
		}
		got, _ := io.ReadAll(resp.Body)
		if string(got) != want {
			t.Fatalf("zstd roundtrip mismatch (got %d bytes, want %d)", len(got), len(want))
		}
	})
}
