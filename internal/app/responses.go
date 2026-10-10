package app

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"cline-go-proxy/internal/kit"
)

// ============================================================================
// OpenAI Responses API (/v1/responses) -> chat/completions 转换
// 支持 Cursor 等客户端直连反代,无需 opencode CLI
// ============================================================================

// responsesToChat 将 Responses 请求体转换为 chat.completions 请求体
func responsesToChat(body map[string]any) map[string]any {
	out := map[string]any{}
	if m, ok := body["model"].(string); ok {
		out["model"] = m
	}
	if s, ok := body["stream"].(bool); ok {
		out["stream"] = s
	}
	if mt, ok := body["max_output_tokens"].(float64); ok {
		out["max_tokens"] = int(mt)
	}
	for _, k := range []string{"temperature", "top_p", "stop", "seed", "user", "metadata", "logit_bias"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	if instr, ok := body["instructions"].(string); ok && instr != "" {
		out["messages"] = append([]any{map[string]any{"role": "system", "content": instr}}, responsesInputToMessages(body["input"])...)
	} else {
		out["messages"] = responsesInputToMessages(body["input"])
	}
	if tools, ok := body["tools"].([]any); ok {
		out["tools"] = responsesToolsToChat(tools)
	}
	if tc, ok := body["tool_choice"]; ok {
		out["tool_choice"] = responsesToolChoiceToChat(tc)
	}
	return out
}

// responsesToolChoiceToChat Responses 的 tool_choice 函数形式是扁平的
// {"type":"function","name":x}；chat completions 要求嵌套
// {"type":"function","function":{"name":x}}。具名形式必须转换，否则
// 上游 400；其余形态（"auto"/"none"/"required"）原样透传。
func responsesToolChoiceToChat(tc any) any {
	m, ok := tc.(map[string]any)
	if !ok {
		return tc
	}
	if t, _ := m["type"].(string); t == "function" {
		if name, _ := m["name"].(string); name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return tc
}

func responsesInputToMessages(input any) []any {
	var msgs []any
	switch v := input.(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": v})
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// 无 type 字段但带 role+content 的条目是合法的 Responses 输入
			//（OpenAI 同时接受简写形态）；缺失该分支会导致 messages 为空，
			// 上游 400 "specify prompt or messages"
			if m["type"] == nil {
				if role, ok := m["role"].(string); ok && role != "" {
					msgs = append(msgs, map[string]any{"role": role, "content": stringifyResponsesContent(m["content"])})
				}
				continue
			}
			switch m["type"] {
			case "message":
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				msgs = append(msgs, map[string]any{"role": role, "content": stringifyResponsesContent(m["content"])})
			case "function_call":
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				name, _ := m["name"].(string)
				args := ""
				switch a := m["arguments"].(type) {
				case string:
					args = a
				case map[string]any:
					if b, err := json.Marshal(a); err == nil {
						args = string(b)
					}
				}
				msgs = append(msgs, map[string]any{
					"role":       "assistant",
					"content":    "",
					"tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": args}}},
				})
			case "function_call_output":
				callID, _ := m["call_id"].(string)
				if callID == "" {
					callID, _ = m["id"].(string)
				}
				output := ""
				switch o := m["output"].(type) {
				case string:
					output = o
				case map[string]any:
					if b, err := json.Marshal(o); err == nil {
						output = string(b)
					}
				}
				msgs = append(msgs, map[string]any{"role": "tool", "content": output, "tool_call_id": callID})
			case "reasoning":
				// 忽略 Reasoning 输入项(无法映射到 chat 输入)
			}
		}
	}
	return msgs
}

func stringifyResponsesContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := []string{}
		for _, block := range v {
			if b, ok := block.(map[string]any); ok {
				if t, ok := b["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func responsesToolsToChat(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if tm["type"] == "function" {
			fn := map[string]any{}
			if n, ok := tm["name"].(string); ok {
				fn["name"] = n
			}
			if d, ok := tm["description"].(string); ok {
				fn["description"] = d
			}
			if p, ok := tm["parameters"].(map[string]any); ok {
				fn["parameters"] = p
			}
			out = append(out, map[string]any{"type": "function", "function": fn})
		}
	}
	return out
}

