// Command agchat 是 S1 的本地宿主 CLI：一个二进制同时扮演
// 「宿主进程」（agchat host serve，通常由 room new/resume 拉起）与「客户端」（join/post/...）。
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agchat/internal/host"
	"agchat/internal/invite"
	"agchat/internal/oplog"
	"agchat/internal/room"
)

const (
	exitOK        = 0
	exitFail      = 1
	exitUsage     = 2
	exitConsent   = 3 // join 没带 --yes：需确认，未进场（pm P1 / AC2）
	exitWrongRoom = 4 // 第三类失败：这是另一场会的卡（docs/09 §2.4）
)

var b64 = base64.RawURLEncoding

// verifyHook 由 cmd/agchat/verify.go（eng-verify 提供）在 init 里注册。
// 这里不写同名函数，避免与它冲突；它落地后 main 里的 verify 分支会自动生效。
var verifyHook func([]string) int

// ---- 数据目录与配置 --------------------------------------------------------

func loadCfg() host.Config {
	cfg := host.DefaultConfig()
	cfg.Home = host.HomeDir()
	cfg.DBPath = filepath.Join(cfg.Home, "agchat.db")
	return cfg
}

// ---- HTTP 客户端 ------------------------------------------------------------

type client struct {
	base  string
	token string
	http  *http.Client
}

func newClient(base, token string) *client {
	return &client{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 60 * time.Second}}
}

