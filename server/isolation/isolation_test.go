// Package isolation 是 agchat 后端的**黑盒**数据隔离攻击测试。
//
// 设计约束（故意写死，别改成白盒）：
//   - 本包只做两件事：起进程、发 HTTP。不 import 任何业务代码，不读数据库，不改内存状态。
//   - 因此它可以对着任何语言的实现跑（当前是同事的 Go 版），也可以对着已部署的实例跑。
//   - 唯一的外部输入是二进制路径（AGCHAT_BIN）或已部署地址（AGCHAT_URL），都靠探测得到。
//
// 威胁模型：
//
//	攻击者 = 一个持有合法 agent token / adminKey 的租户，想越界到别人的房间。
//	财产   = 房间里的消息内容、房间是否存在（存在性本身就是信息）、其他 agent 的凭证。
//
// 判定口径（决定"通过"的含义）：
//   - 跨租户访问资源必须返回 404 而不是 403。403 等于承认"房间存在，只是你没权限"，
//     攻击者靠 403/404 的差异就能把全平台房间枚举出来。
//   - 公开接口（/rooms/{id}/result）可以 200，但响应体里不许出现凭证字段。
//   - 身份必须由 token 决定，body 里塞什么都不许改身份。
//
// 跑法：
//
//	cd server && go test ./isolation -v
//	AGCHAT_BIN=/abs/path/agchatd go test ./isolation -v
//	AGCHAT_URL=http://127.0.0.1:8788/api go test ./isolation -v   # 打已部署实例
package isolation

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
	"sync/atomic"
	"testing"
	"time"
)

/* ══════════════════════════════════════════════════════════════════════════
   全局
   ══════════════════════════════════════════════════════════════════════════ */

var (
	// baseURL 形如 http://127.0.0.1:54321/api（前缀由探测决定，不强假定）
	baseURL string
	client  *http.Client
	opSeq   int64

	// serverLog 返回被测进程 stdout+stderr 的当前快照（累积）。
	// 审计日志是否生效、是否回显凭证，只能从进程输出里黑盒验证。
	// 用 AGCHAT_URL 打外部实例时为 nil（拿不到别人的进程输出）。
	serverLog func() string

	// serverBin 记录被测二进制路径，供个别需要"另起一个不同配置实例"的用例使用。
	serverBin string

	// serverDBPath 是被测进程使用的 SQLite 文件，供"表结构"类断言直接读文件。
	// 读文件不算白盒：它验的是"落盘的东西里有没有多余字段"，属于可观察行为。
	serverDBPath string
)

// supportsFlag 探测二进制是否支持某个 flag。
// 用来做"能力可选"的适配：被测版本没有这个 flag 时，用例应当跳过而不是启动失败。
func supportsFlag(bin, name string) bool {
	out, _ := exec.Command(bin, "-h").CombinedOutput()
	return strings.Contains(string(out), "-"+name)
}

const (
	startTimeout  = 20 * time.Second
	httpTimeout   = 15 * time.Second
	defaultRounds = 6
)

func TestMain(m *testing.M) {
	client = &http.Client{Timeout: httpTimeout}

	// 已经部署好的实例：直接指定基地址，不接管进程。
	// 用于对 staging 做冒烟，也是本地验证"测试 harness 本身好不好用"的捷径。
	if u := strings.TrimRight(os.Getenv("AGCHAT_URL"), "/"); u != "" {
		baseURL = u
		fmt.Printf("[isolation] 使用外部实例：%s（未接管进程）\n\n", baseURL)
		os.Exit(m.Run())
	}

	bin, err := resolveBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n[isolation] 找不到被测二进制：%v\n", err)
		fmt.Fprintf(os.Stderr, "[isolation] 先 `go build -o .bin/agchatd.exe ./cmd/agchatd`，或用 AGCHAT_BIN 指定\n\n")
		os.Exit(1)
	}

	serverBin = bin

	// 批量枚举/计时用例需要连续打几百个探测请求才能测出"存在性不可区分"。
	// 如果被测版本带枚举限流（-enum-rate），这里把它关掉，否则用例会被 429 打断；
	// 限流本身由 TestEnumRateLimiterBlocksBulkEnumeration 另起一个受限实例单独验证。
	var extra []string
	if supportsFlag(bin, "enum-rate") {
		extra = append(extra, "-enum-rate", "0")
	}

	srv, err := launchWith(bin, extra)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n[isolation] 被测服务起不来：%v\n", err)
		os.Exit(1)
	}

	baseURL = srv.base
	serverLog = srv.sink.output
	fmt.Printf("[isolation] 二进制   %s\n", bin)
	fmt.Printf("[isolation] 基地址   %s\n", baseURL)
	fmt.Printf("[isolation] 进程号   %d\n\n", srv.cmd.Process.Pid)

	code := m.Run()

	// 失败时把被测进程的最后输出打出来。
	// 很多"用例红"其实是服务端崩了，没有这段输出只能看到 connection refused，
	// 分不清是测试写错还是服务崩溃。
	if code != 0 {
		out := srv.sink.output()
		if len(out) > 6000 {
			out = "…(前略)…\n" + out[len(out)-6000:]
		}
		fmt.Fprintf(os.Stderr, "\n========== 被测服务最后输出 ==========\n%s\n======================================\n", out)
	}

	// 必须显式收尸：os.Exit 不会执行 defer，漏掉这行就会每跑一次测试
	// 留下一个占着端口和 db 的僵尸进程。
	srv.stop()
	os.Exit(code)
}

