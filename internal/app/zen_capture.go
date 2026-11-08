package app

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// zen 入站请求抓包（排障用，默认关闭）
//
// 目的：当"IDE 发一句话就整天 429"这类问题时，必须看到客户端**实际**
// 发了什么——完整 system prompt、工具 schema 数量与体积、消息条数。
// 只看网关日志无法区分"内容被扫描"与"体积吃光额度"。
//
// 开启：ZEN_CAPTURE_DIR=/app/data/capture（容器内路径）。默认空 = 完全不写盘。
// 每次 zen 请求落一个 JSON：客户端原始 params + 网关真正发往上游的 body。
//
// 安全约定：
//   - Authorization 头一律打码；配置里的所有 key 在落盘前全文替换。
//   - 只写本地目录，不上传、不进普通日志。
//   - 目录属于排障产物，用完应删除（里面是完整的会话内容）。
// ============================================================================

// zenCaptureMaxBytes 单个抓包文件的上限：超过则截断并标记。
// 一个 64 工具的 IDE 请求约 50KB，正常远达不到；设上限只为防止
// 异常大请求把磁盘写满。
const zenCaptureMaxBytes = 32 << 20

var (
	zenCaptureSeq int64
	zenCaptureMu  sync.Mutex
)

func zenCaptureDir() string {
	return strings.TrimSpace(os.Getenv("ZEN_CAPTURE_DIR"))
}

func zenCaptureEnabled() bool { return zenCaptureDir() != "" }

// redactZenSecrets 把所有配置中的 key 从字符串里抹掉。
// 抓包内容来自客户端，理论上不含 key，但 Authorization 头与可能的
// 客户端回显都可能带上——宁可多替换一次。
func redactZenSecrets(s string) string {
	for _, k := range getZenConfig().Keys {
		if len(k) >= 8 {
			s = strings.ReplaceAll(s, k, "<zen-key>")
		}
	}
	return s
}

type zenCaptureRecord struct {
	TS             int64             `json:"ts"`
	TSHuman        string            `json:"tsHuman"`
	Seq            int64             `json:"seq"`
	Path           string            `json:"path"`
	Model          string            `json:"model"`
	Stream         bool              `json:"stream"`
	MessageCount   int               `json:"messageCount"`
	ToolCount      int               `json:"toolCount"`
	ToolBytes      int               `json:"toolBytes"`
	UpstreamTool   int               `json:"upstreamToolCount"`
	SystemChars    int               `json:"systemPromptChars"`
	SystemPrompt   string            `json:"systemPrompt"`
	Headers        map[string]string `json:"headers"`
	ClientParams   json.RawMessage   `json:"clientParams,omitempty"`
	UpstreamBody   json.RawMessage   `json:"upstreamBody,omitempty"`
	ClientBodySize int               `json:"clientBodyBytes"`
	UpstreamSize   int               `json:"upstreamBodyBytes"`
	Truncated      bool              `json:"truncated,omitempty"`
	Note           string            `json:"note,omitempty"`
}

