package app

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	// regexToolCallBlock matches <tool_call>...</tool_call>, <tool_call id="...">...</tool_call>, or unclosed at end of text
	regexToolCallBlock = regexp.MustCompile(`(?s)<tool_call(?:[^>]*)>(.*?)(?:</tool_call>|$)`)
	// regexXMLFunction matches <function=NAME>...</function>, <function name="NAME">...</function>, <function:NAME>...</function>
	regexXMLFunction = regexp.MustCompile(`(?s)<(?:function[=:]|function\s+name=["']?|function\s+["']?)([a-zA-Z0-9_\-\.:]+)["']?>(.*?)(?:</function>|$)`)
	// regexParamTag matches parameter opening tags: <parameter=KEY>, <parameter name="KEY">, <arg:KEY>, <argument name="KEY">
	regexParamTag = regexp.MustCompile(`<(?:parameter[=:]|parameter\s+name=["']?|arg[=:]|arg\s+name=["']?|argument\s+name=["']?)([a-zA-Z0-9_\-\.:]+)["']?>`)
	// regexParamCloseTag matches closing tags: </parameter>, </arg>, </argument>, </parameter:KEY>
	regexParamCloseTag = regexp.MustCompile(`(?s)</(?:parameter|arg|argument)(?::[a-zA-Z0-9_\-\.:]+)?>\s*$`)
)

type parsedXMLToolCall struct {
	Name      string
	Arguments string
}

// parseXMLToolCalls extracts any model-generated XML or JSON tool calls inside <tool_call> tags.
func parseXMLToolCalls(text string) []parsedXMLToolCall {
	if !strings.Contains(text, "<tool_call") {
		return nil
	}

	var results []parsedXMLToolCall
	matches := regexToolCallBlock.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		block := strings.TrimSpace(m[1])
		if block == "" {
			continue
		}

		// Shape A: JSON inside <tool_call>
		// e.g. {"name": "...", "arguments": {...}} or {"name": "...", "parameters": {...}}
		if strings.HasPrefix(block, "{") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(block), &obj); err == nil {
				name, _ := obj["name"].(string)
				if name == "" {
					if fn, ok := obj["function"].(map[string]any); ok {
						name, _ = fn["name"].(string)
					}
				}
				if name != "" {
					argsStr := "{}"
					if args, ok := obj["arguments"]; ok {
						if as, ok := args.(string); ok {
							argsStr = chatToolArguments(as)
						} else if ab, err := json.Marshal(args); err == nil {
							argsStr = string(ab)
						}
					} else if params, ok := obj["parameters"]; ok {
						if ps, ok := params.(string); ok {
							argsStr = chatToolArguments(ps)
						} else if pb, err := json.Marshal(params); err == nil {
							argsStr = string(pb)
						}
					}
					results = append(results, parsedXMLToolCall{
						Name:      name,
						Arguments: argsStr,
					})
					continue
				}
			}
		}

		// Shape B: XML <function=NAME><parameter=KEY>VAL</parameter></function>
		// Also supports <function name="NAME"><parameter name="KEY">VAL</parameter></function>
		fnMatches := regexXMLFunction.FindAllStringSubmatch(block, -1)
		for _, fnM := range fnMatches {
			if len(fnM) < 3 {
				continue
			}
			fnName := strings.TrimSpace(fnM[1])
			paramsBody := fnM[2]
			if fnName == "" {
				continue
			}

			paramsMap := extractXMLParameters(paramsBody)
			argsBytes, err := json.Marshal(paramsMap)
			argsStr := "{}"
			if err == nil {
				argsStr = string(argsBytes)
			}

			results = append(results, parsedXMLToolCall{
				Name:      fnName,
				Arguments: argsStr,
			})
		}
	}

	return results
}

