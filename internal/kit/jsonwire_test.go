package kit

import (
	"encoding/json"
	"strings"
	"testing"
)

// 2026-11-25：上游 body 的序列化必须与官方客户端（JS JSON.stringify）字节一致。
// 两个差异点都来自 opencode2api PR #142 的结论（网关会指纹请求体，不止头）：
// Go 默认转义 <>& 且 map 键按字母序，客户端两者都不做。

// 不转义 HTML：<>& 必须原样出现在字节里，绝不出现 \u003c 形态。
func TestMarshalWireJSONNoHTMLEscape(t *testing.T) {
	out, err := MarshalWireJSON(map[string]any{
		"model": "big-pickle",
		"messages": []any{map[string]any{
			"role":    "system",
			"content": "<env>\n  Working directory: /tmp/x\n</env>\nUse `a < b && c > d` carefully",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, `\u003c`) || strings.Contains(s, `\u003e`) || strings.Contains(s, `\u0026`) {
		t.Fatalf("wire body HTML-escapes like encoding/json, not like JSON.stringify: %s", s)
	}
	for _, want := range []string{"<env>", "a < b && c > d"} {
		if !strings.Contains(s, want) {
			t.Errorf("wire body lost %q: %s", want, s)
		}
	}
}

// 键序：骨架键按客户端顺序（model 在最前），未知键按字母序追加在尾。
func TestMarshalWireJSONKeyOrder(t *testing.T) {
	out, err := MarshalWireJSON(map[string]any{
		"max_completion_tokens": 32000,
		"stream_options":        map[string]any{"include_usage": true},
		"stream":                true,
		"tools":                 []any{},
		"messages":              []any{},
		"model":                 "big-pickle",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// 顶部骨架顺序：model, messages, tools, stream, stream_options, max_completion_tokens
	idx := func(k string) int { return strings.Index(s, `"`+k+`":`) }
	prev := -1
	for _, k := range wireKeyOrder {
		i := idx(k)
		if i < 0 {
			t.Fatalf("key %q missing from output: %s", k, s)
		}
		if i < prev {
			t.Errorf("key %q appears before the previous skeleton key; wire order broken: %s", k, s)
		}
		prev = i
	}
}

// 未知键（responses 路径的 instructions/include 等）不破坏骨架，且字母序收尾。
func TestMarshalWireJSONUnknownKeysAfterSkeleton(t *testing.T) {
	out, err := MarshalWireJSON(map[string]any{
		"include":      []any{"reasoning.encrypted_content"},
		"model":        "big-pickle",
		"instructions": "sys",
		"input":        []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// 用真正的 JSON 解码保持键序来断言（子串查找不可靠——"input" 会撞上
	// "instructions" 的前缀）。
	dec := json.NewDecoder(strings.NewReader(s))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %v %v", tok, err)
	}
	var order []string
	for dec.More() {
		kt, _ := dec.Token()
		order = append(order, kt.(string))
		if err := skipJSONValue(dec); err != nil {
			t.Fatal(err)
		}
	}
	// 字母序：include < input（c < p）< instructions。
	want := []string{"model", "include", "input", "instructions"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("key order = %v, want %v (skeleton first, then alphabetical)\nraw: %s", order, want, s)
	}
}

// skipJSONValue 跳过一个完整的 JSON 值（对象/数组/标量）。
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); ok {
		switch d {
		case '{':
			for dec.More() {
				if _, err := dec.Token(); err != nil { // key
					return err
				}
				if err := skipJSONValue(dec); err != nil {
					return err
				}
			}
			_, err := dec.Token() // closing }
			return err
		case '[':
			for dec.More() {
				if err := skipJSONValue(dec); err != nil {
					return err
				}
			}
			_, err := dec.Token() // closing ]
			return err
		}
	}
	return nil
}

// 输出必须是合法 JSON 且可反解（结构无损——只动字节形态，不动语义）。
func TestMarshalWireJSONRoundTrip(t *testing.T) {
	in := map[string]any{
		"model":    "big-pickle",
		"messages": []any{map[string]any{"role": "user", "content": "hi <>&"}},
		"stream":   true,
	}
	out, err := MarshalWireJSON(in)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	// 与标准 marshal 语义等价（键序/转义不同，值必须相同）。
	std, _ := json.Marshal(in)
	var stdBack map[string]any
	_ = json.Unmarshal(std, &stdBack)
	if len(back) != len(stdBack) {
		t.Fatalf("round trip lost keys: %v vs %v", back, stdBack)
	}
	if back["model"] != stdBack["model"] {
		t.Errorf("model mismatch: %v vs %v", back["model"], stdBack["model"])
	}
	// 无尾换行（JSON.stringify 不补 \n，Encoder.Encode 会）。
	if strings.HasSuffix(string(out), "\n") {
		t.Error("wire body must not have a trailing newline (JSON.stringify emits none)")
	}
}
