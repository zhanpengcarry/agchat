package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppendAndJSONL 每条一行、可解析、重启后仍在（追加而不是截断）。
func TestAppendAndJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	w, err := Open(path, false)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	for _, v := range []string{"cross_tenant_attempt", "unknown_token"} {
		b, _ := json.Marshal(map[string]string{"verdict": v})
		if _, err := w.Write(append(b, '\n')); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// 再"启动"一次，应该追加而不是覆盖
	w2, err := Open(path, true)
	if err != nil {
		t.Fatalf("二次打开失败: %v", err)
	}
	b, _ := json.Marshal(map[string]string{"verdict": "cross_room_attempt"})
	if _, err := w2.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	_ = w2.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("应有 3 行（追加而非覆盖），实际 %d 行：%s", len(lines), raw)
	}
	// 每行都是合法 JSON
	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %s", i+1, line)
		}
		if m["verdict"] == nil {
			t.Fatalf("第 %d 行缺 verdict: %s", i+1, line)
		}
	}
}

// TestOpenFailureIsFatal 打不开必须返回 error（让启动失败），不能静默降级。
func TestOpenFailureIsFatal(t *testing.T) {
	cases := []string{
		filepath.Join(t.TempDir(), "no_such_dir", "audit.jsonl"), // 目录不存在
		t.TempDir(), // 是个目录，不是文件
		"",
	}
	for _, p := range cases {
		if _, err := Open(p, false); err == nil {
			t.Fatalf("路径 %q 应当打开失败", p)
		}
	}
}
