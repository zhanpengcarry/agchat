package oplog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CanonicalJSON 是全域唯一一份规范编码（arch A-25 / §10.2⑦）：
//
//   - 对象成员按键**升序**输出（字节序，不是 locale 序）；
//   - 无多余空格（分隔符后不写空格）；
//   - **null 一律拒绝**（"无 null"：缺省字段由 omitempty 表达）；
//   - **浮点一律拒绝**（float 在两次序列化之间会变字节）。
//
// 也就是说：载荷里只许出现 string / bool / 整数 / 数组 / 对象。谁要传浮点，先自己
// 定标成整数（例如金额用分、比率用万分比）。
func CanonicalJSON(v any) ([]byte, error) {
	if v == nil {
		return nil, fmt.Errorf("%w: nil 不能做规范编码的输入", ErrBadJSON)
	}
	switch t := v.(type) {
	case json.RawMessage:
		if len(bytes.TrimSpace(t)) == 0 {
			return nil, fmt.Errorf("%w: 空 json.RawMessage", ErrBadJSON)
		}
		return canonicalize(t)
	case []byte:
		if len(bytes.TrimSpace(t)) == 0 {
			return nil, fmt.Errorf("%w: 空 JSON 字节", ErrBadJSON)
		}
		return canonicalize(t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	return canonicalize(b)
}

// canonicalize 把任意合法 JSON 重新编码成规范形式。
func canonicalize(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	// 尾部多余的 token 说明输入不是一个值（例如两行 jsonl 拼在一起）。
	if err := dec.Decode(new(json.RawMessage)); err == nil {
		return nil, fmt.Errorf("%w: JSON 尾部有多余内容", ErrBadJSON)
	}
	var buf bytes.Buffer
	buf.Grow(len(raw))
	if err := writeCanonical(&buf, x); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeCanonical 递归输出规范编码。路径用于错误定位。
func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		return fmt.Errorf("%w: 禁止 null（缺省字段用 omitempty 表达）", ErrBadJSON)
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case string:
		return writeCanonicalString(buf, t)
	case json.Number:
		lit := t.String()
		if strings.ContainsAny(lit, ".eE") {
			return fmt.Errorf("%w: 禁止浮点 %q（请先定标成整数）", ErrBadJSON, lit)
		}
		buf.WriteString(lit)
		return nil
	case []any:
		buf.WriteByte('[')
		for i, el := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, el); err != nil {
				return fmt.Errorf("%w (数组下标 %d)", err, i)
			}
		}
		buf.WriteByte(']')
		return nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		first := true
		for _, k := range keys {
			val := t[k]
			if val == nil {
				// 对象成员为 null：直接拒，不静默丢弃——丢弃会让"谁把字段写成 null"
				// 在证据链上不可见（§10.2⑦）。
				return fmt.Errorf("%w: 字段 %q 为 null", ErrBadJSON, k)
			}
			if !first {
				buf.WriteByte(',')
			}
			first = false
			if err := writeCanonicalString(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, val); err != nil {
				return fmt.Errorf("%w (字段 %q)", err, k)
			}
		}
		buf.WriteByte('}')
		return nil
	default:
		return fmt.Errorf("%w: 不支持的类型 %T", ErrBadJSON, v)
	}
}

// writeCanonicalString 输出 JSON 字符串，但不做 HTML 转义（默认的 < > & 转义会
// 让同一段文本产生两种字节形态，规范化就失去意义）。
func writeCanonicalString(buf *bytes.Buffer, s string) error {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
	return nil
}