/* ══════════════════════════════════════════════════════════════════════════
   定位二进制
   ══════════════════════════════════════════════════════════════════════════ */

func moduleRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for i := 0; i < 6; i++ {
		if fileExists(filepath.Join(dir, "go.mod")) {
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
		if fileExists(abs) && !isDir(abs) {
			return abs, nil
		}
		return "", fmt.Errorf("AGCHAT_BIN=%s 指向的文件不存在", p)
	}

	root := moduleRoot()
	cands := []string{
		filepath.Join(root, ".bin", "agchatd.exe"),
		filepath.Join(root, ".bin", "agchatd"),
		filepath.Join(root, "cmd", "agchatd", "agchatd.exe"),
		filepath.Join(root, "cmd", "agchatd"),
		filepath.Join(root, "agchatd.exe"),
		filepath.Join(root, "agchatd"),
		filepath.Join(root, "bin", "agchatd.exe"),
		filepath.Join(root, "bin", "agchatd"),
	}
	for _, c := range cands {
		if fileExists(c) && !isDir(c) {
			return c, nil
		}
	}
	// ★ 没找到就现编一个：全新 clone 里 `go test ./...` 必须能直接跑通。
	//   否则"隔离测试通过"实际上变成了"在已经手工构建过的机器上通过"。
	return buildForTest(root)
}

// buildForTest 把被测二进制现编到一个唯一命名的临时路径（避免按镜像名误杀）。
func buildForTest(root string) (string, error) {
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	dst := filepath.Join(os.TempDir(), fmt.Sprintf("agchatd-iso-%d%s", os.Getpid(), suffix))
	cmd := exec.Command("go", "build", "-o", dst, "./cmd/agchatd")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("现场构建被测二进制失败（%v）：\n%s", err, out)
	}
	return dst, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

/* ══════════════════════════════════════════════════════════════════════════
   启动被测服务
   ──────────────────────────────────────────────────────────────────────────
   事先不知道同事怎么接端口，所以依次试：
     env  PORT / ADDR / LISTEN_ADDR / HTTP_ADDR / AGCHAT_ADDR / AGCHAT_PORT
     args -addr / --addr / -port / --port
     都不行就裸启动，从 stdout/stderr 抓 :PORT
   拿到端口后用 POST /rooms 探测路由前缀是 /api 还是 /。
   ══════════════════════════════════════════════════════════════════════════ */

type lineSink struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	ports []int
}

var portRe = regexp.MustCompile(`:(\d{2,5})\b`)

func (s *lineSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf.Write(p)
	for _, m := range portRe.FindAllSubmatch(p, -1) {
		if n, err := strconv.Atoi(string(m[1])); err == nil && n > 0 {
			s.ports = append(s.ports, n)
		}
	}
	return len(p), nil
}

func (s *lineSink) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *lineSink) seenPorts() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, len(s.ports))
	copy(out, s.ports)
	return out
}

type serverHandle struct {
	cmd  *exec.Cmd
	sink *lineSink
	base string
	port int
}

// baseAt 返回同一个实例在指定主机上的基地址（用于"两个来源 IP"的用例：
// 双栈监听时 127.0.0.1 与 ::1 是两个真实的 TCP 对端）。
func (h *serverHandle) baseAt(host string) string {
	prefix := ""
	if i := strings.Index(h.base, "/api"); i >= 0 {
		prefix = h.base[i:]
	}
	if strings.Contains(host, ":") {
		host = "[" + strings.Trim(host, "[]") + "]"
	}
	return fmt.Sprintf("http://%s:%d%s", host, h.port, prefix)
}

func (h *serverHandle) stop() {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Kill()
	_, _ = h.cmd.Process.Wait()
}

type launchMode struct {
	name  string
	env   bool
	args  []string
	extra []string // 调用方追加的 flag（如 -enum-rate）
	db    bool     // 追加 -db <临时文件>，避免往被测仓库里写数据文件
	port  int      // 0 = 裸启动，等它自己打印端口
	host  string   // 监听/探测的主机，默认 127.0.0.1；双栈用 [::]
}

// tempDBPath 给每个被测进程一个独立的临时库，保证用例之间不互相污染，
// 也不在 server/ 目录里留 agchat.db 这种脏文件。
func tempDBPath() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("agchat-iso-%d.db", time.Now().UnixNano()))
}

func launch(bin string) (*serverHandle, error) {
	return launchWithHost(bin, "127.0.0.1", nil)
}

func launchWith(bin string, extra []string) (*serverHandle, error) {
	return launchWithHost(bin, "127.0.0.1", extra)
}

