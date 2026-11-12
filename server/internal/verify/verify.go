// Package verify 实现离线证据包的**纯函数重放与验签**（docs/09 §6，S1 = tier 1）。
//
// 它只做四件事（arch B-12/B-13/B-14、C-1~C-8、G15）：
//
//	① 逐条校验哈希链：ph / h / 签名（作者公钥从 create 的 roomKey 与各 join 的
//	   agentPub 建表）；断链、h 不符、伪签 → failed 并报第几条；
//	② 从 op 流重建 room.Agg（不执行 Create/Join/Post 的任何副作用，不生成任何
//	   新 ID/token/时间戳，不读真实时钟 —— 唯一的时间输入取自 op 自身）；
//	③ 用纯聚合出口 room.BuildResultForVerify 重算 decisionSet.hash 与
//	   transcriptHash，与 bundle 声明的值比对（C-1/C-2）；
//	④ 数 ack 收据的覆盖度并用 roomKey 验收据签名（C-4/C-5）。
//
// 输出三态（C-7）：verified / failed / unverifiable。**没有第四态**：说不清就写
// unverifiable，绝不允许把"不知道"写成"通过"。每条判决都带稳定 reason code（G15），
// 供验收脚本机读；Reasons 里是人话。
//
// 话术边界（C-6/C-8）：本包不得输出「已证明」「不可篡改」「可证明公正」
// 「宿主的每次排序都可重算」。允许的只有「结论可复算」「转写稿未被改」
// 「宿主干预 M 次，其中 X 次可被输入验证通过」。
package verify

import (
	"errors"
	"fmt"
	"strings"

	"agchat/internal/oplog"
	"agchat/internal/room"
)

// 三态（arch C-7）。取值就是 CLI 与人读输出里的字面量，别改名。
const (
	StateVerified     = "verified"
	StateFailed       = "failed"
	StateUnverifiable = "unverifiable"
)

// 稳定 reason code（G15：verify 输出必须带机读码，不许只有中文句子）。
// 新增码只许追加，不许改已有码的拼写 —— 验收脚本按字符串判。
const (
	CodeOK                   = "ok"
	CodeBundleEmpty          = "bundle_empty"
	CodeBundleUnreadable     = "bundle_unreadable"
	CodeManifestMalformed    = "manifest_malformed"
	CodeRuleVersionMissing   = "rule_version_missing"
	CodeManifestVersionDiff  = "manifest_version_mismatch"
	CodeChainBroken          = "chain_broken"
	CodeOpHashMismatch       = "op_hash_mismatch"
	CodeSigInvalid           = "sig_invalid"
	CodeSigKeyUnknown        = "sig_key_unknown"
	CodeReplayUnknownOp      = "replay_unknown_op"
	CodeAuthorMismatch       = "author_mismatch"
	CodeAdvanceReasonUnknown = "advance_reason_unknown"
	CodeSnapshotMissing      = "snapshot_missing"
	CodeSnapshotNotEnded     = "snapshot_not_ended"
	CodeFingerprintMissing   = "fingerprint_missing"
	CodeRecomputeUnavailable = "recompute_unavailable"
	CodeDecisionMismatch     = "decision_mismatch"
	CodeTranscriptMismatch   = "transcript_mismatch"
	CodeAckMissing           = "ack_missing"
	CodeAckSigInvalid        = "ack_sig_invalid"
	CodeAckRefMismatch       = "ack_ref_mismatch"
	CodeAckUnverifiable      = "ack_unverifiable"
)

// 允许句式（C-6）：结论指纹 / 转写指纹两条。改文案前先看 docs/09 §6 的话术禁令。
const (
	ReasonDecisionOK   = "结论可复算：decisionSet.hash 一致"
	ReasonTranscriptOK = "转写稿未被改：transcriptHash 一致"
)