// ============ 非流式响应转换 ============

// chatAsSSEResponse 把一个已聚合的 chat completion 结果包成单事件的 chat SSE
// 响应体，供 chatStreamToResponses 复用（原生 responses 模型在 /v1/responses
// 入口需先聚合再按流式重放，避免另写一套 responses 事件生成）。
func chatAsSSEResponse(chat map[string]any) *http.Response {
	model, _ := chat["model"].(string)
	msg, _ := getNested(chat, "choices", 0, "message").(map[string]any)
	if msg == nil {
		msg = map[string]any{}
	}
	finish, _ := getNested(chat, "choices", 0, "finish_reason").(string)
	chunk := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%x", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         msg,
			"finish_reason": finish,
		}},
	}
	if u, ok := chat["usage"]; ok {
		chunk["usage"] = u
	}
	data, _ := json.Marshal(chunk)
	body := "data: " + string(data) + "\n\ndata: [DONE]\n\n"
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// chatToResponses chat.completions 响应 -> Responses 响应
func chatToResponses(chat map[string]any) map[string]any {
	resp := map[string]any{
		"id":          "resp_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
		"object":      "response",
		"created_at":  time.Now().Unix(),
		"status":      "completed",
		"model":       chat["model"],
		"output":      []any{},
		"output_text": "",
	}
	choices, _ := chat["choices"].([]any)
	outputs := []any{}
	var outputText strings.Builder
	if len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			msg, _ := ch["message"].(map[string]any)
			if msg == nil {
				msg, _ = ch["delta"].(map[string]any)
			}
			content := []any{}
			if c, ok := msg["content"].(string); ok && c != "" {
				outputText.WriteString(c)
				content = append(content, map[string]any{"type": "output_text", "text": c, "annotations": []any{}})
			}
			msgOut := map[string]any{
				"type":        "message",
				"id":          "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
				"status":      "completed",
				"role":        "assistant",
				"content":     content,
				"output_text": outputText.String(),
			}
			outputs = append(outputs, msgOut)

			if tc, ok := msg["tool_calls"].([]any); ok {
				for _, c := range tc {
					if cm, ok := c.(map[string]any); ok {
						fn, _ := cm["function"].(map[string]any)
						callID, _ := cm["id"].(string)
						if callID == "" {
							callID = fmt.Sprintf("fc_%x", time.Now().UnixNano())
						}
						name := ""
						args := ""
						if fn != nil {
							name, _ = fn["name"].(string)
							if a, ok := fn["arguments"].(string); ok {
								args = a
							}
						}
						outputs = append(outputs, map[string]any{
							"type":      "function_call",
							"id":        "fc_" + fmt.Sprintf("%x", time.Now().UnixMilli()),
							"call_id":   callID,
							"name":      name,
							"arguments": args,
							"status":    "completed",
						})
					}
				}
			}
		}
	}
	resp["output"] = outputs
	resp["output_text"] = outputText.String()
	if u, ok := chat["usage"].(map[string]any); ok {
		details := map[string]any{}
		if pd, ok := u["prompt_tokens_details"].(map[string]any); ok {
			details["cached_tokens"] = pd["cached_tokens"]
		}
		od := map[string]any{}
		if rd, ok := u["reasoning_tokens"]; ok {
			od["reasoning_tokens"] = rd
		}
		resp["usage"] = map[string]any{
			"input_tokens":          u["prompt_tokens"],
			"input_tokens_details":  details,
			"output_tokens":         u["completion_tokens"],
			"output_tokens_details": od,
			"total_tokens":          u["total_tokens"],
		}
	}
	return resp
}

// ============ 流式响应转换 (Responses SSE) ============

type responsesSSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	msgID   string
	respID  string
}

func newResponsesSSE(w http.ResponseWriter) *responsesSSEWriter {
	f, _ := w.(http.Flusher)
	return &responsesSSEWriter{w: w, flusher: f, msgID: "msg_" + fmt.Sprintf("%x", time.Now().UnixMilli()), respID: "resp_" + fmt.Sprintf("%x", time.Now().UnixMilli())}
}

