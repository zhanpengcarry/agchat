// Package meeting 是 agchat v2「会议流程」的黑盒验收测试。
//
// 与 server/isolation 一样，这一层只做两件事：起真实二进制、发真实 HTTP。
// 不 import 任何业务代码，不读数据库，不假设实现细节 —— 规格是
// docs/07-会议流程规格v2.md，实现改了但对得上规格，用例就不该红。
//
// 一个刻意的设计：**每条用例起一个独立实例**。原因是 v1 实测服务端对建房
// 有速率限制（10 次/进程），而本套件要建二十多个房间，共用实例会把自己打成 429。
// 独立实例还顺带让用例之间零污染，红的那条一定只跟它自己有关。
package meeting

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
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	startTimeout = 20 * time.Second
	httpTimeout  = 15 * time.Second
)

var httpClient = &http.Client{Timeout: httpTimeout}

/* ══════════════════════════════════════════════════════════════════════════
   定位二进制
   ══════════════════════════════════════════════════════════════════════════ */

func moduleRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}

func resolveBinary() (string, error) {
	if p := os.Getenv("AGCHAT_BIN"); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(abs); err == nil {
			return abs, nil
		}
		return "", fmt.Errorf("AGCHAT_BIN=%s 不存在", p)
	}
	root := moduleRoot()
	for _, c := range []string{
		filepath.Join(root, ".bin", "agchatd.exe"),
		filepath.Join(root, ".bin", "agchatd"),
		filepath.Join(root, "bin", "agchatd.exe"),
		filepath.Join(root, "bin", "agchatd"),
		filepath.Join(root, "cmd", "agchatd", "agchatd.exe"),
		filepath.Join(root, "cmd", "agchatd"),
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	// ★ 没找到就现编一个：全新 clone 里 `go test ./...` 必须能直接跑通。
	//   否则"测试通过"实际上变成了"在已经手工构建过的机器上通过" —— 实测全新 clone
	//   里 meeting 与 isolation 全场红，原因只是没人先 build。
	return buildForTest(root)
}

// buildForTest 把被测二进制现编到一个**唯一命名的临时路径**上。
//
// 仍遵守那条卫生规则：绝不编成 agchatd.exe，免得同机任何人按镜像名 kill 时误伤。
func buildForTest(root string) (string, error) {
	dst := filepath.Join(os.TempDir(), fmt.Sprintf("agchatd-test-%d%s", os.Getpid(), exeSuffix()))
	cmd := exec.Command("go", "build", "-o", dst, "./cmd/agchatd")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("现场构建被测二进制失败（%v）：\n%s", err, out)
	}
	return dst, nil
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

func testBinary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		src, err := resolveBinary()
		if err != nil {
			binErr = err
			return
		}
		// 现场编出来的已经是唯一命名的临时文件，直接用它。
		if strings.HasPrefix(filepath.Base(src), fmt.Sprintf("agchatd-test-%d", os.Getpid())) {
			binPath = src
			return
		}
		// 关键：测试实例用**唯一的镜像名**，不要叫 agchatd.exe。
		// 否则同机任何人执行 `taskkill /IM agchatd.exe` / `Stop-Process -Name agchatd`
		// 都会连测试实例一起杀掉 —— 症状是 exitCode=-1、零崩溃输出、connection refused，
		// 和"产品崩溃"长得一模一样，极难分辨。
		dst := filepath.Join(os.TempDir(), fmt.Sprintf("agchatd-test-%d.exe", os.Getpid()))
		if err := copyExe(src, dst); err != nil {
			binErr = fmt.Errorf("复制测试二进制失败：%w", err)
			return
		}
		binPath = dst
	})
	if binErr != nil {
		t.Fatalf("找不到被测二进制：%v", binErr)
	}
	return binPath
}

