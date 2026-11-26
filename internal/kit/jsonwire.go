package kit

import (
	"bytes"
	"encoding/json"
	"sort"
)

// MarshalWireJSON 把要发往 zen 上游的 body 序列化成与官方客户端（JS
// JSON.stringify）字节一致的形式。
//
// 与 encoding/json 默认行为的两处差异，都来自 PR #142 的结论
// （jasonxu114514/opencode2api，"match the 2.x client's chat body byte for
// byte"——上游网关会指纹请求体，不止头）：
//
//  1. **不转义 HTML**。encoding/json 默认把 < > & 写成 \u003c \u003e \u0026，
//     而 JSON.stringify 从不转义。客户端 body 里满是 <env>、<system-reminder>
//     和代码片段，逐字符都不同。注意 SetEscapeHTML(false) 必须**递归**生效：
//     顶层 encoder 的设置不会传入对嵌套值的独立 json.Marshal 调用。
//  2. **固定键序**。Go 的 map 序列化按键字母序输出（max_completion_tokens
//     会排在最前），官方客户端（JSON.stringify 按插入序）先发 model，再
//     messages、tools、stream……
//
// 键序策略：顶层 body 先按客户端骨架顺序写已知键，其余键按字母序追加
// （与 opencode2api 的 ordered encoder 同一策略）。嵌套对象无法恢复"插入
// 序"（Go map 无序），保持字母序——上游指纹对顶层顺序敏感，嵌套结构由
// buildZenBody 按 map 字面量构造，字段集合与客户端一致即可。
//
// 非 zen 的调用方（配置落盘、日志、admin API）继续用标准 json.Marshal——
// 那里没有"像客户端"的需求。
//
// 骨架顺序 2026-11-11 Android 客户端 2.2.7 抓包实测（body 恰好 6 个键）：
// model, messages, tools, stream, stream_options, max_completion_tokens。
// responses 路径的 instructions/include 等键不在骨架里，落字母序分支。
var wireKeyOrder = []string{
	"model", "messages", "tools", "stream", "stream_options",
	"max_completion_tokens",
}

// wireEncode 递归编码一个值：string/number/bool/nil 走标准 marshal，数组逐
// 元素递归，map 按键字母序逐键递归。整个调用链共享一个关掉 HTML 转义的
// encoder，保证 <>& 在任意深度都不被转义。
func wireEncode(enc *json.Encoder, buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := wireEncode(enc, buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := wireEncode(enc, buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := wireEncode(enc, buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	default:
		// 标量与其他类型：encoder 已关掉 HTML 转义，直接写入 buf。
		// 注意不能用 json.Marshal（它转义 <>&）。
		// Encode 在每个值后补换行（流式 API 的分隔符），对内嵌标量是损坏
		// 字节；先编码到临时 buffer 再剥掉尾换行。
		var tmp bytes.Buffer
		tenc := json.NewEncoder(&tmp)
		tenc.SetEscapeHTML(false)
		if err := tenc.Encode(v); err != nil {
			return err
		}
		buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
		return nil
	}
}

func MarshalWireJSON(body map[string]any) ([]byte, error) {
	// 顶层键：骨架序在前，剩余按字母序。
	keys := make([]string, 0, len(body))
	seen := make(map[string]bool, len(body))
	for _, k := range wireKeyOrder {
		if _, ok := body[k]; ok {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(body))
	for k := range body {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// 关键差异 1：JSON.stringify 不转义 <>&。这个设置对经 enc.Encode 写出的
	// 所有标量生效（wireEncode 递归全程用它）。
	enc.SetEscapeHTML(false)
	// JSON.stringify 输出无尾换行；Encode 会给标量补 \n，wireEncode 里逐标量
	// TrimRight 去掉。
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		if err := wireEncode(enc, &buf, body[k]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