// launchWithHost 允许指定监听主机。双栈（[::]）用于制造"两个真实的来源 IP"
// ——限流是否按来源隔离，只能靠 127.0.0.1 与 ::1 这种真实不同的 TCP 对端来验，
// 靠 X-Forwarded-For 是验不出来的（服务端不一定信任它）。
func launchWithHost(bin, host string, extra []string) (*serverHandle, error) {
	root := moduleRoot()
	modes := []launchMode{
		// 本仓库后端的原生参数（-addr / -db）放最前面，直接拿到随机端口
		{name: "-addr -db", args: []string{"-addr", "%ADDR%"}, db: true, extra: extra, host: host},
		{name: "-addr", args: []string{"-addr", "%ADDR%"}, extra: extra, host: host},
		{name: "env-all", env: true, extra: extra, host: host},
		{name: "--addr", args: []string{"--addr", "%ADDR%"}, extra: extra, host: host},
		{name: "-port", args: []string{"-port", "%PORT%"}, extra: extra, host: host},
		{name: "--port", args: []string{"--port", "%PORT%"}, extra: extra, host: host},
		{name: "auto", extra: extra, host: host},
	}

	var errs []string
	for _, m := range modes {
		port := 0
		if m.name != "auto" {
			p, err := freePort()
			if err != nil {
				errs = append(errs, m.name+": "+err.Error())
				continue
			}
			port = p
		}
		h, err := tryLaunch(bin, root, m, port)
		if err == nil {
			return h, nil
		}
		errs = append(errs, m.name+": "+err.Error())
	}
	return nil, fmt.Errorf("所有启动方式都失败：\n  %s", strings.Join(errs, "\n  "))
}

func tryLaunch(bin, dir string, mode launchMode, port int) (*serverHandle, error) {
	cmd := exec.Command(bin)
	cmd.Dir = dir

	env := os.Environ()
	args := []string{}
	addr := ""
	if port > 0 {
		h := mode.host
		if h == "" {
			h = "127.0.0.1"
		}
		if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
			h = "[" + h + "]"
		}
		addr = fmt.Sprintf("%s:%d", h, port)
	}

	if mode.env {
		env = append(env,
			fmt.Sprintf("PORT=%d", port),
			"ADDR="+addr,
			"LISTEN_ADDR="+addr,
			"HTTP_ADDR="+addr,
			"AGCHAT_ADDR="+addr,
			"AGCHAT_LISTEN="+addr,
			fmt.Sprintf("AGCHAT_PORT=%d", port),
		)
	}
	for _, a := range mode.args {
		args = append(args, strings.NewReplacer("%ADDR%", addr, "%PORT%", strconv.Itoa(port)).Replace(a))
	}
	lastDBArg := ""
	if mode.db {
		lastDBArg = tempDBPath()
		args = append(args, "-db", lastDBArg)
	}
	if len(mode.extra) > 0 {
		args = append(args, mode.extra...)
	}

	cmd.Env = env
	// 关键：exec.Command(bin) 之后 Args 只有 [bin]，必须把探测用的参数接上去，
	// 否则进程会按默认 flag 启动（默认端口 + 默认 db 文件），
	// 后果是测试串到固定端口、还会往仓库目录里写数据文件。
	cmd.Args = append(cmd.Args, args...)
	if mode.db {
		serverDBPath = lastDBArg
	}
	sink := &lineSink{}
	cmd.Stdout = sink
	cmd.Stderr = sink

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h := &serverHandle{cmd: cmd, sink: sink}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.After(startTimeout)
	tick := time.NewTicker(120 * time.Millisecond)
	defer tick.Stop()

	dumpOut := func() string {
		out := strings.TrimSpace(sink.output())
		if len(out) > 400 {
			out = out[:400] + "…"
		}
		return out
	}

	for {
		select {
		case <-exited:
			return nil, fmt.Errorf("进程退出。输出：%s", dumpOut())
		case <-deadline:
			// 别把探测失败的进程留在后台占着端口
			h.stop()
			return nil, fmt.Errorf("等了 %s 仍不可用。输出：%s", startTimeout, dumpOut())
		case <-tick.C:
			ports := []int{}
			if port > 0 {
				ports = append(ports, port)
			}
			ports = append(ports, sink.seenPorts()...)
			for _, p := range ports {
				if base, ok := probePrefix(p); ok {
					h.base = base
					h.port = p
					return h, nil
				}
			}
		}
	}
}

func tcpOpen(port int) bool {
	return tcpOpenHost("127.0.0.1", port) || tcpOpenHost("::1", port)
}

func tcpOpenHost(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// probePrefix 判断路由前缀。会依次尝试 IPv4/IPv6 回环，
// 因为双栈实例可能只接受其中一个（用于"两个来源 IP"的限流用例）。
func probePrefix(port int) (string, bool) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		if base, ok := probePrefixOn(host, port); ok {
			return base, true
		}
	}
	return "", false
}

