package kit

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

var ExecCommand = exec.Command

var HTTPTransport = &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 10,
	IdleConnTimeout:     90 * time.Second,
	DisableCompression:  false,
}

var HTTPClient = &http.Client{
	Transport: HTTPTransport,
}

// JSONHTTPClient 短 JSON 请求（认证/设备登录/管理探测）专用：整体 60s 超时，
// 防止上游吞连接后调用方永久挂起（启动预热的 seedAccountsFromFile 就会被卡住）。
// 不要用于流式转发 —— 那些请求生命周期由 ctx 控制，可能远超 60s。
var JSONHTTPClient = &http.Client{
	Transport: HTTPTransport,
	Timeout:   60 * time.Second,
}

func HTTPPostForm(rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequest("POST", rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return JSONHTTPClient.Do(req)
}

func HTTPPostJSON(rawURL string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", rawURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return JSONHTTPClient.Do(req)
}

// DecodeResponse 按 Content-Encoding 给 resp.Body 套上解码器，并删除
// Content-Encoding / Content-Length 头（长度已不代表解码后的体）。
//
// 为什么必需：任何显式设置 Accept-Encoding 的请求都会让 net/http 的
// 自动解压失效（它只在客户端未设置该头时才解压）。zen 上游配了
// "gzip, deflate, br, zstd" 来对齐 CLI 指纹，因此必须自己解压，否则
// 压缩响应会被当成明文交给 JSON/SSE 解析器（表现为"乱码"或
// "non-SSE body"）。小响应通常未压缩，大响应才会压 —— 也就是说
// 越正常的请求越容易踩到。
//
// 未知/不支持的编码保持原样（fail-open）：调用方读到的仍是上游字节，
// 与未加本函数时的行为一致，不会比现在更差。
func DecodeResponse(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return nil
	}
	// 多编码按应用顺序叠放，解码需逆序。
	parts := strings.Split(enc, ",")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	body := resp.Body
	for _, p := range parts {
		p = strings.TrimSpace(p)
		var err error
		switch p {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			var zr *gzip.Reader
			if zr, err = gzip.NewReader(body); err == nil {
				body = zr
			}
		case "deflate":
			// 历史上 deflate 既可能是 zlib 也可能是裸 DEFLATE，先试 zlib。
			if zr, zerr := zlib.NewReader(body); zerr == nil {
				body = zr
			} else {
				body = flate.NewReader(body)
			}
		case "br":
			body = &readCloser{Reader: brotli.NewReader(body), closer: body}
		case "zstd":
			var zr *zstd.Decoder
			if zr, err = zstd.NewReader(body); err == nil {
				body = zr.IOReadCloser()
			}
		default:
			// 未知编码：不解码，保持原样。
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode %s response: %w", p, err)
		}
	}
	resp.Body = &decodedBody{inner: body}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	return nil
}

// decodedBody 同时关闭解码器与底层连接。
type decodedBody struct {
	inner io.ReadCloser
}

func (d *decodedBody) Read(p []byte) (int, error) { return d.inner.Read(p) }
func (d *decodedBody) Close() error              { return d.inner.Close() }

// readCloser 给只有 Read 的解码器（brotli）补上 Close（关底层连接）。
type readCloser struct {
	io.Reader
	closer io.Closer
}

func (r *readCloser) Close() error { return r.closer.Close() }

// ReadBody 读取响应体用于日志/报错展示。上限 1MB：这些路径只做
// 截断展示，一个恶意/异常上游返回 GB 级 body 时不能拖垮进程内存。
// 会先按 Content-Encoding 解码，避免压缩体被当成文本解析。
func ReadBody(resp *http.Response) string {
	if err := DecodeResponse(resp); err != nil {
		return fmt.Sprintf("<decode error: %v>", err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Sprintf("<read error: %v>", err)
	}
	return string(data)
}

func Truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func RunCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Start()
}