// Report 是 verify 的判决。
//
// FailAt 是 **1-based 的 op 序号**（= op 的 i 字段）；0 表示无法定位到单条 op
// （例如指纹不一致 —— 那是"整条流重算"的结果，不是某一条 op 能单独背的）。
//
// Code 是主 reason code；Codes 是按发生顺序去重后的全部码（含 "ok" 之外的告警）。
// DecisionMatch/TranscriptMatch 只在"确实比过"时为 true —— 没比就是 false，
// 不允许用"没比成"冒充"一致"（C-7）。
type Report struct {
	State           string   `json:"state"`
	Code            string   `json:"code"`
	Codes           []string `json:"codes,omitempty"`
	DecisionMatch   bool     `json:"decisionMatch"`
	TranscriptMatch bool     `json:"transcriptMatch"`
	SigsValid       int      `json:"sigsValid"`
	SigsTotal       int      `json:"sigsTotal"`
	FailAt          int      `json:"failAt,omitempty"`
	Reasons         []string `json:"reasons"`
	AckNote         string   `json:"ackNote,omitempty"`
}

// OK 报告三态里的 verified。注意：只有 verified 才允许说"一致"。
func (r Report) OK() bool { return r.State == StateVerified }

// ExitCode 是 CLI 的退出码：0=verified / 1=failed / 2=unverifiable。
func (r Report) ExitCode() int {
	switch r.State {
	case StateVerified:
		return 0
	case StateFailed:
		return 1
	default:
		return 2
	}
}

// resultForVerify 是 internal/room 的**纯聚合出口**接线点。
//
// 契约：BuildResultForVerify(agg *Agg, now int64) *Result —— 语义等价 buildResult(agg)，
// 但**禁止读墙钟**（result.go 的 endedAt() 在 EndedAt==0 时会调 domain.NowMillis()，
// now 就是用来替换它的）。Health 不在 decisionHash 域内，故不参与比对（C-2）。
//
// 这里默认直接接上真实实现；测试可以临时替换（同包内赋值），生产路径不接受
// "没接线就报通过"——nil 时 Run 报 unverifiable（recompute_unavailable）。
var resultForVerify func(agg *room.Agg, now int64) *room.Result = room.BuildResultForVerify