// copyExe 复制一份独占名字的可执行文件供本次测试进程使用。
func copyExe(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

var (
	flagCacheMu sync.Mutex
	flagCache   = map[string]bool{}
)

// supportsFlag 探测二进制是否支持某个 flag。规格里的新 flag 可能还没实现，
// 用例应当据此跳过而不是启动失败。
func supportsFlag(bin, name string) bool {
	flagCacheMu.Lock()
	defer flagCacheMu.Unlock()
	if v, ok := flagCache[name]; ok {
		return v
	}
	out, _ := exec.Command(bin, "-h").CombinedOutput()
	v := strings.Contains(string(out), "-"+name)
	flagCache[name] = v
	return v
}

/* ══════════════════════════════════════════════════════════════════════════
   起服务
   ══════════════════════════════════════════════════════════════════════════ */

type server struct {
	base     string
	port     int
	pid      int
	cmd      *exec.Cmd
	sink     *lineSink
	mu       sync.Mutex
	exited   bool
	exitCode int
	waitErr  string
}

// recordExit 记录进程真实退出状态——这是判定"产品崩溃 vs harness 弄丢进程"的关键。
func (s *server) recordExit(st *os.ProcessState, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exited = true
	if st != nil {
		s.exitCode = st.ExitCode()
	}
	if err != nil {
		s.waitErr = err.Error()
	}
}

func (s *server) exitInfo() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.exited {
		return "进程仍在运行（未退出）→ 不是崩溃，是测试侧/端口侧的问题"
	}
	if s.exitCode < 0 {
		return fmt.Sprintf("exitCode=%d（被信号杀死）→ 不是产品崩溃，是外部 kill", s.exitCode)
	}
	return fmt.Sprintf("exitCode=%d waitErr=%q", s.exitCode, s.waitErr)
}

// stop 只回收"自己确认启动过"的那个进程。
//
// 之前的实现直接 Process.Kill()，在几百次进程创建回收之后，
// 一旦 Windows 复用了 PID，就可能误杀新起的实例 —— 症状恰好是
// 后续请求 connection refused、exitCode=-1、且没有任何崩溃输出。
func (s *server) stop() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return
	}
	s.exited = true // 认领回收权，避免与 Wait goroutine 重复收尸
	pid := s.pid
	s.mu.Unlock()

	// ① PID 必须与启动时记录的一致（防 PID 复用误杀）
	if pid != 0 && s.cmd.Process.Pid != pid {
		return
	}
	// ② 已经被 Wait 收过尸的进程不再碰
	if s.cmd.ProcessState != nil {
		return
	}
	_ = s.cmd.Process.Kill()
}

// output 返回被测进程的 stdout+stderr 快照（审计/崩溃诊断用）。
func (s *server) output() string { return s.sink.output() }

type lineSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

var portRe = regexp.MustCompile(`:(\d{2,5})\b`)

func (l *lineSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	return len(p), nil
}

func (l *lineSink) output() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *lineSink) ports() []int {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []int
	for _, m := range portRe.FindAllStringSubmatch(l.buf.String(), -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// newServer 起一个独立实例，测试结束自动回收。
//
// 默认带 -briefing 1s：规格里 briefing 默认等 120s，若不加这个参数，
// 人少的会议要等两分钟才开场，整套用例没法跑。
func newServer(t *testing.T, extra ...string) *server {
	t.Helper()
	bin := testBinary(t)

	args := []string{}
	if supportsFlag(bin, "briefing") {
		args = append(args, "-briefing", "1s")
	}
	// 枚举限流会干扰本套件里"多次打不存在房间"的探测，能关就关
	if supportsFlag(bin, "enum-rate") {
		args = append(args, "-enum-rate", "0")
	}
	args = append(args, extra...)

	srv, err := launch(bin, moduleRoot(), args)
	if err != nil {
		t.Fatalf("启动被测实例失败：%v", err)
	}
	// 清理顺序是 LIFO：这个 dump 必须在 stop 之前跑，否则进程已被杀、
	// 但缓冲区里其实还有内容；更重要的是失败时把 stderr 带出来，
	// 否则只能看到 connection refused，分不清是测试写错还是进程崩溃。
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		out := srv.output()
		if len(out) > 20000 {
			out = "…(前略)…\n" + tailRunes(out, 20000)
		}
		t.Logf("========== 被测进程诊断 ==========\n  %s\n  有 fatal error: %v\n  有 panic: %v\n=================================\n%s\n==========",
			srv.exitInfo(), containsAny(out, "fatal error"), containsAny(out, "panic:"), out)
	})
	t.Cleanup(srv.stop)
	return srv
}

