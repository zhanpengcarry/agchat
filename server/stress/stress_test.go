//go:build stress

// Package stress 复现 P0："长时间跑会后进程自杀"。
//
// 形态刻意对齐崩溃现场：**6 人 × 10 轮满场**、每场 3 个 agent 挂 20s 长轮询、
// 会议会走完 投票 → summarizing 等概述超时 这条长路径，连续 20 场单进程。
//
// ★ 关键：被测进程的 stdout+stderr 必须接到 buffer。
//
//	进程消失但不知道为什么，就是因为栈被吞了 —— 这里任一 exit 都把尾巴打出来。
package stress

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	meetings      = 20
	agentsPerRoom = 6
	roundsPerRoom = 10
	longPollers   = 3
)

func findBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("AGCHAT_BIN"); p != "" {
		return p
	}
	name := "agchatd"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, c := range []string{
		filepath.Join("..", ".bin", name),
		filepath.Join("..", "bin", name),
		filepath.Join("..", "cmd", "agchatd", name),
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	// ★ 找不到就现编一个，而不是 Skip。
	//
	//   原来这里 `t.Skipf("找不到 %s")`：全新 clone 里压测会**悄悄跳过并显示 PASS**
	//   （实测 2.6 秒"通过"）。这正是本项目自己写下的教训——"只看绿灯，绿灯可能毫无意义"。
	//   压测是发版前的门槛，找不到被测进程就该是红的，不该是绿的。
	dst := filepath.Join(os.TempDir(), fmt.Sprintf("agchatd-stress-build-%d", os.Getpid()))
	if runtime.GOOS == "windows" {
		dst += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", dst, "./cmd/agchatd")
	cmd.Dir = ".."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("压测需要被测二进制，现场构建失败（%v）；也可以用 AGCHAT_BIN 指定：\n%s", err, out)
	}
	return dst
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatalf("写入 %s 失败: %v", dst, err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestLongMeetingsNeverCrash(t *testing.T) {
	// ★ 把二进制复制成唯一名字再跑。
	//
	// 教训：同一台机器上任何人执行 `taskkill /IM agchatd.exe` 或
	// PowerShell 的 `Get-Process agchatd | Stop-Process -Force`，
	// 都会把别人的测试实例一起打死 —— 表现就是"进程凭空消失、exitCode=-1、
	// 只有启动横幅"（我们为这个假 P0 追了四轮）。改成唯一名后，按镜像名的 kill
	// 打不到它，它也不会去误杀别人。
	baseBin := findBinary(t)
	bin := filepath.Join(os.TempDir(), fmt.Sprintf("agchatd-stress-%d", os.Getpid()))
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	copyFile(t, baseBin, bin)
	defer os.Remove(bin)

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	base := "http://" + addr

	cmd := exec.Command(bin,
		"-addr", addr,
		"-db", filepath.Join(t.TempDir(), "stress.db"),
		"-briefing", "1s",
		"-turn-timeout", "5s",
		"-summary-timeout", "5s", // 让每场都走一遍 summarizing → 兜底 这条长路径
		"-vote-grace", "0", // 压测不涉及掉线救援，关掉宽限期省 10s/场
		"-tick", "100ms",
		"-enum-rate", "0",
		"-room-rate", "0",
	)
	// ★ 必须接住：进程若 fatal error，栈只会打在 stderr 上
	var out bytes.Buffer
	var mu sync.Mutex
	w := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return out.Write(p)
	})
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// 只按**记录的 PID** 清理，绝不按镜像名匹配。
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	dump := func() string {
		mu.Lock()
		defer mu.Unlock()
		return tail(out.String(), 8000)
	}

	ready := false
	for i := 0; i < 200; i++ {
		if res, err := http.Get(base + "/healthz"); err == nil {
			_ = res.Body.Close()
			ready = true
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("进程在就绪前退出: %v\n--- 被测进程输出 ---\n%s", err, dump())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatalf("10 秒内没起来\n%s", dump())
	}

	dead := make(chan error, 1)
	go func() { dead <- <-exited }()

	start := time.Now()
	for i := 0; i < meetings; i++ {
		tokens, err := runLongMeeting(base, i)
		if err != nil {
			t.Fatalf("第 %d/%d 场失败: %v\n--- 被测进程输出 ---\n%s", i+1, meetings, err, dump())
		}
		select {
		case e := <-dead:
			t.Fatalf("第 %d 场后进程退出: %v\n--- 被测进程输出 ---\n%s", i+1, e, dump())
		default:
		}
		if res, err := http.Get(base + "/healthz"); err != nil {
			t.Fatalf("第 %d 场后探活失败: %v\n%s", i+1, err, dump())
		} else {
			_ = res.Body.Close()
		}
		t.Logf("第 %2d/%d 场完成  cost=%d  elapsed=%s", i+1, meetings, tokens, time.Since(start).Round(time.Millisecond))
	}

	elapsed := time.Since(start)
	logs := dump()
	t.Logf("=== 完成 %d 场 6 人 × %d 轮（每场 %d 个 20s 长轮询）总耗时 %s ===",
		meetings, roundsPerRoom, longPollers, elapsed.Round(time.Millisecond))
	if strings.Contains(logs, "fatal error") || strings.Contains(logs, "panic") {
		t.Fatalf("被测进程输出里出现 fatal error / panic:\n%s", logs)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func postJSON(url string, body any, token string) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("content-type", "application/json")
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, map[string]any{"err": err.Error()}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return res.StatusCode, m
}

func getJSON(url, token string) (int, map[string]any) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, map[string]any{"err": err.Error()}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return res.StatusCode, m
}

// runLongMeeting 跑一场满场会议：10 轮发言 → 投票 → 等概述超时兜底。
func runLongMeeting(base string, idx int) (tokens int, err error) {
	code, m := postJSON(base+"/api/rooms", map[string]any{
		"topic":     fmt.Sprintf("长会压测 %d：把定价从年付改成月付要不要？", idx),
		"maxAgents": agentsPerRoom, "maxRounds": roundsPerRoom, "maxMinutes": 30,
	}, "")
	if code != 201 {
		return 0, fmt.Errorf("建房 %d: %v", code, m)
	}
	no := strings.ReplaceAll(m["roomNo"].(string), " ", "")
	pass := m["password"].(string)

	toks := make([]string, 0, agentsPerRoom)
	for i := 0; i < agentsPerRoom; i++ {
		c, jm := postJSON(base+"/api/rooms/"+no+"/join", map[string]any{
			"password": pass, "alias": fmt.Sprintf("a%d-%d", idx, i), "model": "stress",
		}, "")
		if c != 201 {
			return 0, fmt.Errorf("加入 %d: %v", c, jm)
		}
		toks = append(toks, jm["agentToken"].(string))
	}

	// 3 个 agent 挂 20s 长轮询，全程不停（模拟真实 agent 心跳）
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < longPollers; i++ {
		wg.Add(1)
		go func(tok string) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				getJSON(base+"/api/rooms/"+no+"/inbox?since=0&wait=20", tok)
			}
		}(toks[i])
	}
	// 不 Wait：长轮询可能正挂着 20s，等它会拖慢整个压测
	defer close(stop)

	deadline := time.Now().Add(120 * time.Second)
	body := "这是本轮的观点陈述，针对议题给出具体理由与取舍。"

	// 10 轮 × 6 人，按进入顺序串行（主席 = m_1）
	var proposalID string
	for round := 1; round <= roundsPerRoom; round++ {
		for i := 0; i < agentsPerRoom; i++ {
			payload := map[string]any{
				"text": body, "opKey": fmt.Sprintf("m-%d-%d-%d", idx, round, i),
			}
			if round >= 2 && round < roundsPerRoom {
				payload["replyTo"] = 1
			}
			ok := false
			for try := 0; try < 400 && time.Now().Before(deadline); try++ {
				c, _ := postJSON(base+"/api/rooms/"+no+"/messages", payload, toks[i])
				if c == 201 {
					ok = true
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !ok {
				return 0, fmt.Errorf("R%d 第 %d 人发言失败", round, i)
			}
		}
		// 第 2 轮提一个方案（这样投票会产生获胜者 → 进 summarizing 等概述超时）
		if round == 2 {
			c, p := postJSON(base+"/api/rooms/"+no+"/proposals", map[string]any{
				"title": "唯一方案", "body": "方案正文内容", "opKey": fmt.Sprintf("p-%d", idx),
			}, toks[0])
			if c == 201 {
				proposalID, _ = p["proposalId"].(string)
			}
		}
	}

	// 投票：全员投给唯一方案 → 产生获胜者 → summarizing → 等 summary-timeout 兜底
	for i, tok := range toks {
		for try := 0; try < 400 && time.Now().Before(deadline); try++ {
			payload := map[string]any{"opKey": fmt.Sprintf("v-%d-%d", idx, i)}
			if proposalID != "" {
				payload["proposalId"] = proposalID
			} else {
				payload["finalStance"] = "agree"
			}
			c, d := postJSON(base+"/api/rooms/"+no+"/votes", payload, tok)
			if c == 201 || (c == 403 && isWrongRound(d)) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// 故意不提交概述：让会议停在 summarizing 等满 -summary-timeout，走的正是崩溃现场那条长路径

	// 等 ended：无获胜者会立刻 ended；这条同时覆盖 summarizing 路径
	for time.Now().Before(deadline) {
		c, res := getJSON(base+"/api/rooms/"+no+"/result", "")
		if c != 200 {
			return 0, fmt.Errorf("取结果 %d", c)
		}
		if st, _ := res["state"].(string); st == "ended" {
			if cost, ok := res["cost"].(map[string]any); ok {
				if v, ok := cost["estTokens"].(float64); ok {
					tokens = int(v)
				}
			}
			return tokens, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, fmt.Errorf("会议 %s 未在超时内结束", no)
}

func isWrongRound(d map[string]any) bool {
	if e, ok := d["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		return code == "wrong_round"
	}
	return false
}