// Run 对证据包做 tier 1 纯函数重放。
//
// 返回的 error 只表示"调用方给的东西没法判"（如 nil bundle），不表示判决失败；
// 判决本身永远在 Report.State 里（三态）。
func Run(b *oplog.Bundle) (Report, error) {
	rep := Report{State: StateUnverifiable, Code: CodeBundleEmpty, Reasons: []string{}}
	if b == nil {
		return rep, errors.New("verify: bundle 为 nil")
	}
	if len(b.Ops) == 0 {
		rep.Codes = addCode(rep.Codes, CodeBundleEmpty)
		rep.Reasons = append(rep.Reasons, "bundle 里没有任何 op：没有可重放的证据")
		return rep, nil
	}

	// ① 逐条链校验：ph / h / s。断链、h 对不上、签名验不过 = failed（必须报第几条）。
	ch := checkChain(b.Ops)
	rep.SigsValid, rep.SigsTotal = ch.sigsOK, ch.sigsTotal
	if ch.failed {
		rep.State, rep.FailAt = StateFailed, ch.failAt
		rep.Code = ch.code
		rep.Codes = addCode(rep.Codes, ch.code)
		rep.Reasons = append(rep.Reasons, ch.reasons...)
		// DecisionMatch/TranscriptMatch 保持 false = "没比成"，不是"比过且不一致"：
		// 链已破，重算谁都不能当证据（C-7）。这句必须在机读输出里说清楚，
		// 否则验收脚本会把 false 误读成"指纹不一致"。
		rep.Reasons = append(rep.Reasons,
			"因链校验未通过，两条指纹没有做比对（decisionMatch/transcriptMatch=false 表示未比对成功，不代表指纹不一致）")
		return rep, nil
	}
	// 作者公钥缺失是"无从校验"而非"伪造"（C-7）：先记账，最终判决降级为 unverifiable。
	unverifiedAuthors := ch.reasons

	// C-3 / B-10：manifest 必须钉住规则版本。对不上时「重算不符」就无法区分
	// 「被篡改」与「代码升级了」，此时说 verified 没有判决力。
	if code, why := checkRuleVersions(b.Manifest); why != "" {
		rep.Code = code
		rep.Codes = addCode(rep.Codes, code)
		rep.Reasons = append(rep.Reasons, why)
		return rep, nil
	}

	// ② 从 op 流纯函数重建 Agg（终态事实取 end op 里的快照，B-14）。
	rp, blk := replayBundle(b.Ops)
	if blk != nil {
		rep.Code, rep.FailAt = blk.code, blk.failAt
		rep.Codes = addCode(rep.Codes, blk.code)
		rep.Reasons = append(rep.Reasons, blk.reason)
		return rep, nil
	}

	// ③ 重算两条指纹并与 bundle 声明值比对（C-1/C-2）。
	if b.DecisionHash == "" || b.TranscriptHash == "" {
		rep.Code = CodeFingerprintMissing
		rep.Codes = addCode(rep.Codes, CodeFingerprintMissing)
		rep.Reasons = append(rep.Reasons,
			"bundle 未同时声明 decisionHash 与 transcriptHash：缺指纹就无法判决（C-3）")
		return rep, nil
	}
	calcDecision, calcTranscript, why := recompute(rp.agg, rp.now, b.Ops)
	if why != "" {
		rep.Code = CodeRecomputeUnavailable
		rep.Codes = addCode(rep.Codes, CodeRecomputeUnavailable)
		rep.Reasons = append(rep.Reasons, why)
		return rep, nil
	}
	rep.DecisionMatch = strings.EqualFold(calcDecision, b.DecisionHash)
	rep.TranscriptMatch = strings.EqualFold(calcTranscript, b.TranscriptHash)
	if !rep.DecisionMatch || !rep.TranscriptMatch {
		rep.State = StateFailed
		// 指纹不一致是"整条流重算"的结论，不是单条 op 能单独背的：
		// 把 FailAt 指向携带终态快照的 end op（声明值就锚在它身上）。
		rep.FailAt = endOpIndex(b.Ops)
		if !rep.DecisionMatch {
			rep.Code = CodeDecisionMismatch
			rep.Codes = addCode(rep.Codes, CodeDecisionMismatch)
			rep.Reasons = append(rep.Reasons,
				fmt.Sprintf("结论指纹不一致（decisionSet.hash）：重算 %s ≠ bundle 声明 %s",
					calcDecision, b.DecisionHash))
		} else {
			rep.Code = CodeTranscriptMismatch
			rep.Codes = addCode(rep.Codes, CodeTranscriptMismatch)
		}
		if !rep.TranscriptMatch {
			rep.Codes = addCode(rep.Codes, CodeTranscriptMismatch)
			rep.Reasons = append(rep.Reasons,
				fmt.Sprintf("转写指纹不一致（transcriptHash）：重算 %s ≠ bundle 声明 %s",
					calcTranscript, b.TranscriptHash))
		}
		return rep, nil
	}

	// ④ ack 覆盖度 + 收据签名（C-4/C-5）。缺 ack 是"无从证明"，不是"证明有假" → unverifiable。
	ak := checkAcks(b.Ops, b.Acks, roomKeyOf(b.Ops), b.Manifest.Ref)
	rep.AckNote = ak.note
	if ak.failed {
		rep.State, rep.FailAt = StateFailed, ak.failAt
		rep.Code = ak.code
		rep.Codes = addCode(rep.Codes, ak.code)
		rep.Reasons = append(rep.Reasons, ak.reason)
		return rep, nil
	}
	if len(ak.missing) > 0 {
		rep.State = StateUnverifiable
		rep.Code = CodeAckMissing
		rep.Codes = addCode(rep.Codes, CodeAckMissing)
		rep.Reasons = append(rep.Reasons,
			fmt.Sprintf("缺 %d 条 ack 收据（第 %s 条 op）：无法据此判断宿主是否漏记参与者 op（C-4/C-7）",
				len(ak.missing), idxList(ak.missing)))
		rep.Reasons = append(rep.Reasons, unverifiedAuthors...)
		return rep, nil
	}
	if ak.unverifiable != "" {
		rep.State = StateUnverifiable
		rep.Code = CodeAckUnverifiable
		rep.Codes = addCode(rep.Codes, CodeAckUnverifiable)
		rep.Reasons = append(rep.Reasons, ak.unverifiable)
		rep.Reasons = append(rep.Reasons, unverifiedAuthors...)
		return rep, nil
	}
	if len(unverifiedAuthors) > 0 {
		rep.State = StateUnverifiable
		rep.Code = CodeSigKeyUnknown
		rep.Codes = addCode(rep.Codes, CodeSigKeyUnknown)
		rep.Reasons = append(rep.Reasons, unverifiedAuthors...)
		return rep, nil
	}

	rep.State = StateVerified
	rep.Code = CodeOK
	rep.Codes = addCode([]string{CodeOK}, CodeOK)
	rep.Reasons = append(rep.Reasons,
		ReasonDecisionOK, ReasonTranscriptOK,
		fmt.Sprintf("op 签名 %d/%d 有效", rep.SigsValid, rep.SigsTotal))
	return rep, nil
}