func (s *responsesSSEWriter) event(event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, string(b))
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// respCall 单个工具调用的流式累积器（按上游 index 区分，支持并行工具调用）
type respCall struct {
	id     string
	name   string
	args   strings.Builder
	outIdx int // 该调用在 Responses 输出序列中的位置；-1 = 尚未发出 added
}

// chatStreamToResponses 将上游 chat.completions SSE 流转换为 Responses SSE 流
func chatStreamToResponses(w http.ResponseWriter, upstream *http.Response, onUsage func(map[string]any)) {
	model := ""
	// 开场
	s := newResponsesSSE(w)
	s.event("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         s.respID,
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "in_progress",
			"model":      "",
			"output":     []any{},
		},
	})
	s.event("response.in_progress", map[string]any{"type": "response.in_progress", "response": map[string]any{"id": s.respID}})

	textEmitted := false
	textOutIdx := 0
	nextOutIdx := 0
	var outText strings.Builder
	// 推理内容独立成 output item：reasoning 常先于正文到达，若挂在文本
	// item 的 index 上，文本 item 尚未 added，客户端收到的是孤儿 delta
	reasoningEmitted := false
	reasoningOutIdx := 0
	var outReasoning strings.Builder
	calls := map[int]*respCall{}
	order := []int{}
	var upUsage map[string]any

	emitCallAdded := func(call *respCall) {
		itemID := "fc_" + call.id
		if call.id == "" {
			itemID = "fc_" + call.name
		}
		call.outIdx = nextOutIdx
		nextOutIdx++
		s.event("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": call.outIdx,
			"item": map[string]any{
				"type":      "function_call",
				"id":        itemID,
				"call_id":   call.id,
				"name":      call.name,
				"arguments": "",
				"status":    "in_progress",
			},
		})
	}

	reader := bufio.NewReader(upstream.Body)
	// 首个非空行若不是 SSE 语法，说明上游以 200 回了 JSON（错误体最常见）。
	// 不判会一路读到底、一个事件都不发，最后发出 response.completed + 空 output，
	// 客户端把上游故障读成"模型回了空内容"。与 collectStreamResponse 同一判据。
	firstNonEmpty := true
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if t := strings.TrimSpace(line); firstNonEmpty && t != "" {
				firstNonEmpty = false
				if !isSSELine(t) {
					rest, _ := io.ReadAll(io.LimitReader(reader, 64<<10))
					body := kit.Truncate(strings.TrimSpace(t+"\n"+string(rest)), 300)
					s.event("response.failed", map[string]any{
						"type": "response.failed",
						"response": map[string]any{
							"id":         s.respID,
							"object":     "response",
							"created_at": time.Now().Unix(),
							"status":     "failed",
							"model":      model,
							"output":     []any{},
							"error":      map[string]any{"code": "upstream_error", "message": "upstream returned non-SSE body: " + body},
						},
					})
					return
				}
			}
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(line[5:])
				if payload == "" || payload == "[DONE]" {
					continue
				}
				var obj map[string]any
				if json.Unmarshal([]byte(payload), &obj) != nil {
					continue
				}
				if data, ok := obj["data"]; ok {
					if d, ok := data.(map[string]any); ok {
						obj = d
					}
				}
				if m, ok := obj["model"].(string); ok && m != "" {
					model = m
				}
				if onUsage != nil {
					if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
						upUsage = u
						onUsage(u)
					}
				}
				choices, _ := obj["choices"].([]any)
				if len(choices) == 0 {
					continue
				}
				ch, _ := choices[0].(map[string]any)
				if ch == nil {
					continue
				}
				delta, _ := ch["delta"].(map[string]any)
				if delta == nil {
					delta = ch
				}
				// 文本
				if c, ok := delta["content"].(string); ok && c != "" {
					if !textEmitted {
						textEmitted = true
						textOutIdx = nextOutIdx
						nextOutIdx++
						s.event("response.output_item.added", map[string]any{
							"type":         "response.output_item.added",
							"output_index": textOutIdx,
							"item":         map[string]any{"id": s.msgID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
						})
						s.event("response.content_part.added", map[string]any{
							"type":          "response.content_part.added",
							"item_id":       s.msgID,
							"output_index":  textOutIdx,
							"content_index": 0,
							"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
						})
					}
					outText.WriteString(c)
					s.event("response.output_text.delta", map[string]any{
						"type":          "response.output_text.delta",
						"item_id":       s.msgID,
						"output_index":  textOutIdx,
						"content_index": 0,
						"delta":         c,
					})
				}
				// 推理
				if r, ok := delta["reasoning_content"].(string); ok && r != "" {
					if !reasoningEmitted {
						reasoningEmitted = true
						reasoningOutIdx = nextOutIdx
						nextOutIdx++
						s.event("response.output_item.added", map[string]any{
							"type":         "response.output_item.added",
							"output_index": reasoningOutIdx,
							"item":         map[string]any{"id": "rs_" + s.msgID, "type": "reasoning", "summary": []any{}},
						})
					}
					outReasoning.WriteString(r)
					s.event("response.reasoning_summary_text.delta", map[string]any{
						"type":          "response.reasoning_summary_text.delta",
						"item_id":       "rs_" + s.msgID,
						"output_index":  reasoningOutIdx,
						"content_index": 0,
						"delta":         r,
					})
				}
				// 工具调用（并行调用按上游 index 各自累积，互不覆盖）
				if tc, ok := delta["tool_calls"].([]any); ok {
					for _, c := range tc {
						cm, ok := c.(map[string]any)
						if !ok {
							continue
						}
						idx := 0
						if i, ok := cm["index"].(float64); ok {
							idx = int(i)
						}
						call := calls[idx]
						if call == nil {
							call = &respCall{outIdx: -1}
							calls[idx] = call
							order = append(order, idx)
						}
						if id, ok := cm["id"].(string); ok && id != "" {
							call.id = id
						}
						fn, _ := cm["function"].(map[string]any)
						if fn != nil {
							if n, ok := fn["name"].(string); ok && n != "" {
								call.name = n
							}
							if call.outIdx < 0 && call.name != "" {
								emitCallAdded(call)
							}
							// 空字符串分片必须整体跳过（否则序列化成字面量 ""
							// 拼进参数，得到坏 JSON），见 collectStreamResponse 同注
							if a, ok := fn["arguments"].(string); ok {
								if a != "" {
									call.args.WriteString(a)
									if call.outIdx >= 0 {
										itemID := "fc_" + call.id
										if call.id == "" {
											itemID = "fc_" + call.name
										}
										s.event("response.function_call_arguments.delta", map[string]any{
											"type":         "response.function_call_arguments.delta",
											"item_id":      itemID,
											"output_index": call.outIdx,
											"delta":        a,
										})
									}
								}
							} else if aRaw, ok := fn["arguments"]; ok && aRaw != nil {
								if b, merr := json.Marshal(aRaw); merr == nil {
									call.args.WriteString(string(b))
								}
							}
						}
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				// 上游流中断：不能谎报 completed（截断内容会被客户端当完整结果）
				s.event("response.failed", map[string]any{
					"type": "response.failed",
					"response": map[string]any{
						"id":         s.respID,
						"object":     "response",
						"created_at": time.Now().Unix(),
						"status":     "failed",
						"model":      model,
						"output":     []any{},
						"error":      map[string]any{"code": "upstream_error", "message": err.Error()},
					},
				})
				return
			}
			break
		}
	}

	// 收尾
	if reasoningEmitted {
		s.event("response.reasoning_summary_text.done", map[string]any{"type": "response.reasoning_summary_text.done", "item_id": "rs_" + s.msgID, "output_index": reasoningOutIdx, "content_index": 0, "text": outReasoning.String()})
		s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": reasoningOutIdx, "item": map[string]any{"id": "rs_" + s.msgID, "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": outReasoning.String()}}}})
	}
	if textEmitted {
		s.event("response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": s.msgID, "output_index": textOutIdx, "content_index": 0, "text": outText.String()})
		s.event("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": s.msgID, "output_index": textOutIdx, "content_index": 0, "part": map[string]any{"type": "output_text", "text": outText.String(), "annotations": []any{}}})
		s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": textOutIdx, "item": map[string]any{"id": s.msgID, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": outText.String(), "annotations": []any{}}}}})
	}
	finalOutput := []any{}
	if reasoningEmitted {
		finalOutput = append(finalOutput, map[string]any{"id": "rs_" + s.msgID, "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": outReasoning.String()}}})
	}
	if textEmitted {
		finalOutput = append(finalOutput, map[string]any{"id": s.msgID, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": outText.String(), "annotations": []any{}}}})
	}
	for _, idx := range order {
		call := calls[idx]
		if call.outIdx < 0 {
			if call.name == "" {
				continue
			}
			emitCallAdded(call)
		}
		// 兜底修复上游偶发的损坏参数后再落事件
		finalArgs := repairToolArguments(call.args.String())
		itemID := "fc_" + call.id
		if call.id == "" {
			itemID = "fc_" + call.name
		}
		s.event("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": itemID, "output_index": call.outIdx, "arguments": finalArgs})
		s.event("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": call.outIdx, "item": map[string]any{"type": "function_call", "id": itemID, "call_id": call.id, "name": call.name, "arguments": finalArgs, "status": "completed"}})
		finalOutput = append(finalOutput, map[string]any{"type": "function_call", "id": itemID, "call_id": call.id, "name": call.name, "arguments": finalArgs, "status": "completed"})
	}
	// usage 用上游真实值 —— 客户端靠它做上下文与用量估算
	usageOut := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	if upUsage != nil {
		if pt, ok := upUsage["prompt_tokens"].(float64); ok {
			usageOut["input_tokens"] = int(pt)
		}
		if ct, ok := upUsage["completion_tokens"].(float64); ok {
			usageOut["output_tokens"] = int(ct)
		}
		usageOut["total_tokens"] = usageOut["input_tokens"].(int) + usageOut["output_tokens"].(int)
	}
	s.event("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":          s.respID,
			"object":      "response",
			"created_at":  time.Now().Unix(),
			"status":      "completed",
			"model":       model,
			"output":      finalOutput,
			"output_text": outText.String(),
			"usage":       usageOut,
		},
	})
}

