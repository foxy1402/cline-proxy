package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseXMLToolCallsUserEvidenceCase(t *testing.T) {
	raw := `<tool_call><function=pdf-reader_search_pdf><parameter=context_chars>60</parameter><parameter=max_matches_per_source>10</parameter><parameter=query>Figure 1</parameter><parameter=sources>[{"path": "04_documentacion/referencias/AComparisonOfMachineLearningModelsForPredictingRainfallInRubanMetropolitanCities.pdf"}]</parameter><parameter=prefer_speed>true</parameter></function></tool_call>`

	calls := parseXMLToolCalls(raw)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "pdf-reader_search_pdf" {
		t.Errorf("expected name pdf-reader_search_pdf, got %s", calls[0].Name)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("failed to unmarshal parsed arguments: %v", err)
	}

	if q, ok := args["query"].(string); !ok || q != "Figure 1" {
		t.Errorf("expected query 'Figure 1', got %v", args["query"])
	}
	if ps, ok := args["prefer_speed"].(bool); !ok || !ps {
		t.Errorf("expected prefer_speed true, got %v", args["prefer_speed"])
	}
	if cc, ok := args["context_chars"].(float64); !ok || cc != 60 {
		t.Errorf("expected context_chars 60, got %v", args["context_chars"])
	}
	if mm, ok := args["max_matches_per_source"].(float64); !ok || mm != 10 {
		t.Errorf("expected max_matches_per_source 10, got %v", args["max_matches_per_source"])
	}
	sources, ok := args["sources"].([]any)
	if !ok || len(sources) != 1 {
		t.Fatalf("expected sources array of len 1, got %v", args["sources"])
	}
	sm, _ := sources[0].(map[string]any)
	if sm["path"] != "04_documentacion/referencias/AComparisonOfMachineLearningModelsForPredictingRainfallInRubanMetropolitanCities.pdf" {
		t.Errorf("unexpected path: %v", sm["path"])
	}
}

func TestParseXMLToolCallsJSONVariant(t *testing.T) {
	raw := `<tool_call>
{"name": "get_weather", "arguments": {"city": "Tokyo", "unit": "celsius"}}
</tool_call>`

	calls := parseXMLToolCalls(raw)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "get_weather" {
		t.Errorf("expected get_weather, got %s", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, "Tokyo") {
		t.Errorf("expected Tokyo in arguments, got %s", calls[0].Arguments)
	}
}

func TestParseXMLToolCallsMultiModelVariants(t *testing.T) {
	// Format with attributes: <function name="..."> <parameter name="...">
	rawAttr := `<tool_call id="call_1"><function name="qgis_buffer"><parameter name="distance">50.5</parameter><parameter name="dissolve">false</parameter></function></tool_call>`
	calls := parseXMLToolCalls(rawAttr)
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Name != "qgis_buffer" {
		t.Errorf("expected qgis_buffer, got %s", calls[0].Name)
	}
	if !strings.Contains(calls[0].Arguments, "50.5") || !strings.Contains(calls[0].Arguments, "false") {
		t.Errorf("expected parsed params, got %s", calls[0].Arguments)
	}

	// Format with colon: <function:calc> <arg:x>
	rawColon := `<tool_call><function:calc><arg:x>100</arg:x><arg:y>200</arg:y></function></tool_call>`
	calls2 := parseXMLToolCalls(rawColon)
	if len(calls2) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls2))
	}
	if calls2[0].Name != "calc" {
		t.Errorf("expected calc, got %s", calls2[0].Name)
	}
	if !strings.Contains(calls2[0].Arguments, "100") {
		t.Errorf("expected 100 in arguments, got %s", calls2[0].Arguments)
	}
}

