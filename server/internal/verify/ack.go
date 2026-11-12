package verify

import (
	"crypto/ed25519"
	"fmt"
	"strings"

	"agchat/internal/oplog"
)

// ackCheck 是第④步（ack 收据覆盖度）的结果。
//
// 语义约定（C-4/C-5/C-7）：
//   - missing 非空 → 缺收据，判 unverifiable（"无从证明"，不是"证明有假"）；
//   - failed=true → 收据本身对不上：签名验不过、ref 不是这场会的。判 failed；
//   - unverifiable 非空 → 有收据但没有 roomKey 公钥，签不了名（同样 unverifiable）。
type ackCheck struct {
	note         string
	missing      []int
	failed       bool
	failAt       int
	code         string
	reason       string
	unverifiable string
}

// checkAcks 数 ack 收据的覆盖度并用 roomKey 验签（C-4）。
//
// 需要收据的是**参与者**的 op：join 之后由该座位签的 post/propose/vote/summary。
// 宿主的 create/advance/end/invite 不在其中 —— 参与者手上不会有自己没发的 op 的收据，
// 要求它反而会让每份 legit bundle 都判 unverifiable。
func checkAcks(ops []oplog.Op, acks []oplog.Ack, roomKeyPub ed25519.PublicKey, manifestRef string) ackCheck {
	var out ackCheck

	seats := map[string]bool{}
	for _, o := range ops {
		if o.O != "join" {
			continue
		}
		// join op 的作者是 "host"，座位身份取载荷里的 agentId（见 joinSeatAndPub）。
		if seat, _ := joinSeatAndPub(o); seat != "" && seat != "host" {
			seats[seat] = true
		}
	}
	bySeq := map[int]oplog.Ack{}
	for _, a := range acks {
		bySeq[a.I] = a
	}

	required, covered := 0, 0
	for _, o := range ops {
		if o.O == "reject" || !seats[o.A] {
			continue
		}
		required++
		a, ok := bySeq[o.I]
		if !ok || !strings.EqualFold(a.H, o.H) {
			out.missing = append(out.missing, o.I)
			continue
		}
		covered++
		if manifestRef != "" && a.Ref != "" && a.Ref != manifestRef {
			out.failed = true
			out.failAt = o.I
			out.code = CodeAckRefMismatch
			out.reason = fmt.Sprintf(
				"第 %d 条 op 的 ack 收据 ref=%q 不是本场会（manifest.ref=%q）：这张收据不能用",
				o.I, a.Ref, manifestRef)
			return out
		}
		if roomKeyPub == nil {
			out.unverifiable = "bundle 里有 ack 收据，但 create 载荷未提供 roomKey 公钥：收据签名无从校验（C-7）"
			continue
		}
		if err := oplog.VerifyAck(a, roomKeyPub); err != nil {
			out.failed = true
			out.failAt = o.I
			out.code = CodeAckSigInvalid
			out.reason = fmt.Sprintf("第 %d 条 op 的 ack 收据签名验不过：%v", o.I, err)
			return out
		}
	}

	switch {
	case len(acks) == 0:
		out.note = "bundle 未附 ack 收据（acks.jsonl 缺失）：无法判断宿主是否漏记参与者 op"
	case required == 0:
		out.note = "没有需要收据的参与者 op"
	default:
		out.note = fmt.Sprintf("ack 收据 %d 条，覆盖参与者 op %d/%d", len(acks), covered, required)
	}
	return out
}