// ============ /v1/responses 入口 ============

func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error": map[string]string{"message": "method not allowed", "type": "invalid_request_error"},
		})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	var params map[string]any
	if err := json.Unmarshal(body, &params); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	model, _ := params["model"].(string)
	isStream, _ := params["stream"].(bool)
	log.Printf("  responses: model=%s stream=%v", model, isStream)

	chat := responsesToChat(params)
	chatModel, _ := chat["model"].(string)
	// combo 别名模型：改写为平台上游真实模型
	useProxies := clineProxiesEnabled()
	if c := resolveCombo(chatModel); c != nil {
		log.Printf("  responses combo %q -> %s model %q (useProxies=%v)", chatModel, c.Platform, c.Target, c.UseProxies)
		chat["model"] = c.Target
		chatModel = c.Target
		if c.UseProxies {
			useProxies = true
		}
	}
	route := routeModel(chatModel)
	if route == "reject" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": fmt.Sprintf("model %q is a paid zen model; only free zen models are proxied", chatModel), "type": "invalid_request_error"},
		})
		return
	}
	if route == "zen" {
		// 与管理面板的 zen 开关一致：关掉后 /v1/responses 也不得继续往 zen 打
		//（chat 与 messages 两个入口都查了，这里漏查会让开关形同虚设）
		if !getZenConfig().Enabled {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": map[string]string{"message": "zen upstream disabled in /admin/ settings", "type": "api_error"},
			})
			return
		}
		zm, ok := resolveZenFreeModel(chatModel)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]string{"message": fmt.Sprintf("model %q is not a free zen model", chatModel), "type": "invalid_request_error"},
			})
			return
		}
		sid := requestSessionID(chat, r.Header)
		out := maybeCompact(chat, zm, sid)
		if out.changed {
			log.Printf("  responses zen: %s", out.note)
		}
		// 原生 responses 端点模型（muse-spark 等）：上游只认 /responses 协议，
		// 走 chat/completions 会被 400 ModelProtocolUnsupported。此前本入口无条件
		// 调 callZenAPI（chat 端点），这类模型在 /v1/responses 上 100% 失败。
		// 与 chat 入口一致：上游恒 stream=true，先聚合原生 SSE 成 chat 形态，
		// 再用既有的 chat→responses 转换（流式经 chatStreamToResponses 重放）。
		if zm.Upstream == "responses" {
			nresp, _, rerr := callZenResponsesAPI(r.Context(), chat, true)
			if rerr != nil {
				log.Printf("  responses zen native api error: %v", rerr)
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"error": map[string]string{"message": rerr.Error(), "type": "api_error"},
				})
				return
			}
			agg, aerr := responsesSSEToChat(nresp)
			if aerr != nil {
				log.Printf("  responses zen native aggregate error: %v", aerr)
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"error": map[string]string{"message": aerr.Error(), "type": "api_error"},
				})
				return
			}
			agg["model"] = zm.ID
			if isStream {
				// 复用 chat SSE→responses 转换：把聚合结果包成一条 chat SSE 流喂进去。
				// 原生流此前已完整读出，这里不再有增量；对客户端仍按 responses
				// 事件序列下发（文本/推理/工具调用/usage 均经既有转换路径）。
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.WriteHeader(http.StatusOK)
				chatStreamToResponses(w, chatAsSSEResponse(agg), nil)
				return
			}
			writeJSON(w, http.StatusOK, chatToResponses(normalizeOpenAIResponse(agg)))
			return
		}
		// 上游恒 stream=true：zen 免费层 chat 端点只接受 CLI 形态的流式请求，
		// stream=false 会被 FreeTier gate 直接 403。非流式客户端在这里把 SSE
		// 聚合回 JSON（原先透传 isStream 会让这类请求必 403）。
		resp, _, err := callZenAPI(r.Context(), chat, true)
		if err != nil {
			log.Printf("  responses zen api error: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]string{"message": err.Error(), "type": "api_error"},
			})
			return
		}
		defer resp.Body.Close()
		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(http.StatusOK)
			chatStreamToResponses(w, resp, nil)
			return
		}
		raw, cerr := collectStreamResponse(resp)
		if cerr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": map[string]string{"message": cerr.Error(), "type": "api_error"},
			})
			return
		}
		writeJSON(w, http.StatusOK, chatToResponses(normalizeOpenAIResponse(raw)))
		return
	}

	// cline 上游
	// 未知模型名显式拒绝（与 chat 路径一致），不做静默替换
	if msg := strictModelGate(chatModel); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": msg, "type": "invalid_request_error"},
		})
		return
	}
	stream := isStream
	if !isStream && modelNeedsStream(normalizeRequestModel(chatModel)) {
		stream = true
	}
	up, acc, streamed, err := callClineAutoStream(r.Context(), chat, stream, useProxies)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "api_error"},
		})
		return
	}
	defer up.Body.Close()

	// 自学习重试过：上游那份是 SSE，即便客户端要的是非流式也要走聚合
	stream = stream || streamed

	usageFn := accountUsageFn(acc, chat)
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		chatStreamToResponses(w, up, usageFn)
		return
	}
	if stream {
		out, err := collectStreamResponse(up)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if u, ok := out["usage"].(map[string]any); ok && len(u) > 0 {
			usageFn(u)
		}
		writeJSON(w, http.StatusOK, chatToResponses(out))
		return
	}
	var raw map[string]any
	if err := json.NewDecoder(up.Body).Decode(&raw); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if u, ok := raw["usage"].(map[string]any); ok && len(u) > 0 {
		usageFn(u)
	}
	out := raw
	if data, ok := raw["data"]; ok {
		if d, ok := data.(map[string]any); ok {
			out = d
		}
	}
	// 与 zen 路径一致：先归一化再转换，剥掉上游私有字段并保证 choices 结构
	writeJSON(w, http.StatusOK, chatToResponses(normalizeOpenAIResponse(out)))
}