// recompute 用纯聚合出口重算两条指纹。
// 返回的第三个值是"无法重算"的原因（空 = 成功）——这属于 unverifiable，不是 failed。
func recompute(agg *room.Agg, now int64, ops []oplog.Op) (decision, transcript, why string) {
	if resultForVerify == nil {
		return "", "", "本工具的纯聚合出口 room.BuildResultForVerify 尚未接线：无法复算结论指纹（C-7：不猜）"
	}
	res := resultForVerify(agg, now)
	if res == nil || res.DecisionSet == nil || res.DecisionSet.Hash == "" {
		return "", "", "重算没有产出 decisionSet.hash（会议未结束或没有结论），无法比对结论指纹"
	}
	th, err := transcriptOf(ops, agg)
	if err != nil {
		return "", "", "重算 transcriptHash 失败：" + err.Error()
	}
	return res.DecisionSet.Hash, th, ""
}

// checkRuleVersions 校验 manifest 是否钉住规则版本（C-3 / B-10）。
//
// 返回 (reason code, 人话)；人话为空表示通过。
// 注意：只有 resultSchema 有代码侧常量可逐字比对（room.ResultSchemaVersion）；
// voteRule/hashDomain 目前只做"必须存在"的自证检查 —— 它们的取值由 eng-host 在
// export 时写入，room 侧尚未导出对应常量。存在性检查仍能挡住"manifest 缺元数据"。
func checkRuleVersions(m oplog.Manifest) (code, why string) {
	if m.Ref == "" {
		return CodeManifestMalformed, "manifest 缺 roomRef：这不是一份可识别的证据包"
	}
	if m.ResultSchema == "" || m.VoteRule == "" || m.HashDomain == "" {
		return CodeRuleVersionMissing,
			"manifest 未钉住规则版本（C-3）：resultSchema/voteRule/hashDomain 三者都必须写明，" +
				"否则「重算不符」无法区分被篡改与代码升级"
	}
	if m.ResultSchema != room.ResultSchemaVersion {
		return CodeManifestVersionDiff,
			fmt.Sprintf("结果规则版本不匹配：bundle 声明 resultSchema=%q，本工具支持 %q —— 这是另一版规则办出来的会（C-3）",
				m.ResultSchema, room.ResultSchemaVersion)
	}
	return "", ""
}

// endOpIndex 返回携带终态快照的 end op 的 1-based 序号；没有则返回 0。
func endOpIndex(ops []oplog.Op) int {
	for _, o := range ops {
		if o.O == "end" {
			return o.I
		}
	}
	return 0
}

// addCode 追加一个 reason code（去重）。
func addCode(codes []string, c string) []string {
	for _, x := range codes {
		if x == c {
			return codes
		}
	}
	return append(codes, c)
}

// idxList 把若干 op 序号排成 "3, 4, 7" 的样子（调用方已按流顺序给）。
// 刻意不用 strings.Join：本包有一条可执行门禁（TestVerifyNoCreateJoinCalls）
// 禁止源码里出现 Create(/Join( 形态的调用，免得哪天有人顺手把引擎副作用搬回来。
func idxList(idx []int) string {
	var b strings.Builder
	for i, n := range idx {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d", n)
	}
	if b.Len() == 0 {
		return "无"
	}
	return b.String()
}