func TestRepairXMLToolCallsInChat(t *testing.T) {
	chat := map[string]any{
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role": "assistant",
					"content": "Let me search the PDF:\n<tool_call><function=pdf-reader_search_pdf><parameter=query>Figure 1</parameter><parameter=sources>[{\"path\": \"doc.pdf\"}]</parameter><parameter=prefer_speed>true</parameter></function></tool_call>",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_existing_123",
							"type": "function",
							"function": map[string]any{
								"name":      "pdf-reader_search_pdf",
								"arguments": "{}",
							},
						},
					},
				},
			},
		},
	}

	repaired := repairXMLToolCalls(chat)
	if !repaired {
		t.Fatalf("expected repair to return true")
	}

	choice := chat["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("expected finish_reason tool_calls, got %v", choice["finish_reason"])
	}

	msg := choice["message"].(map[string]any)
	if msg["content"] != "Let me search the PDF:" {
		t.Errorf("expected stripped content, got %q", msg["content"])
	}

	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_existing_123" {
		t.Errorf("expected preserved id call_existing_123, got %v", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	argsStr := fn["arguments"].(string)
	if !strings.Contains(argsStr, "Figure 1") || !strings.Contains(argsStr, "doc.pdf") {
		t.Errorf("expected updated arguments, got %s", argsStr)
	}
}

func TestRepairXMLToolCallsWhenNoExistingToolCall(t *testing.T) {
	chat := map[string]any{
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role":    "assistant",
					"content": "<tool_call><function=calculate><parameter=expr>2+2</parameter></function></tool_call>",
				},
			},
		},
	}

	repaired := repairXMLToolCalls(chat)
	if !repaired {
		t.Fatalf("expected repair to return true")
	}

	choice := chat["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("expected finish_reason tool_calls, got %v", choice["finish_reason"])
	}

	msg := choice["message"].(map[string]any)
	if msg["content"] != "" {
		t.Errorf("expected empty content, got %q", msg["content"])
	}

	tcs := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 generated tool call, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	if fn["name"] != "calculate" {
		t.Errorf("expected calculate, got %v", fn["name"])
	}
	if !strings.Contains(fn["arguments"].(string), "2+2") {
		t.Errorf("expected 2+2 in arguments, got %v", fn["arguments"])
	}
}

func TestCollectStreamResponseRepairsXMLToolCalls(t *testing.T) {
	// Simulate an upstream SSE stream that emits:
	// 1. A tool_call chunk with empty arguments
	// 2. A content chunk with leaked XML <tool_call><function=...><parameter=...></function></tool_call>
	// 3. finish_reason: tool_calls
	sseData := `data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Thinking about query..."}}]}

data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_test_123","type":"function","function":{"name":"pdf-reader_search_pdf","arguments":""}}]}}]}

data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"<tool_call><function=pdf-reader_search_pdf><parameter=context_chars>60</parameter><parameter=query>Figure 1</parameter><parameter=sources>[{\"path\":\"doc.pdf\"}]</parameter><parameter=prefer_speed>true</parameter></function></tool_call>"}}]}

data: {"id":"chatcmpl-1","choices":[{"index":0,"finish_reason":"tool_calls"}]}

data: [DONE]

`
	resp := sseResp(sseData)
	out, err := collectStreamResponse(resp)
	if err != nil {
		t.Fatalf("collectStreamResponse failed: %v", err)
	}

	msg, ok := getNested(out, "choices", 0, "message").(map[string]any)
	if !ok {
		t.Fatalf("missing message in output")
	}

	// Verify content was stripped of XML
	if c, _ := msg["content"].(string); c != "" {
		t.Errorf("expected empty content after XML strip, got %q", c)
	}

	// Verify reasoning_content was preserved
	if rc, _ := msg["reasoning_content"].(string); !strings.Contains(rc, "Thinking about query") {
		t.Errorf("expected reasoning_content preserved, got %q", rc)
	}

	// Verify tool_calls has repaired arguments
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(tcs))
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_test_123" {
		t.Errorf("expected id call_test_123, got %v", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	args := fn["arguments"].(string)
	if !strings.Contains(args, "Figure 1") || !strings.Contains(args, "prefer_speed") {
		t.Errorf("expected repaired arguments, got %s", args)
	}
}