func (c *client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Human string `json:"human"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		if e.Human != "" {
			msg = e.Human
		}
		return &apiError{Status: resp.StatusCode, Code: e.Error.Code, Msg: msg}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type apiError struct {
	Status int
	Code   string
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

// healthz 探活（/healthz 不含任何会议数据）。
func healthz(addr string) bool {
	cli := &http.Client{Timeout: 2 * time.Second}
	resp, err := cli.Get("http://" + addr + "/healthz")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == 200
}

// ensureHost 保证这个 home 里有一个宿主在跑，返回它的监听地址。
//
// 已有宿主（home 级 rendezvous + /healthz 通）→ 直接复用（幂等）。
// 没有 → 拉起一个 detached 的 `agchat host serve` 子进程，等它写 rendezvous。
func ensureHost(cfg host.Config, addr string, exposeLAN bool) (string, error) {
	if r, err := host.ReadHomeRendezvous(cfg); err == nil && r.Addr != "" && healthz(r.Addr) {
		return r.Addr, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	args := []string{"host", "serve"}
	if addr != "" {
		args = append(args, "--addr", addr)
	}
	if exposeLAN {
		args = append(args, "--expose-lan")
	}
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return "", err
	}
	logPath := filepath.Join(cfg.Home, "host.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = cfg.Home
	cmd.Env = os.Environ()
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return "", err
	}
	_ = cmd.Process.Release()
	_ = f.Close()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := host.ReadHomeRendezvous(cfg); err == nil && r.Addr != "" && healthz(r.Addr) {
			return r.Addr, nil
		}
		time.Sleep(80 * time.Millisecond)
	}
	return "", fmt.Errorf("宿主没能在 15 秒内起来，看 %s 里的日志", logPath)
}

// ctl 返回带控制面 token 的客户端。
func ctl(cfg host.Config, addr string) (*client, error) {
	tok, err := host.ControlToken(cfg)
	if err != nil {
		return nil, err
	}
	return newClient("http://"+addr, tok), nil
}

// ---- 座位凭据（客户端侧）----------------------------------------------------

// seatCred 是 seats/<ref>.key 的内容（0600）。
//
// ★ 它是 **agent 自己的**凭证：agentKey 私钥、bearer token、端点。
// 座位靠它认回（A-18/A-19），不靠 alias —— alias 只是展示名。
type seatCred struct {
	V         int      `json:"v"`
	Ref       string   `json:"ref"`
	AgentID   string   `json:"agentId"`
	Alias     string   `json:"alias"`
	Token     string   `json:"token"`
	AgentPub  string   `json:"agentPub"`
	AgentPriv string   `json:"agentPriv"`
	Caps      []string `json:"caps"`
	Endpoint  string   `json:"endpoint"`
	InviteID  string   `json:"inviteId"`
	JoinedAt  int64    `json:"joinedAt"`
}

func (c seatCred) priv() (ed25519.PrivateKey, error) {
	b, err := b64.DecodeString(c.AgentPriv)
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("seats 文件里的私钥坏了，请重新 join")
	}
	return ed25519.PrivateKey(b), nil
}

func loadSeat(cfg host.Config, ref string) (*seatCred, error) {
	var sc seatCred
	if err := readJSONFile(cfg.SeatPath(ref), &sc); err != nil {
		return nil, fmt.Errorf("找不到本机座位凭据 %s（先 agchat join）: %w", cfg.SeatPath(ref), err)
	}
	return &sc, nil
}

func saveSeat(cfg host.Config, sc *seatCred) error {
	return writeJSONFile(cfg.SeatPath(sc.Ref), sc, 0o600)
}

// appendReceipt 把 ack 追加到 seats/<ref>.receipts.jsonl（arch C-4）。
func appendReceipt(cfg host.Config, ref string, ack *oplog.Ack) {
	if ack == nil {
		return
	}
	b, err := json.Marshal(ack)
	if err != nil {
		return
	}
	f, err := os.OpenFile(cfg.ReceiptsPath(ref), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(b, '\n'))
}

// ---- 小工具 ----------------------------------------------------------------

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// 去掉 BOM：本工具自己写不带 BOM，但用户拿记事本 / PowerShell 改过一遍就会有，
	// 而 json.Unmarshal 见到 BOM 直接报错——那种失败看起来像"凭据没了"，很难查。
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	return json.Unmarshal(b, v)
}

func writeJSONFile(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// resolveRef 在没给 --ref 时：本 home 下恰好一场会就用它。
func resolveRef(cfg host.Config, ref string) (string, error) {
	if strings.TrimSpace(ref) != "" {
		return ref, nil
	}
	entries, err := os.ReadDir(filepath.Join(cfg.Home, "rooms"))
	if err != nil {
		return "", errors.New("没有指定 --ref，且这个 home 里还没有任何房间（先 agchat room new）")
	}
	var refs []string
	for _, e := range entries {
		if e.IsDir() && fileExists(filepath.Join(cfg.Home, "rooms", e.Name(), "host.key")) {
			refs = append(refs, e.Name())
		}
	}
	switch len(refs) {
	case 0:
		return "", errors.New("没有指定 --ref，且这个 home 里还没有任何房间（先 agchat room new）")
	case 1:
		return refs[0], nil
	}
	return "", fmt.Errorf("这个 home 里有 %d 场会，请用 --ref 指定（%s）", len(refs), strings.Join(refs, ", "))
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func nowMS() int64 { return time.Now().UnixMilli() }

// newAgentKey 生成 seats 里的 agentKey。
func newAgentKey() (pub ed25519.PublicKey, priv ed25519.PrivateKey, err error) {
	return ed25519.GenerateKey(rand.Reader)
}

// printJSON 打印单行 JSON（--json 时 stdout 只出它，人话走 stderr）。
func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	return exitFail
}

// joinCardFromArg 支持 --card 传文件路径或整段卡文本。
func joinCardFromArg(arg string) (string, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", false
	}
	if !strings.Contains(arg, invite.BeginMarker) && fileExists(arg) {
		b, err := os.ReadFile(arg)
		if err == nil {
			return string(b), true
		}
	}
	return arg, true
}

// cardEndpoint 选一个可用端点（loopback 优先；--expose-lan 时 LAN 也可）。
func cardEndpoint(c *invite.Card) string {
	for _, ep := range c.P.EP {
		if ep.K == invite.EndpointLoopback {
			return ep.U
		}
	}
	if len(c.P.EP) > 0 {
		return c.P.EP[0].U
	}
	return ""
}

// roomState 是 GET /v1/head 的响应（客户端签名前要它）。
type headResp struct {
	Ref     string        `json:"ref"`
	AgentID string        `json:"agentId"`
	Head    host.HeadView `json:"head"`
	State   string        `json:"state,omitempty"`
	Round   int           `json:"round,omitempty"`
	Seq     int           `json:"seq,omitempty"`
}

// signOp 用 agentKey 签一条 op（i/t/ph 由作者填，宿主只验签不重签 —— oplog.SealOp 的约定）。
func signOp(priv ed25519.PrivateKey, h struct {
	I int
	H string
}, kind, agentID string, payload map[string]any, round, seq int) (oplog.Op, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return oplog.Op{}, err
	}
	op := oplog.Op{
		I: h.I + 1, T: nowMS(), O: kind, A: agentID,
		P: raw, PH: h.H,
	}
	return oplog.SealOp(op, priv)
}

var _ = context.Background
var _ = room.ResultSchemaVersion
