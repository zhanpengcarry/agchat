package verify

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"agchat/internal/oplog"
)

// chainResult 是第①步（逐条链校验）的结果。
//
// 语义约定（C-7）：
//   - failed=true → 有**可证明**的破损：断链、h 与重算不符、签名验不过。判决 failed。
//   - reasons 非空而 failed=false → 有 op 的作者公钥无从获悉，只能"无法校验"。
//     这不是伪造的证据，最终判决降级为 unverifiable，绝不当成通过。
type chainResult struct {
	sigsOK, sigsTotal int
	failed            bool
	failAt            int
	code              string
	reasons           []string
}

func (c *chainResult) fail(at int, code, reason string) {
	c.failed, c.failAt, c.code = true, at, code
	c.reasons = append(c.reasons, reason)
}

// checkChain 逐条校验 op 链：序号连续 → ph 接上一条 h → h = 重算值 → 作者签名。
//
// 公钥表只从 op 流自身建立（docs/09 §4.1 / B-13）：
//   - create 的载荷带 roomKey 公钥（`key`）——S1 没有 hostKey，宿主用 roomKey 签；
//   - join 的载荷带 `agentPub` —— 该座位之后的 op 全用它验。
//
// 绝不回查数据库、绝不重新执行 Join（B-13）。
func checkChain(ops []oplog.Op) chainResult {
	res := chainResult{}
	prev := strings.Repeat("0", 64) // 首条的 ph 必须是 64 个 0
	pubs := map[string][]ed25519.PublicKey{}

	for idx, o := range ops {
		res.sigsTotal++
		pos := idx + 1
		if o.I != pos {
			res.fail(pos, CodeChainBroken,
				fmt.Sprintf("第 %d 条 op 的链内序号 i=%d 与位置 %d 不一致：op 被插入、删除或重排过（断链）",
					pos, o.I, pos))
			return res
		}
		if !strings.EqualFold(o.PH, prev) {
			res.fail(o.I, CodeChainBroken,
				fmt.Sprintf("第 %d 条 op 断链：ph=%s，但上一条的 h=%s", o.I, shortHex(o.PH), shortHex(prev)))
			return res
		}

		// 先登记本 op 自带的公钥，再验本 op 的签名（create/join 是公钥首次出现的地方）。
		switch o.O {
		case "create":
			keys := createAuthorKeys(o)
			if len(keys) > 0 {
				pubs[o.A] = keys
			}
		case "join":
			// ★ join op 的作者是 "host"（座位/ord 是宿主分配的，agent 在这一刻还
			// 没法签它），真正的新公钥属于**载荷里的 agentId**：该座位之后的
			// post/propose/vote/summary 都用 agentPub 验。
			if seat, k := joinSeatAndPub(o); k != nil && seat != "" {
				pubs[seat] = []ed25519.PublicKey{k}
			}
		}

		// h 必须等于按 docs/09 §4 的域重算出来的值：改一个字节就会变。
		h, err := oplog.HashOp(o)
		if err != nil {
			res.fail(o.I, CodeOpHashMismatch,
				fmt.Sprintf("第 %d 条 op 的载荷无法规范化（%v）：op 内容被改过或不是本工具能读的版本", o.I, err))
			return res
		}
		if !strings.EqualFold(h, o.H) {
			res.fail(o.I, CodeOpHashMismatch,
				fmt.Sprintf("第 %d 条 op 的 h 与重算不符：记录 %s，重算 %s —— op 内容（含载荷）被改过",
					o.I, shortHex(o.H), shortHex(h)))
			return res
		}

		keys := pubs[o.A]
		if len(keys) == 0 {
			// 无从校验 ≠ 伪造：记下来，最终判 unverifiable。
			res.reasons = append(res.reasons,
				fmt.Sprintf("第 %d 条 op 的作者座位 %q 没有任何公钥可查（create 的 roomKey 或 join 的 agentPub 缺失）：这条签名无从校验",
					o.I, o.A))
			prev = o.H
			continue
		}
		ok := false
		var lastErr error
		for _, k := range keys {
			if lastErr = oplog.VerifySig(o.H, o.S, k); lastErr == nil {
				ok = true
				break
			}
		}
		if !ok {
			res.fail(o.I, CodeSigInvalid,
				fmt.Sprintf("第 %d 条 op 的签名验不过（作者 %q）：%v", o.I, o.A, lastErr))
			return res
		}
		res.sigsOK++
		prev = o.H
	}
	return res
}

// createAuthorKeys 返回 create op 作者（宿主）的候选公钥：
// 依次是 roomKey 公钥（`key`，S1 用 roomKey 签宿主 op）与可选的 `agentPub`。
func createAuthorKeys(o oplog.Op) []ed25519.PublicKey {
	var p struct {
		Key      string `json:"key"`
		AgentPub string `json:"agentPub"`
		Pub      string `json:"pub"`
	}
	if err := decodePayload(o.P, &p); err != nil {
		return nil
	}
	var out []ed25519.PublicKey
	for _, s := range []string{p.Key, p.AgentPub, p.Pub} {
		if k := decodePub(s); k != nil {
			out = append(out, k)
		}
	}
	return out
}

// joinSeatAndPub 返回 join op 载荷里的 (agentId, agentPub)。
//
// 为什么不是 op.a：join 由宿主签（A="host"），因为座位号/顺序是宿主分配的，
// agent 那一刻还没有这些信息可签。座位身份只认载荷里的 agentId + agentPub。
func joinSeatAndPub(o oplog.Op) (string, ed25519.PublicKey) {
	var p struct {
		AgentID  string `json:"agentId"`
		AgentPub string `json:"agentPub"`
		Pub      string `json:"pub"`
	}
	if err := decodePayload(o.P, &p); err != nil {
		return "", nil
	}
	k := decodePub(p.AgentPub)
	if k == nil {
		k = decodePub(p.Pub)
	}
	seat := p.AgentID
	if seat == "" {
		seat = o.A // 容错：极早期版本可能省略 agentId
	}
	return seat, k
}

// roomKeyOf 从 create op 里取 roomKey 公钥（用来验 ack 收据，C-4）。
func roomKeyOf(ops []oplog.Op) ed25519.PublicKey {
	for _, o := range ops {
		if o.O != "create" {
			continue
		}
		var p struct {
			Key string `json:"key"`
		}
		if err := decodePayload(o.P, &p); err != nil {
			return nil
		}
		if k := decodePub(p.Key); k != nil {
			return k
		}
		return nil
	}
	return nil
}

// decodePub 解析公钥：base64url_nopad 优先，兼容带 padding 的 base64 与 hex。
// 全仓公钥只应有一种写法的（base64url_nopad）；这里的宽容只是为了不在
// "宿主写错编码"时把整份证据包判死（那种情况应由结构性 reason code 报出来）。
func decodePub(s string) ed25519.PublicKey {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil && len(b) == ed25519.PublicKeySize {
		return ed25519.PublicKey(b)
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil && len(b) == ed25519.PublicKeySize {
		return ed25519.PublicKey(b)
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil && len(b) == ed25519.PublicKeySize {
		return ed25519.PublicKey(b)
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == ed25519.PublicKeySize {
		return ed25519.PublicKey(b)
	}
	return nil
}

// shortHex 把长 hash 截成前 12 后 4，便于人读输出里指认。
func shortHex(h string) string {
	if len(h) <= 20 {
		return h
	}
	return h[:12] + "…" + h[len(h)-4:]
}