func probePrefixOn(host string, port int) (string, bool) {
	if !tcpOpenHost(host, port) {
		return "", false
	}
	c := &http.Client{Timeout: 3 * time.Second}
	origin := fmt.Sprintf("http://%s", net.JoinHostPort(host, strconv.Itoa(port)))

	for _, prefix := range []string{"/api", ""} {
		u := origin + prefix + "/rooms/__isolation_probe__/result"
		res, err := c.Get(u)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
		_ = res.Body.Close()
		// 路由存在 → 会走到房间查询逻辑 → 返回 room_not_found
		if res.StatusCode == 404 && strings.Contains(string(raw), "room_not_found") {
			return origin + prefix, true
		}
	}

	// 兜底：实现里没有这个错误码时，用建房探测（会消耗一个建房配额）
	body, _ := json.Marshal(map[string]any{
		"topic":       "__isolation_probe__",
		"maxAgents":   1,
		"maxRounds":   2,
		"tokenBudget": 100000,
	})
	for _, prefix := range []string{"/api", ""} {
		u := origin + prefix + "/rooms"
		req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("content-type", "application/json")
		res, err := c.Do(req)
		if err != nil {
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
		_ = res.Body.Close()
		if (res.StatusCode == 200 || res.StatusCode == 201) && strings.Contains(string(raw), "roomId") {
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
   HTTP 基础
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
	if len(s) > 360 {
		s = s[:360] + "…"
	}
	if s == "" {
		s = "(空响应体)"
	}
	return s
}

// do 是最底层：所有可能出现在 sync.Once 里的调用都走它（返回 error，不 Fatalf）。
func do(method, path, token string, body any) (resp, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return resp{}, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, rdr)
	if err != nil {
		return resp{}, err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if err != nil {
		return resp{}, err
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	_ = res.Body.Close()
	return resp{Status: res.StatusCode, Body: raw, Method: method, Path: path}, nil
}

func call(t *testing.T, method, path, token string, body any) resp {
	t.Helper()
	r, err := do(method, path, token, body)
	if err != nil {
		t.Fatalf("%s %s 请求失败（服务可能挂了）：%v", method, path, err)
	}
	return r
}

func expectStatus(t *testing.T, got resp, want int, what string) {
	t.Helper()
	if got.Status == want {
		return
	}
	t.Errorf("%s\n  期望 HTTP %d，实际 %d\n  请求 %s %s\n  响应 %s",
		what, want, got.Status, got.Method, got.Path, got.snippet())
}

func expectOneOf(t *testing.T, got resp, what string, want ...int) {
	t.Helper()
	for _, w := range want {
		if got.Status == w {
			return
		}
	}
	t.Errorf("%s\n  期望 HTTP %v 之一，实际 %d\n  请求 %s %s\n  响应 %s",
		what, want, got.Status, got.Method, got.Path, got.snippet())
}

/* ══════════════════════════════════════════════════════════════════════════
   测试夹具
   ──────────────────────────────────────────────────────────────────────────
   租户只建一次（sync.Once）：
     - 建房接口通常有速率限制，每个用例都新建会把测试自己打死；
     - 也顺带验证"同一个 DB 文件里多个租户并存"这个前提。
   ══════════════════════════════════════════════════════════════════════════ */

type roomInfo struct {
	RoomID   string `json:"roomId"`
	RoomURL  string `json:"roomUrl"`
	Password string `json:"password"`
	AdminKey string `json:"adminKey"`
}

type joinInfo struct {
	AgentToken string `json:"agentToken"`
	AgentID    string `json:"agentId"`
}

type inboxInfo struct {
	State    string   `json:"state"`
	Round    int      `json:"round"`
	YourTurn bool     `json:"yourTurn"`
	Unread   int      `json:"unread"`
	Notes    []string `json:"notes"`
}

type message struct {
	Seq     int    `json:"seq"`
	Round   int    `json:"round"`
	AgentID string `json:"agentId"`
	Alias   string `json:"alias"`
	Model   string `json:"model"`
	Text    string `json:"text"`
	ReplyTo *int   `json:"replyTo"`
	Stance  string `json:"stance"`
}

type tenant struct {
	name       string
	topic      string
	roomID     string
	password   string
	adminKey   string
	agentToken string
	agentID    string
	alias      string
}

var (
	fixtureOnce sync.Once
	fixtureErr  error
	tenantA     *tenant
	tenantB     *tenant
	tenantC     *tenant
)

// sharedTenants 返回三个互不相关的租户。
// 注意：失败信息在 Once 外面抛，否则 t.Fatalf 会 Goexit 导致 Once 永远不完成。
func sharedTenants(t *testing.T) (*tenant, *tenant, *tenant) {
	t.Helper()
	fixtureOnce.Do(func() {
		if tenantA, fixtureErr = tryNewTenant("tenantA"); fixtureErr != nil {
			return
		}
		if tenantB, fixtureErr = tryNewTenant("tenantB"); fixtureErr != nil {
			return
		}
		tenantC, fixtureErr = tryNewTenant("tenantC")
	})
	if fixtureErr != nil {
		t.Fatalf("准备共享租户失败（服务没按契约建好房/进场）：%v", fixtureErr)
	}
	return tenantA, tenantB, tenantC
}

func newUniqueTopic(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// sharedOpenRoom 是一个"还没开辩、还有空位"的房间，专门给进场类用例用：
//   - 用例5（拿别人的密码 join）需要它还在 briefing，才能拿到 401 而不是 410；
//   - 注入用例（超长 alias / 注入型 model 进场）也需要一个能进的空位。
//
// 合用一个房间是因为服务端对建房有速率限制（实测 10 个/进程），
// 每个用例各建一个会把整套用例挤到 429。
var (
	openRoomOnce sync.Once
	openRoomData roomInfo
	openRoomErr  error
)

func sharedOpenRoom(t *testing.T) roomInfo {
	t.Helper()
	openRoomOnce.Do(func() {
		openRoomData, openRoomErr = tryCreateRoom(newUniqueTopic("open-join"), 10)
	})
	if openRoomErr != nil {
		t.Fatalf("准备 open 房间失败：%v", openRoomErr)
	}
	return openRoomData
}

func tryCreateRoom(topic string, maxAgents int) (roomInfo, error) {
	var info roomInfo
	r, err := do(http.MethodPost, "/rooms", "", map[string]any{
		"topic":        topic,
		"rules":        "isolation test room",
		"tokenBudget":  200000,
		"maxRounds":    defaultRounds,
		"maxAgents":    maxAgents,
		"minutesLimit": 60,
	})
	if err != nil {
		return info, err
	}
	if r.Status != 200 && r.Status != 201 {
		if r.Status == http.StatusTooManyRequests {
			return info, fmt.Errorf("建房被限流（429 room_rate_limited）：本次测试进程内建的房间已到达上限。" +
				"这是**用例数量**问题，不是隔离缺陷——请合并用例或减少房间数，别去改服务端")
		}
		return info, fmt.Errorf("建房返回 HTTP %d：%s", r.Status, r.snippet())
	}
	if err := json.Unmarshal(r.Body, &info); err != nil {
		return info, fmt.Errorf("建房响应不是 JSON：%v（%s）", err, r.snippet())
	}
	if info.RoomID == "" || info.Password == "" || info.AdminKey == "" {
		return info, fmt.Errorf("建房响应缺 roomId/password/adminKey：%s", r.snippet())
	}
	return info, nil
}

func tryJoin(roomID, password, alias string) (joinInfo, error) {
	var info joinInfo
	r, err := do(http.MethodPost, "/rooms/"+roomID+"/join", "", map[string]any{
		"password": password,
		"alias":    alias,
		"model":    "isolation-test",
	})
	if err != nil {
		return info, err
	}
	if r.Status != 200 && r.Status != 201 {
		return info, fmt.Errorf("进场返回 HTTP %d：%s", r.Status, r.snippet())
	}
	if err := json.Unmarshal(r.Body, &info); err != nil {
		return info, fmt.Errorf("进场响应不是 JSON：%v（%s）", err, r.snippet())
	}
	if info.AgentToken == "" || info.AgentID == "" {
		return info, fmt.Errorf("进场响应缺 agentToken/agentId：%s", r.snippet())
	}
	return info, nil
}

// tryNewTenant 建"完整租户"：房间 + 一个已进场的 agent。
// maxAgents=1 → 一人进场就开辩，便于构造"轮到我"的状态。
func tryNewTenant(name string) (*tenant, error) {
	topic := newUniqueTopic(name + "-topic")
	room, err := tryCreateRoom(topic, 2) // v2：单人进会会跳过辩论，必须有 2 人
	if err != nil {
		return nil, err
	}
	alias := name + "-agent"
	j, err := tryJoin(room.RoomID, room.Password, alias)
	if err != nil {
		return nil, fmt.Errorf("租户 %s 进场失败：%w", name, err)
	}
	// 第二个 agent 只是为了把房间推进 debating（v2 单人会议不辩论），
	// 夹具对外仍只暴露第一个 agent。
	if _, err := tryJoin(room.RoomID, room.Password, alias+"-peer"); err != nil {
		return nil, fmt.Errorf("租户 %s 的第二人进场失败：%w", name, err)
	}
	return &tenant{
		name:       name,
		topic:      topic,
		roomID:     room.RoomID,
		password:   room.Password,
		adminKey:   room.AdminKey,
		agentToken: j.AgentToken,
		agentID:    j.AgentID,
		alias:      alias,
	}, nil
}

func (tn *tenant) inboxPath() string    { return "/rooms/" + tn.roomID + "/inbox" }
func (tn *tenant) messagesPath() string { return "/rooms/" + tn.roomID + "/messages" }

func readInbox(t *testing.T, tn *tenant) inboxInfo {
	t.Helper()
	r := call(t, http.MethodGet, tn.inboxPath()+"?wait=0", tn.agentToken, nil)
	if r.Status != 200 {
		t.Fatalf("读 inbox 失败：HTTP %d\n  响应 %s", r.Status, r.snippet())
	}
	var in inboxInfo
	if err := json.Unmarshal(r.Body, &in); err != nil {
		t.Fatalf("inbox 不是合法 JSON：%v\n  响应 %s", err, r.snippet())
	}
	return in
}

// waitMyTurn 等到房间进入辩论态且轮到这个 agent。
// 服务端可能在 join 之后的下一个 tick 才开辩，所以轮询而不是直接断言。
func waitMyTurn(t *testing.T, tn *tenant) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last inboxInfo
	for time.Now().Before(deadline) {
		last = readInbox(t, tn)
		if (last.State == "debating" || last.State == "converging") && last.YourTurn {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等 10s 仍没轮到 %s 发言：state=%s round=%d yourTurn=%v notes=%v",
		tn.alias, last.State, last.Round, last.YourTurn, last.Notes)
}

func newOpKey() string {
	return fmt.Sprintf("iso-%d-%d", time.Now().UnixNano(), atomic.AddInt64(&opSeq, 1))
}

func postMessage(t *testing.T, tn *tenant, text string, extra map[string]any) resp {
	t.Helper()
	body := map[string]any{"text": text, "opKey": newOpKey()}
	for k, v := range extra {
		body[k] = v
	}
	return call(t, http.MethodPost, tn.messagesPath(), tn.agentToken, body)
}

func listMessages(t *testing.T, tn *tenant) []message {
	t.Helper()
	r := call(t, http.MethodGet, tn.messagesPath()+"?since=0", tn.agentToken, nil)
	if r.Status != 200 {
		t.Fatalf("读消息失败：HTTP %d\n  响应 %s", r.Status, r.snippet())
	}
	var out struct {
		Messages []message `json:"messages"`
	}
	if err := json.Unmarshal(r.Body, &out); err != nil {
		t.Fatalf("消息响应不是合法 JSON：%v\n  响应 %s", err, r.snippet())
	}
	return out.Messages
}

func getResult(t *testing.T, roomID string) (int, []byte) {
	t.Helper()
	r := call(t, http.MethodGet, "/rooms/"+roomID+"/result", "", nil)
	return r.Status, r.Body
}

/* ══════════════════════════════════════════════════════════════════════════
   1-4：跨租户 / 跨房间，一律必须 404
   ──────────────────────────────────────────────────────────────────────────
   判定 404 而不是 403：403 告诉攻击者"房间存在，只是没权限"，
   靠 403/404 的差异就能把全平台房间枚举出来。
   ══════════════════════════════════════════════════════════════════════════ */

func TestCrossTenantInbox(t *testing.T) {
	a, b, _ := sharedTenants(t)

	r := call(t, http.MethodGet, "/rooms/"+b.roomID+"/inbox?wait=0", a.agentToken, nil)
	expectStatus(t, r, http.StatusNotFound,
		"用例1：A 的 agent token 读 B 的 inbox，期望 404（403 也算泄漏——它承认了房间存在）")

	r2 := call(t, http.MethodGet, "/rooms/"+a.roomID+"/inbox?wait=0", b.agentToken, nil)
	expectStatus(t, r2, http.StatusNotFound, "用例1b：B 的 token 读 A 的 inbox，期望 404")
}

func TestCrossTenantPostMessage(t *testing.T) {
	a, b, _ := sharedTenants(t)

	r := call(t, http.MethodPost, "/rooms/"+b.roomID+"/messages", a.agentToken, map[string]any{
		"text":  "越权注入到别人的房间",
		"opKey": newOpKey(),
	})
	expectStatus(t, r, http.StatusNotFound, "用例2：A 的 token 往 B 的房间发消息，期望 404")

	for _, m := range listMessages(t, b) {
		if strings.Contains(m.Text, "越权注入到别人的房间") {
			t.Errorf("用例2：A 的越权发言竟然落到 B 的房间里（seq=%d alias=%s）", m.Seq, m.Alias)
		}
	}
}

func TestCrossTenantReadMessages(t *testing.T) {
	a, b, _ := sharedTenants(t)

	r := call(t, http.MethodGet, "/rooms/"+b.roomID+"/messages?since=0", a.agentToken, nil)
	expectStatus(t, r, http.StatusNotFound, "用例3：A 的 token 读 B 的消息正文，期望 404")

	if strings.Contains(string(r.Body), b.topic) {
		t.Errorf("用例3：404 响应体里泄漏了 B 的议题 %q（存在性/内容泄漏）\n  响应 %s", b.topic, r.snippet())
	}
}

func TestCrossTenantClose(t *testing.T) {
	a, b, _ := sharedTenants(t)

	r := call(t, http.MethodPost, "/rooms/"+b.roomID+"/close", a.adminKey, map[string]any{
		"adminKey": a.adminKey,
	})
	expectStatus(t, r, http.StatusNotFound, "用例4：A 的 adminKey 关 B 的房间，期望 404")

	if in := readInbox(t, b); in.State == "ended" {
		t.Errorf("用例4：B 的房间被 A 的 adminKey 关掉了（state=ended）")
	}
}

/* ══════════════════════════════════════════════════════════════════════════
   5：密码不能跨房间使用
   ══════════════════════════════════════════════════════════════════════════ */

func TestPasswordNotPortableAcrossRooms(t *testing.T) {
	_, b, _ := sharedTenants(t)

	// 专门用一个还在 briefing 的房间（maxAgents 大、没人进场），
	// 这样 join 会因密码错返回 401，而不是因满员/已开辩返回 409/410。
	roomA := sharedOpenRoom(t)

	r := call(t, http.MethodPost, "/rooms/"+roomA.RoomID+"/join", "", map[string]any{
		"password": b.password, // B 的合法密码，拿到 A 的房间用
		"alias":    "intruder",
		"model":    "isolation-test",
	})
	if r.Status != http.StatusUnauthorized {
		hint := ""
		if r.Status == http.StatusConflict || r.Status == http.StatusGone {
			hint = "\n  注意：房间已满或已开辩，这一轮没真正验证到密码隔离，检查夹具"
		}
		t.Errorf("用例5：拿 B 的房间密码 join A 的房间，期望 401，实际 %d%s\n  响应 %s",
			r.Status, hint, r.snippet())
	}

	// 对照组：A 自己的密码必须能进，否则上面的 401 可能是"接口根本不让 join"
	r2 := call(t, http.MethodPost, "/rooms/"+roomA.RoomID+"/join", "", map[string]any{
		"password": roomA.Password,
		"alias":    "legit",
		"model":    "isolation-test",
	})
	if r2.Status != 200 && r2.Status != 201 {
		t.Errorf("用例5 对照组：A 自己的密码也进不去（HTTP %d），夹具或 join 接口有问题\n  响应 %s",
			r2.Status, r2.snippet())
	}
}

/* ══════════════════════════════════════════════════════════════════════════
   6：roomId 枚举探测
   ══════════════════════════════════════════════════════════════════════════ */

func TestRoomIDEnumeration(t *testing.T) {
	a, b, _ := sharedTenants(t)

	ids := []string{
		"r_0000", "r_aaaa", "r_zzzz", "r_1234", "not-a-room", strings.Repeat("x", 64),
	}
	if len(a.roomID) > 1 {
		ids = append(ids,
			a.roomID[:len(a.roomID)-1]+"0",
			a.roomID[:len(a.roomID)-1]+"z",
		)
	}
	secrets := []string{a.topic, b.topic}

	for _, id := range ids {
		if id == a.roomID || id == b.roomID {
			continue
		}
		for _, p := range []string{"/rooms/" + id, "/rooms/" + id + "/result"} {
			r := call(t, http.MethodGet, p, "", nil)
			expectStatus(t, r, http.StatusNotFound, "用例6：枚举探测 "+p+"，不存在的房间必须 404")
			for _, s := range secrets {
				if strings.Contains(string(r.Body), s) {
					t.Errorf("用例6：探测 %s 的响应体里出现了真实议题 %q（房间存在性泄漏）\n  响应 %s",
						p, s, r.snippet())
				}
			}
		}
	}
}

/* ══════════════════════════════════════════════════════════════════════════
   7：身份不可伪造
   ══════════════════════════════════════════════════════════════════════════ */

func TestIdentityCannotBeSpoofedByBody(t *testing.T) {
	a, _, _ := sharedTenants(t)
	waitMyTurn(t, a)

	const marker = "身份伪造探针"
	spoofed := map[string]any{
		"alias":   "tenantB-agent", // 冒充别人的名字
		"agentId": "m_999",         // 冒充别人的 id
		"model":   "claude-sonnet",
		"author": map[string]any{
			"alias":     "tenantB-agent",
			"agentId":   "m_999",
			"type":      "human",
			"yourOwner": true,
			"verified":  "server",
		},
	}
	r := postMessage(t, a, marker, spoofed)
	if r.Status != 200 && r.Status != 201 {
		t.Fatalf("用例7 前置：A 正常发一条消息都失败（HTTP %d），无法验证身份\n  响应 %s",
			r.Status, r.snippet())
	}

	var found *message
	msgs := listMessages(t, a)
	for i := range msgs {
		if strings.Contains(msgs[i].Text, marker) {
			found = &msgs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("用例7：消息发出去了但读不回来，无法验证作者字段")
	}

	if found.AgentID != a.agentID {
		t.Errorf("用例7：作者 id 被 body 篡改成功。期望 %q（token 对应 agent），实际 %q",
			a.agentID, found.AgentID)
	}
	if found.Alias != a.alias {
		t.Errorf("用例7：作者昵称被 body 篡改成功。期望 %q，实际 %q（body 塞的是 tenantB-agent）",
			a.alias, found.Alias)
	}
}

/* ══════════════════════════════════════════════════════════════════════════
   8：adminKey 不是 agent 身份，不能发言
   ══════════════════════════════════════════════════════════════════════════ */

func TestAdminKeyCannotPostAsAgent(t *testing.T) {
	a, _, _ := sharedTenants(t)

	r := call(t, http.MethodPost, a.messagesPath(), a.adminKey, map[string]any{
		"text":  "我是管理员我说话",
		"opKey": newOpKey(),
	})
	expectOneOf(t, r, "用例8：用 adminKey 当 Bearer 发言，期望 401 或 404（人类不该有发言通道）",
		http.StatusUnauthorized, http.StatusNotFound)

	r2 := call(t, http.MethodPost, a.messagesPath(), "", map[string]any{
		"text":     "无 token 只带 adminKey",
		"opKey":    newOpKey(),
		"adminKey": a.adminKey,
	})
	expectOneOf(t, r2, "用例8b：无 Bearer、只把 adminKey 放 body 里发言，期望被拒",
		http.StatusUnauthorized, http.StatusNotFound, http.StatusForbidden)
}

/* ══════════════════════════════════════════════════════════════════════════
   9：token 不可拼接 / 篡改
   ══════════════════════════════════════════════════════════════════════════ */

func TestTokenTampering(t *testing.T) {
	a, b, _ := sharedTenants(t)

	half := len(a.agentToken) / 2
	cases := map[string]string{
		"改最后一位":      flipLastChar(a.agentToken),
		"截断一半":       a.agentToken[:half],
		"加前缀":        "x" + a.agentToken,
		"拼接 agentId": a.agentToken + a.agentID,
		"空 token":    "",
		"只有 agentId": a.agentID,
		"B 头 + A 尾":  b.agentToken[:half] + a.agentToken[half:],
	}

	for name, tok := range cases {
		r := call(t, http.MethodGet, a.inboxPath()+"?wait=0", tok, nil)
		if r.Status == 200 {
			t.Errorf("用例9：篡改方式「%s」竟然通过鉴权（HTTP 200）—— token 可被构造", name)
		} else if r.Status != 401 && r.Status != 404 {
			t.Logf("用例9：篡改方式「%s」返回 %d（既非放行也非 401/404，记录备查）", name, r.Status)
		}
	}
}

func flipLastChar(s string) string {
	if s == "" {
		return "x"
	}
	last := s[len(s)-1]
	if last == 'a' {
		return s[:len(s)-1] + "b"
	}
	return s[:len(s)-1] + "a"
}

/* ══════════════════════════════════════════════════════════════════════════
   10：未加入的房间一律 404（用第三个租户，排除"两两配对"的特例）
   ══════════════════════════════════════════════════════════════════════════ */

func TestUnjoinedRoomIsInvisible(t *testing.T) {
	_, _, c := sharedTenants(t)
	a, _, _ := sharedTenants(t)

	r := call(t, http.MethodGet, "/rooms/"+c.roomID+"/messages?since=0", a.agentToken, nil)
	expectStatus(t, r, http.StatusNotFound, "用例10：A 的 token 读完全无关的 C 房间消息，期望 404")

	r2 := call(t, http.MethodGet, "/rooms/"+a.roomID+"/messages?since=0", c.agentToken, nil)
	expectStatus(t, r2, http.StatusNotFound, "用例10b：C 的 token 读 A 的房间，期望 404")
}

/* ══════════════════════════════════════════════════════════════════════════
   11：公开结果页可以 200，但不能夹带凭证
   ══════════════════════════════════════════════════════════════════════════ */

func TestResultIsPublicButLeaksNothing(t *testing.T) {
	a, b, _ := sharedTenants(t)

	status, body := getResult(t, a.roomID)
	if status != 200 {
		t.Fatalf("用例11：结果页是产品明确设计的公开接口，期望 200，实际 %d\n  响应 %s",
			status, snippetOf(body))
	}

	lower := strings.ToLower(string(body))
	for _, key := range []string{"agenttoken", "adminkey", "password", "tokenhash"} {
		if strings.Contains(lower, key) {
			t.Errorf("用例11：公开结果页里出现凭证字段 %q\n  %s", key, snippetOf(body))
		}
	}
	for _, secret := range []string{a.agentToken, a.adminKey, a.password} {
		if secret != "" && strings.Contains(string(body), secret) {
			t.Errorf("用例11：公开结果页泄漏了真实凭证值（前缀 %s…）", firstN(secret, 8))
		}
	}
	if strings.Contains(string(body), b.topic) {
		t.Errorf("用例11：A 的结果页里出现了 B 的议题 %q", b.topic)
	}
}

/* ══════════════════════════════════════════════════════════════════════════
   12：同一 DB 文件内作用域 —— positions 只含本房间的 agent
   ══════════════════════════════════════════════════════════════════════════ */

func TestResultPositionsScopedToRoom(t *testing.T) {
	a, b, _ := sharedTenants(t)

	status, body := getResult(t, a.roomID)
	if status != 200 {
		t.Fatalf("用例12：结果页期望 200，实际 %d\n  响应 %s", status, snippetOf(body))
	}

	var res map[string]any
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("用例12：结果页不是 JSON 对象：%v\n  响应 %s", err, snippetOf(body))
	}

	rawPos, ok := res["positions"]
	if !ok {
		t.Fatalf("用例12：结果页没有 positions 字段，无法验证隔离。实际字段：%v", keysOf(res))
	}
	list, ok := rawPos.([]any)
	if !ok {
		t.Fatalf("用例12：positions 不是数组（%T）", rawPos)
	}

	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if alias, _ := obj["alias"].(string); alias == b.alias {
			t.Errorf("用例12：A 的 result.positions 里出现了 B 房间的 agent %q", alias)
		}
		if strings.Contains(fmt.Sprint(obj), b.topic) {
			t.Errorf("用例12：A 的 result.positions 里出现了 B 的议题内容：%v", obj)
		}
	}

	// 反向确认
	if _, bodyB := getResult(t, b.roomID); bodyB != nil {
		var resB map[string]any
		if json.Unmarshal(bodyB, &resB) == nil {
			if rawB, ok := resB["positions"].([]any); ok {
				for _, item := range rawB {
					if obj, ok := item.(map[string]any); ok {
						if alias, _ := obj["alias"].(string); alias == a.alias {
							t.Errorf("用例12b：B 的 result.positions 里出现了 A 房间的 agent %q", alias)
						}
					}
				}
			}
		}
	}
}

func snippetOf(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 360 {
		s = s[:360] + "…"
	}
	if s == "" {
		return "(空响应体)"
	}
	return s
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// firstN 取前 n 个字符。故意不用内置 min()，因为那是 Go 1.21 才有的，
// 被测模块的 go.mod 若声明更低版本会直接编译失败——测试不该被这种事绊住。
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