func containsAny(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && strings.Contains(s, sub)
}

func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func launch(bin, dir string, args []string) (*server, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(os.TempDir(), fmt.Sprintf("agchat-meeting-%d.db", time.Now().UnixNano()))
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	full := append([]string{"-addr", addr}, args...)
	full = append(full, "-db", dbPath)

	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Args = append(cmd.Args, full...)
	sink := &lineSink{}
	cmd.Stdout = sink
	cmd.Stderr = sink

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	s := &server{cmd: cmd, sink: sink, port: port, pid: cmd.Process.Pid}

	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		s.recordExit(cmd.ProcessState, err)
		exited <- err
	}()

	deadline := time.After(startTimeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-exited:
			out := strings.TrimSpace(sink.output())
			if len(out) > 400 {
				out = out[:400] + "…"
			}
			return nil, fmt.Errorf("进程退出。输出：%s", out)
		case <-deadline:
			out := strings.TrimSpace(sink.output())
			if len(out) > 400 {
				out = out[:400] + "…"
			}
			return nil, fmt.Errorf("等了 %s 仍未就绪。输出：%s", startTimeout, out)
		case <-tick.C:
			for _, p := range append([]int{port}, sink.ports()...) {
				if base, ok := probe(p); ok {
					s.base = base
					return s, nil
				}
			}
		}
	}
}

func tcpOpen(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// probe 判断路由前缀。零副作用优先：先打不存在的房间，靠错误码区分"路由存在"。
func probe(port int) (string, bool) {
	if !tcpOpen(port) {
		return "", false
	}
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	c := &http.Client{Timeout: 3 * time.Second}

	for _, prefix := range []string{"/api", ""} {
		res, err := c.Get(origin + prefix + "/rooms/__probe__/result")
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
		_ = res.Body.Close()
		body := string(raw)
		if res.StatusCode == 404 &&
			(strings.Contains(body, "room_not_found") || strings.Contains(body, "not_found")) {
			return origin + prefix, true
		}
	}
	return "", false
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

/* ══════════════════════════════════════════════════════════════════════════
   HTTP
   ══════════════════════════════════════════════════════════════════════════ */

type resp struct {
	Status int
	Body   []byte
	Method string
	Path   string
}

func (r resp) snippet() string {
	s := strings.TrimSpace(string(r.Body))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		return "(空响应体)"
	}
	return s
}

// errCode 取响应体里的错误码，用于精确断言 422 text_too_long 这类业务码。
func (r resp) errCode() string {
	var v struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(r.Body, &v) == nil && v.Error.Code != "" {
		return v.Error.Code
	}
	return ""
}

func call(t *testing.T, base, method, path, token string, body any) resp {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("构造请求体失败：%v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败（服务可能挂了）：%v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return resp{Status: res.StatusCode, Body: raw, Method: method, Path: path}
}

func expectStatus(t *testing.T, r resp, want int, what string) {
	t.Helper()
	if r.Status != want {
		t.Errorf("%s\n  期望 HTTP %d，实际 %d\n  请求 %s %s\n  响应 %s",
			what, want, r.Status, r.Method, r.Path, r.snippet())
	}
}

func expectStatusIn(t *testing.T, r resp, what string, want ...int) {
	t.Helper()
	for _, w := range want {
		if r.Status == w {
			return
		}
	}
	t.Errorf("%s\n  期望 HTTP %v 之一，实际 %d\n  请求 %s %s\n  响应 %s",
		what, want, r.Status, r.Method, r.Path, r.snippet())
}
