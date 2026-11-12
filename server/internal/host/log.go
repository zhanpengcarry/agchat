package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"agchat/internal/oplog"
)

// commitRec 是 ops.jsonl 里的 **commit 标记**（arch R-4：日志先行 + commit 标记）。
//
// 为什么要它：引擎的事务提交与日志追加不在同一个事务里（R-4 禁止给引擎加钩子），
// 崩在中间会留下"签了但未生效"的尾巴。导出 bundle 时只取**已提交前缀**，
// 恢复时把未提交尾巴截掉。
//
// 字段解释：
//
//	commit 对应 op 的链内序号 i
//	ok     engine 是否接受了这条 op（false = 这条是 reject 或被拒）
//	opKey 幂等键（post/propose/vote/summary 在外层请求里，不在签名载荷里；
//	      它是宿主侧的幂等索引，不参与链哈希）
type commitRec struct {
	Commit int             `json:"commit"`
	OK     bool            `json:"ok"`
	Code   string          `json:"code,omitempty"`
	Agent  string          `json:"agent,omitempty"`
	Kind   string          `json:"kind,omitempty"`
	OpKey  string          `json:"opKey,omitempty"`
	Seq    int             `json:"seq,omitempty"`
	Res    json.RawMessage `json:"res,omitempty"`
	Ack    *oplog.Ack      `json:"ack,omitempty"`
	T      int64           `json:"t,omitempty"`
}

// opLog 是**一场会一个** ops.jsonl：只追加、一行一个 op。
//
// 内存里只保留已提交前缀；文件里可能还有崩溃留下的尾巴，加载时截断。
type opLog struct {
	path string

	mu      sync.Mutex
	ops     []oplog.Op
	commits map[int]commitRec
	acks    []oplog.Ack
	idem    map[string]commitRec // agent\x00kind\x00opKey → commit
	f       *os.File
	size    int64
}

func idemKey(agent, kind, opKey string) string { return agent + "\x00" + kind + "\x00" + opKey }

// loadOpLog 读文件、重建链、截掉未提交尾巴。
func loadOpLog(path string) (*opLog, error) {
	l := &opLog{path: path, commits: map[int]commitRec{}, idem: map[string]commitRec{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		b = nil // 新房间：文件还不存在，下面会创建
	}

	var lastCommitEnd int64
	var offset int64
	for len(b) > 0 {
		nl := bytes.IndexByte(b, '\n')
		if nl < 0 {
			break // 没有换行的最后半行 = 崩溃尾巴，忽略
		}
		line := bytes.TrimSpace(b[:nl])
		lineEnd := offset + int64(nl) + 1
		if len(line) > 0 {
			var probe map[string]json.RawMessage
			if err := json.Unmarshal(line, &probe); err != nil {
				// 坏行：到这里之前的内容视为可读前缀，之后整段丢弃。
				break
			}
			if _, isCommit := probe["commit"]; isCommit {
				var cr commitRec
				if err := json.Unmarshal(line, &cr); err != nil {
					break
				}
				l.commits[cr.Commit] = cr
				if cr.Ack != nil {
					l.acks = append(l.acks, *cr.Ack)
				}
				if cr.OpKey != "" {
					l.idem[idemKey(cr.Agent, cr.Kind, cr.OpKey)] = cr
				}
				lastCommitEnd = lineEnd
			} else if _, isOp := probe["i"]; isOp {
				var op oplog.Op
				if err := json.Unmarshal(line, &op); err != nil {
					break
				}
				l.ops = append(l.ops, op)
			}
		}
		offset = lineEnd
		b = b[nl+1:]
	}

	// 只保留已提交前缀：所有 i 大于"最后一个 commit"的 op 都丢掉。
	maxCommitted := 0
	for i := range l.commits {
		if i > maxCommitted {
			maxCommitted = i
		}
	}
	kept := l.ops[:0]
	for _, op := range l.ops {
		if op.I <= maxCommitted {
			kept = append(kept, op)
		}
	}
	l.ops = kept
	// 文件里如果还有尾巴，截掉。
	if fi, err := os.Stat(path); err == nil && fi.Size() > lastCommitEnd {
		f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		if err := f.Truncate(lastCommitEnd); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	l.f = f
	if fi, err := f.Stat(); err == nil {
		l.size = fi.Size()
	}
	return l, nil
}

func (l *opLog) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// head 返回当前已提交的链头。
func (l *opLog) head() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return oplog.HeadOf(l.ops)
}

func (l *opLog) nextIndex() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	maxOp := 0
	for _, op := range l.ops {
		if op.I > maxOp {
			maxOp = op.I
		}
	}
	maxCommit := 0
	for i := range l.commits {
		if i > maxCommit {
			maxCommit = i
		}
	}
	// 兜底：老日志里可能只有 commit 没有 op 行（v1 的 bug），
	// 这时用 commit 的最大序号继续，避免链内序号重号。
	if maxCommit > maxOp {
		return maxCommit + 1
	}
	return maxOp + 1
}

func (l *opLog) opsCopy() []oplog.Op {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]oplog.Op, len(l.ops))
	copy(out, l.ops)
	return out
}

func (l *opLog) acksCopy() []oplog.Ack {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]oplog.Ack, len(l.acks))
	copy(out, l.acks)
	return out
}

func (l *opLog) lookupIdem(agent, kind, opKey string) (commitRec, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.idem[idemKey(agent, kind, opKey)]
	return c, ok
}

// intent 追加一行 op（日志先行）。返回写入前的偏移，失败时用于回滚。
func (l *opLog) intent(op oplog.Op) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line, err := json.Marshal(op)
	if err != nil {
		return 0, err
	}
	off := l.size
	if err := l.appendLine(line); err != nil {
		return off, err
	}
	return off, nil
}

// rollback 把文件截回 intent 之前的长度（被拒的 op 不留尾巴）。
func (l *opLog) rollback(off int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	if err := l.f.Truncate(off); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.size = off
	return nil
}

// commit 追加 commit 标记，并把这条 op 纳入"已提交前缀"。
func (l *opLog) commit(op oplog.Op, cr commitRec) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cr.Commit = op.I
	line, err := json.Marshal(cr)
	if err != nil {
		return err
	}
	if err := l.appendLine(line); err != nil {
		return err
	}
	l.ops = append(l.ops, op)
	l.commits[op.I] = cr
	if cr.Ack != nil {
		l.acks = append(l.acks, *cr.Ack)
	}
	if cr.OpKey != "" {
		l.idem[idemKey(cr.Agent, cr.Kind, cr.OpKey)] = cr
	}
	return nil
}

func (l *opLog) appendLine(line []byte) error {
	if l.f == nil {
		return fmt.Errorf("ops.jsonl 未打开")
	}
	buf := make([]byte, 0, len(line)+1)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	n, err := l.f.Write(buf)
	if err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.size += int64(n)
	return nil
}

// committedOps 返回导出用的已提交前缀（含 reject，它们的 ok=false）。
func (l *opLog) committedOps() []oplog.Op { return l.opsCopy() }