// captureZenRequest 落一个抓包文件。params 为客户端原始请求（压缩后），
// upstream 为网关构造出的上游 body（nil 表示该路径不复用 chat body）。
//
// 失败只记一行日志，绝不影响请求本身——抓包是排障工具，不是业务路径。
func captureZenRequest(label string, params map[string]any, h http.Header) {
	dir := zenCaptureDir()
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Printf("  zen capture: mkdir %s failed: %v", dir, err)
		return
	}

	rec := zenCaptureRecord{
		TS:      time.Now().UnixMilli(),
		TSHuman: time.Now().UTC().Format(time.RFC3339),
		Path:    label,
		Headers: map[string]string{},
	}
	// 请求头：凭据类一律打码，其余原样（UA / session 头对门禁排查有用）。
	//
	// 必须覆盖 x-api-key：网关自己就是用 x-api-key 认证 /v1 客户端的
	//（Authorization: Bearer 是第二选择）。漏掉它，抓包目录里就躺着调用方的
	// 有效网关密钥——而抓包正是"复现问题时长期开着、事后容易忘记删"的功能。
	// 判据用"名字里带 key/auth/token/secret"的宽匹配，宁可多打码。
	for k, v := range h {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "cookie" || lk == "proxy-authorization" ||
			lk == "x-api-key" || lk == "api-key" || strings.Contains(lk, "auth") ||
			strings.Contains(lk, "token") || strings.Contains(lk, "secret") ||
			strings.Contains(lk, "cookie") {
			rec.Headers[k] = "<redacted>"
			continue
		}
		rec.Headers[k] = strings.Join(v, ", ")
	}

	model, _ := params["model"].(string)
	rec.Model = model
	rec.Stream, _ = params["stream"].(bool)

	if msgs, ok := params["messages"].([]any); ok {
		rec.MessageCount = len(msgs)
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := mm["role"].(string); role == "system" || role == "developer" {
				s := zenContentText(mm["content"])
				if s != "" {
					rec.SystemChars += len(s)
					rec.SystemPrompt += s + "\n\n--8<--\n\n"
				}
			}
		}
	}
	if rec.SystemPrompt == "" {
		if ins, ok := params["instructions"].(string); ok && ins != "" {
			rec.SystemChars = len(ins)
			rec.SystemPrompt = ins
		}
	}
	rec.SystemPrompt = redactZenSecrets(rec.SystemPrompt)

	if ts, ok := params["tools"].([]any); ok {
		rec.ToolCount = len(ts)
		if b, err := json.Marshal(ts); err == nil {
			rec.ToolBytes = len(b)
		}
	}

	if b, err := json.Marshal(params); err == nil {
		rec.ClientBodySize = len(b)
		if len(b) <= zenCaptureMaxBytes {
			rec.ClientParams = redactJSON(b, params)
		} else {
			rec.Truncated = true
		}
	}

	// 上游形态：与真实发送完全一致的构造（含 12 个 gate 工具）。
	if body := buildZenBody(params, true); body != nil {
		if b, err := json.Marshal(body); err == nil {
			rec.UpstreamSize = len(b)
			if t, ok := body["tools"].([]any); ok {
				rec.UpstreamTool = len(t)
			}
			if len(b) <= zenCaptureMaxBytes {
				rec.UpstreamBody = redactJSON(b, params)
			} else {
				rec.Truncated = true
			}
		}
	}

	seq := atomic.AddInt64(&zenCaptureSeq, 1)
	rec.Seq = seq
	name := filepath.Join(dir, fmt.Sprintf("%s-%04d.json", time.Now().UTC().Format("20060102T150405Z"), seq))

	zenCaptureMu.Lock()
	defer zenCaptureMu.Unlock()
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		log.Printf("  zen capture: open %s failed: %v", name, err)
		return
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", " ")
	if err := enc.Encode(rec); err != nil {
		log.Printf("  zen capture: write %s failed: %v", name, err)
		return
	}
	log.Printf("  zen capture: %s (path=%s msgs=%d tools=%d sysChars=%d clientBytes=%d upstreamBytes=%d)",
		filepath.Base(name), label, rec.MessageCount, rec.ToolCount, rec.SystemChars,
		rec.ClientBodySize, rec.UpstreamSize)
}

// redactJSON 返回打码后的原始字节（替换 key 文本，但保持 JSON 可解析）。
func redactJSON(b []byte, params map[string]any) json.RawMessage {
	s := redactZenSecrets(string(b))
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	// 替换破坏了 JSON（key 中含转义字符等极端情况）：退回不落原文，
	// 只留一个结构化摘要，避免写出半个坏 JSON。
	sum, _ := json.Marshal(map[string]any{"_unparsable": true, "bytes": len(b)})
	return json.RawMessage(sum)
}