// extractXMLParameters tokenizes parameter tags across different model formats
// (<parameter=KEY>, <parameter name="KEY">, <arg:KEY>, etc.)
func extractXMLParameters(body string) map[string]any {
	paramsMap := make(map[string]any)
	matches := regexParamTag.FindAllStringSubmatchIndex(body, -1)
	for i, m := range matches {
		if len(m) < 4 {
			continue
		}
		key := strings.TrimSpace(body[m[2]:m[3]])
		valStart := m[1]
		valEnd := len(body)
		if i+1 < len(matches) {
			valEnd = matches[i+1][0]
		}
		rawVal := strings.TrimSpace(body[valStart:valEnd])
		rawVal = strings.TrimSpace(regexParamCloseTag.ReplaceAllString(rawVal, ""))

		if key != "" {
			paramsMap[key] = coerceXMLParamValue(rawVal)
		}
	}
	return paramsMap
}

// coerceXMLParamValue attempts to parse XML parameter content into proper JSON primitives
// (arrays, objects, booleans, numbers, null), falling back to raw string.
func coerceXMLParamValue(raw string) any {
	if raw == "" {
		return ""
	}
	if raw == "true" {
		return true
	}
	if raw == "false" {
		return false
	}
	if raw == "null" {
		return nil
	}

	// Try JSON unmarshal (covers objects, arrays, numbers, quoted strings)
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v
	}

	// Try numeric conversion for numbers
	if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}

	return raw
}

// stripXMLToolCalls removes <tool_call>...</tool_call> blocks from text and trims whitespace.
func stripXMLToolCalls(text string) string {
	cleaned := regexToolCallBlock.ReplaceAllString(text, "")
	return strings.TrimSpace(cleaned)
}

// repairXMLToolCalls inspects a standard OpenAI chat completion response map,
// detects model-leaked XML tool calls in content or reasoning_content,
// attaches the parsed arguments to tool_calls, and strips the leaked XML.
func repairXMLToolCalls(chat map[string]any) bool {
	if chat == nil {
		return false
	}
	msg, _ := getNested(chat, "choices", 0, "message").(map[string]any)
	if msg == nil {
		return false
	}

	content, _ := msg["content"].(string)
	reasoning, _ := msg["reasoning_content"].(string)

	parsedContent := parseXMLToolCalls(content)
	parsedReasoning := parseXMLToolCalls(reasoning)

	allParsed := append(parsedContent, parsedReasoning...)
	if len(allParsed) == 0 {
		return false
	}

	// Clean XML from content and reasoning
	if len(parsedContent) > 0 {
		msg["content"] = stripXMLToolCalls(content)
	}
	if len(parsedReasoning) > 0 {
		msg["reasoning_content"] = stripXMLToolCalls(reasoning)
	}

	// Attach or update tool_calls
	existingCalls, _ := msg["tool_calls"].([]any)
	updatedCalls := make([]any, 0, len(existingCalls)+len(allParsed))

	usedParsed := make(map[int]bool)

	// First pass: match with existing tool calls that have empty arguments
	for _, tc := range existingCalls {
		tcm, ok := tc.(map[string]any)
		if !ok {
			updatedCalls = append(updatedCalls, tc)
			continue
		}
		fn, ok := tcm["function"].(map[string]any)
		if !ok {
			updatedCalls = append(updatedCalls, tc)
			continue
		}
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)

		// If existing tool call has empty/trivial args ("" or "{}"), see if XML has real args
		if strings.TrimSpace(args) == "" || strings.TrimSpace(args) == "{}" {
			for i, p := range allParsed {
				if !usedParsed[i] && p.Name == name && p.Arguments != "{}" {
					fn["arguments"] = p.Arguments
					usedParsed[i] = true
					break
				}
			}
		}
		updatedCalls = append(updatedCalls, tcm)
	}

	// Second pass: append any parsed tool calls that were not matched to an existing call
	for i, p := range allParsed {
		if !usedParsed[i] {
			callID := fmt.Sprintf("call_%x_%d", time.Now().UnixNano(), i)
			newCall := map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      p.Name,
					"arguments": p.Arguments,
				},
			}
			updatedCalls = append(updatedCalls, newCall)
			usedParsed[i] = true
		}
	}

	msg["tool_calls"] = updatedCalls

	// Ensure finish_reason reflects tool_calls
	if choice, ok := getNested(chat, "choices", 0).(map[string]any); ok {
		choice["finish_reason"] = "tool_calls"
	}

	return true
}
