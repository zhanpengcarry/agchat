// Package audit 把审计日志落到文件（JSONL 追加），用于进程重启后仍留凭证。
//
// 设计取舍：
//   - **只追加**（O_APPEND|O_CREATE|O_WRONLY）：审计日志不允许被就地改写；
//   - **每条 fsync**（可关）：崩机/断电时最后一条也在盘上。代价是吞吐，
//     但审计事件本来就是低频的（只发生在凭证被拒时），不值得为它牺牲可追溯性；
//   - **不做轮转**：这是运维的事，生产上请配 logrotate。代码里做轮转要处理
//     并发、重命名、句柄失效，收益远小于风险；
//   - **打开失败让启动失败**：审计日志静默失效比没有审计更危险 ——
//     你以为有记录，真出事时才发现一条都没落盘。
package audit

import (
	"fmt"
	"io"
	"os"
	"sync"
)

// Writer 是一个带锁 + 可选 fsync 的追加写入器。
type Writer struct {
	mu     sync.Mutex
	f      *os.File
	doSync bool
}

// Open 打开（或创建）审计日志文件。配置了却打不开时应当让进程启动失败。
func Open(path string, doSync bool) (*Writer, error) {
	if path == "" {
		return nil, fmt.Errorf("audit: 审计日志路径为空")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("打开审计日志 %s: %w", path, err)
	}
	return &Writer{f: f, doSync: doSync}, nil
}

// Write 追加一行。写失败时把错误同时打到 stderr —— 调用方（slog）会吞掉 error，
// 不主动报出来就没人知道审计链断了。
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.f.Write(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit: 写审计日志失败（审计链已断）: %v\n", err)
		return n, err
	}
	if w.doSync {
		if err := w.f.Sync(); err != nil {
			fmt.Fprintf(os.Stderr, "audit: fsync 审计日志失败: %v\n", err)
			return n, err
		}
	}
	return n, nil
}

// Close 关闭文件。
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Multi 把审计同时写到 stdout 和文件（stdout 方便容器日志采集，文件保证重启不丢）。
func Multi(stdout io.Writer, w *Writer) io.Writer {
	if w == nil {
		return stdout
	}
	return io.MultiWriter(stdout, w)
}
