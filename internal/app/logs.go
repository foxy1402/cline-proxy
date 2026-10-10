package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"cline-go-proxy/internal/kit"
)

// RequestLog 单条代理请求记录（对话/API 调用历史）
type RequestLog struct {
	Time     time.Time `json:"time"`
	Client   string    `json:"client"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Model    string    `json:"model,omitempty"`
	Route    string    `json:"route"` // zen | cline | admin | other
	Status   int       `json:"status"`
	Duration int64     `json:"duration_ms"`
	Note     string    `json:"note,omitempty"`
}

const (
	maxReqLogs = 500
)

var (
	reqLogsMu sync.Mutex
	reqLogs   []RequestLog
)

var reqLogsFile = kit.ResolveDataPath("requests.jsonl")

// AppendReqLog 记录一条请求日志：内存环形保留 + 异步追加落盘。
// LOG_REQUESTS=false 时完全关闭（调用方已跳过，这里兜底）。
func AppendReqLog(l RequestLog) {
	if !LogRequestsEnabled() {
		return
	}
	reqLogsMu.Lock()
	reqLogs = append(reqLogs, l)
	if len(reqLogs) > maxReqLogs {
		reqLogs = reqLogs[len(reqLogs)-maxReqLogs:]
	}
	reqLogsMu.Unlock()
	go func() {
		data, _ := json.Marshal(l)
		f, err := os.OpenFile(reqLogsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		f.Write(append(data, '\n'))
		f.Close()
		// 大小上限（LOG_FILE_MAX_MB，默认 10MB）：超出清空落盘文件（内存仍保留最近 500 条）
		if st, err := os.Stat(reqLogsFile); err == nil && st.Size() > LogFileMaxBytes() {
			os.WriteFile(reqLogsFile, nil, 0600)
		}
	}()
}

// LoadRequestLogs 返回最近的请求日志（内存优先，启动后从落盘文件补载）。
// 必须返回副本：调用方（admin 面板）会原地反转切片，共享底层数组会
// 与 AppendReqLog 的 append 产生数据竞争。
func LoadRequestLogs() []RequestLog {
	reqLogsMu.Lock()
	defer reqLogsMu.Unlock()
	out := make([]RequestLog, len(reqLogs))
	copy(out, reqLogs)
	return out
}

// LoadRequestLogsFromFile 启动时从落盘文件读取尾部记录（日志关闭时跳过）
func LoadRequestLogsFromFile() {
	if !LogRequestsEnabled() {
		return
	}
	raw, err := os.ReadFile(reqLogsFile)
	if err != nil {
		return
	}
	lines := splitLinesSafe(string(raw))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var l RequestLog
		if json.Unmarshal([]byte(line), &l) == nil {
			reqLogs = append(reqLogs, l)
		}
	}
	if len(reqLogs) > maxReqLogs {
		reqLogs = reqLogs[len(reqLogs)-maxReqLogs:]
	}
}

func splitLinesSafe(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// ============ 请求日志中间件 ============

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush 透传底层 Flusher，保证 SSE 流式响应不被中间件吞掉
func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recoverMiddleware 兜底 panic：任何一个 handler 崩溃都只终止该请求（记录
// panic 与堆栈），而不是杀掉整个进程 —— 进程死会同时切断所有在途 SSE 流，
// 影响范围远超单个请求。放在最外层（包住 log 中间件），使日志中间件自身的
// 崩溃也被兜住。若响应尚未写出，返回 500；已写出（流式进行中）则无法再改
// 状态码，只能记录后结束该连接。
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 自持一个 statusWriter 跟踪是否已写出响应头，与本中间件在链上的
		// 位置无关（流式进行中不能再改状态码，只能记录后结束该连接）。
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("PANIC recovered on %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				if sw.status == 0 {
					writeJSON(sw, http.StatusInternalServerError, map[string]any{
						"error": map[string]string{"message": "internal server error", "type": "api_error"},
					})
				}
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

// requestLogMiddleware 记录所有进入代理的请求（API 调用与调用历史）。
// LOG_REQUESTS=false 时跳过日志，但仍包一层 statusWriter —— 它实现了
// http.Flusher，SSE 流式响应依赖它透传 Flush。
// 所有请求体先经 http.MaxBytesReader 限幅（MAX_BODY_MB，默认 32MB），
// 公网匿名请求的超大 body 在读入内存前即被拒绝。
func requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}

		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes())

		logEnabled := LogRequestsEnabled()
		model := ""
		tooLarge := false
		if logEnabled {
			// 读取请求体提取模型，并放回，避免影响后续处理
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				// 超限（或读失败）：直接 413，不进入业务处理。不 return ——
				// 落到下面的统一日志块，让被拒的超大请求也进请求日志
				//（滥用诊断的盲区就是这些 413）。
				tooLarge = true
				writeJSON(sw, http.StatusRequestEntityTooLarge, map[string]any{
					"error": map[string]string{
						"message": fmt.Sprintf("request body too large (limit %d MB)", MaxRequestBodyBytes()>>20),
						"type":    "invalid_request_error",
					},
				})
			} else if len(bodyBytes) > 0 {
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				var probe struct {
					Model string `json:"model"`
				}
				if json.Unmarshal(bodyBytes, &probe) == nil {
					model = probe.Model
				}
			}
		}

		if !tooLarge {
			next.ServeHTTP(sw, r)
		}

		if !logEnabled {
			return
		}
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		route := "other"
		switch {
		case strings.HasPrefix(r.URL.Path, "/admin"):
			route = "admin"
		case strings.HasPrefix(model, "zen/"):
			route = "zen"
		case model != "":
			route = "cline"
		case strings.Contains(r.URL.Path, "models") || strings.Contains(r.URL.Path, "health"):
			route = "meta"
		}
		client := r.RemoteAddr
		if host, _, err := net.SplitHostPort(client); err == nil {
			client = host
		}
		AppendReqLog(RequestLog{
			Time:     time.Now(),
			Client:   client,
			Method:   r.Method,
			Path:     r.URL.Path,
			Model:    model,
			Route:    route,
			Status:   sw.status,
			Duration: time.Since(start).Milliseconds(),
		})
	})
}
