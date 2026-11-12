package host

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---- 原子写 + 权限 ----------------------------------------------------------

// writeJSON 原子写一个 JSON 文件（先写临时文件再 rename）。
// secret=true 时权限 0600（密钥、控制面 token、座位凭证）。
func writeJSON(path string, v any, secret bool) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, secret)
}

func writeFileAtomic(path string, b []byte, secret bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if secret {
		mode = 0o600
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---- rendezvous（arch B-17）------------------------------------------------

// Rendezvous 是 rooms/<ref>/host.json 的形状（字段集由 arch B-17 固定）。
type Rendezvous struct {
	Ref       string `json:"ref"`
	PID       int    `json:"pid"`
	Addr      string `json:"addr"`
	StartedAt int64  `json:"startedAt"`
	Version   string `json:"version"`
}

// homeRendezvous 是 <Home>/host.json：让 CLI 找到"这个 home 里有没有宿主在跑"。
type homeRendezvous struct {
	PID       int    `json:"pid"`
	Addr      string `json:"addr"`
	StartedAt int64  `json:"startedAt"`
	Version   string `json:"version"`
}

func writeRendezvous(c Config, ref, addr string, startedAt int64) error {
	r := Rendezvous{Ref: ref, PID: os.Getpid(), Addr: addr, StartedAt: startedAt, Version: c.Version}
	if err := writeJSON(c.roomJSONPath(ref), r, false); err != nil {
		return err
	}
	// pidfile：kill 用（Windows 上也能按 pid 找进程）。
	if err := writeFileAtomic(c.roomPidPath(ref), []byte(strconv.Itoa(os.Getpid())), false); err != nil {
		return err
	}
	return nil
}

func writeHomeRendezvous(c Config, addr string, startedAt int64) error {
	return writeJSON(c.homeJSONPath(), homeRendezvous{
		PID: os.Getpid(), Addr: addr, StartedAt: startedAt, Version: c.Version,
	}, false)
}

// ReadRendezvous 读出某房间的 rendezvous 文件（CLI 找宿主用）。
func ReadRendezvous(c Config, ref string) (Rendezvous, error) {
	var r Rendezvous
	err := readJSON(c.roomJSONPath(ref), &r)
	return r, err
}

// ReadHomeRendezvous 读出 home 级 rendezvous。
func ReadHomeRendezvous(c Config) (Rendezvous, error) {
	var r homeRendezvous
	err := readJSON(c.homeJSONPath(), &r)
	if err != nil {
		return Rendezvous{}, err
	}
	return Rendezvous{PID: r.PID, Addr: r.Addr, StartedAt: r.StartedAt, Version: r.Version}, nil
}

// ---- 控制面 token（G13：~/.agchat/control.token，0600）----------------------

// ControlToken 读（必要时生成）控制面 token。
func ControlToken(c Config) (string, error) {
	p := c.controlTokenPath()
	if b, err := os.ReadFile(p); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s, nil
		}
	}
	tok, err := randomHex(32)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(p, []byte(tok), true); err != nil {
		return "", err
	}
	return tok, nil
}

// HasControlToken 只读检查是否已经存在（不生成）。
func HasControlToken(c Config) bool {
	b, err := os.ReadFile(c.controlTokenPath())
	return err == nil && strings.TrimSpace(string(b)) != ""
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ---- 进程存活/端口占用 ------------------------------------------------------

// pidAlive 判断 pid 是否还活着。
// 注意：不引入第三方库；Windows 上 os.FindProcess 永远成功，
// 所以用 "signal 0" 语义（syscall）不可移植 —— 这里靠调用 /healthz 做权威判定，
// pid 只用于"看起来还没退出"的快速过滤。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(os.Signal(nil)); err != nil {
		// Windows/Unix 对 nil signal 的行为不一致，忽略这里的错误。
		return true
	}
	return true
}

// ensureHome 建好数据目录骨架。
func ensureHome(c Config) error {
	for _, d := range []string{c.Home, c.roomsDir(), c.seatsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// opError 是宿主对外错误：带 HTTP 状态码与机器可读 code。
type opError struct {
	Status int
	Code   string
	Msg    string
	Detail map[string]any
}

func (e *opError) Error() string { return e.Code + ": " + e.Msg }

func newOpErr(status int, code, msg string) *opError {
	return &opError{Status: status, Code: code, Msg: msg}
}

func (e *opError) with(kv ...any) *opError {
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		if k != "" {
			e.Detail[k] = kv[i+1]
		}
	}
	return e
}

var errNotFound = &opError{Status: 404, Code: "not_found", Msg: "没有这个房间"}

// readAll 是 io.ReadAll 的别名（避免在多个文件里 import io）。
func readAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

// nowMillis 是宿主统一时钟入口（测试可覆盖）。
func nowMillis() int64 { return time.Now().UnixMilli() }

// 防止 errors 包在某些构建标签下未使用。
var _ = errors.Is
var _ = fmt.Sprintf
