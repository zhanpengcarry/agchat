package verify

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agchat/internal/oplog"
	"agchat/internal/room"
)

// =============================================================================
// 测试数据构造器：只用 internal/oplog 直接造证据包，**不依赖 host 跑起来**。
//
// 一场 2 人小会：create → invite → 2×join → 2×post → 2×propose → advance →
// 2×vote → summary → end(快照)。所有时间戳都是写死的，同一份 fixture 每次
// 封出来的 op 字节完全一致。
//
// 声明值（verify-target 里的两个指纹）用**真实的** room.BuildResultForVerify
// 与 oplog.TranscriptHash 在干净状态下算一次，之后篡改不再更新它 —— 这样
// "重算 vs 声明"比的才是 host 与 verify 之间真正的契约，而不是自说自话。
// =============================================================================

const (
	fixtureRef   = "ref_verify_demo"
	fixtureTopic = "月付定价"
	fixtureBaseT = int64(1765000000000)
)

type step struct {
	seat    string
	op      string
	payload map[string]any
	t       int64
}

type fixture struct {
	t              *testing.T
	ref            string
	roomPub        string
	roomPriv       ed25519.PrivateKey
	seatPub        map[string]string
	seatPriv       map[string]ed25519.PrivateKey
	steps          []step
	ops            []oplog.Op
	acks           []oplog.Ack
	decisionHash   string
	transcriptHash string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("生成 roomKey 失败：%v", err)
	}
	f := &fixture{
		t:        t,
		ref:      fixtureRef,
		roomPub:  b64(pub),
		roomPriv: priv,
		seatPub:  map[string]string{"host": b64(pub)},
		seatPriv: map[string]ed25519.PrivateKey{"host": priv},
	}
	for _, seat := range []string{"m_1", "m_2"} {
		spub, spriv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatalf("生成 %s 的 agentKey 失败：%v", seat, err)
		}
		f.seatPub[seat] = b64(spub)
		f.seatPriv[seat] = spriv
	}
	return f
}

func b64(pub ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(pub)
}

func (f *fixture) add(seat, op string, payload map[string]any) {
	f.steps = append(f.steps, step{
		seat: seat, op: op, payload: payload,
		t: fixtureBaseT + int64(len(f.steps))*1000,
	})
}

// buildClean 铺一场干净的小会（载荷键名严格按 docs/09 §4.1 v1.1）。
func (f *fixture) buildClean() {
	f.add("host", "create", map[string]any{
		"ref": f.ref, "key": f.roomPub, "topic": fixtureTopic, "seats": 2,
		"maxRounds": 6, "maxAgents": 3, "maxMinutes": 30, "maxOutput": 800,
		"tokenBudget": 9000,
		"ids":         []any{map[string]any{"id": "iv_1", "uses": 2, "exp": 0}},
		"rv":          map[string]any{"res": 2, "vote": 1, "hash": 1},
		"hn":          "本机", "on": "周岭",
	})
	f.add("host", "invite", map[string]any{"id": "iv_1", "uses": 2, "exp": 0})
	// join 由宿主签（座位/ord 是宿主分配的），座位身份在载荷 agentId + agentPub 里。
	f.add("host", "join", map[string]any{
		"inviteId": "iv_1", "agentPub": f.seatPub["m_1"], "agentId": "m_1",
		"alias": "阿蓝", "ord": 0, "joinKey": "jk_m1",
	})
	f.add("host", "join", map[string]any{
		"inviteId": "iv_1", "agentPub": f.seatPub["m_2"], "agentId": "m_2",
		"alias": "阿绿", "ord": 1, "joinKey": "jk_m2",
	})
	f.add("m_1", "post", map[string]any{
		"seq": 1, "round": 1, "agentId": "m_1", "alias": "阿蓝", "text": "我主张按月付，先给 9 折。",
	})
	f.add("m_2", "post", map[string]any{
		"seq": 2, "round": 1, "agentId": "m_2", "alias": "阿绿", "text": "我担心月付的现金流压力。",
	})
	f.add("m_1", "propose", map[string]any{
		"id": "p_1", "seq": 3, "agentId": "m_1", "alias": "阿蓝",
		"title": "月付 9 折", "body": "月付 99 元，年付不折。",
	})
	f.add("m_2", "propose", map[string]any{
		"id": "p_2", "seq": 4, "agentId": "m_2", "alias": "阿绿",
		"title": "月付 8.8 折 + 随时退", "body": "月付 88 元，支持随时退。",
	})
	f.add("host", "advance", map[string]any{
		"reason": "rounds_done", "deadlineHit": true, "allVoted": false,
		"votesSeen": []any{}, "seatLastOpSeq": map[string]any{"m_1": 3, "m_2": 4},
	})
	f.add("m_1", "vote", map[string]any{
		"agentId": "m_1", "alias": "阿蓝", "proposalId": "p_2", "reason": "可退更重要",
	})
	f.add("m_2", "vote", map[string]any{
		"agentId": "m_2", "alias": "阿绿", "proposalId": "p_2", "reason": "我自己提的",
	})
	f.add("m_2", "summary", map[string]any{
		"agentId": "m_2", "alias": "阿绿", "text": "结论：月付 8.8 折 + 随时退。",
	})
	f.add("host", "end", map[string]any{"snapshot": map[string]any{
		"state": "ended", "round": 2, "emptyRounds": 0, "roundSkipped": false,
		"endReason": "summary_done", "convergenceReason": "rounds_done",
		"winnerProposalId": "p_2",
		"phaseStartedAt":   fixtureBaseT + 9000,
		"startedAt":        fixtureBaseT + 2000,
		"endedAt":          fixtureBaseT + 12000,
		"seq":              4,
		"planSkipped":      []any{},
		"summaryText":      "结论：月付 8.8 折 + 随时退。",
		"summaryAuthor":    "m_2",
		"summaryAt":        fixtureBaseT + 11000,
	}})
}

// seal 按序封 op（h/ph/签名全由 oplog 的唯一实现算），再为参与者 op 签 ack。
func (f *fixture) seal() {
	f.t.Helper()
	prev := oplog.ZeroHash
	ops := make([]oplog.Op, 0, len(f.steps))
	for i, s := range f.steps {
		op, err := oplog.Seal(f.seatPriv[s.seat], prev, i+1, s.t, s.op, s.seat, s.payload)
		if err != nil {
			f.t.Fatalf("封第 %d 条 op 失败：%v", i+1, err)
		}
		ops = append(ops, op)
		prev = op.H
	}
	f.ops = ops
	f.sealAcks()
}

func (f *fixture) sealAcks() {
	f.t.Helper()
	f.acks = nil
	for _, op := range f.ops {
		if op.A != "m_1" && op.A != "m_2" {
			continue // 宿主的 create/invite/advance/end 不在参与者收据范围内
		}
		a, err := oplog.SignAck(f.roomPriv, f.ref, op.I, op.H, op.T)
		if err != nil {
			f.t.Fatalf("签第 %d 条 op 的 ack 失败：%v", op.I, err)
		}
		f.acks = append(f.acks, a)
	}
}

// captureDeclared 记录"宿主 export 时写进 verify-target 的声明值"。
// 必须在干净状态下调用一次；之后的篡改都不再更新它。
func (f *fixture) captureDeclared() {
	f.t.Helper()
	rp, blk := replayBundle(f.ops)
	if blk != nil {
		f.t.Fatalf("干净 fixture 竟然重放不了：%s（%s）", blk.code, blk.reason)
	}
	if resultForVerify == nil {
		f.t.Fatal("纯聚合出口未接线")
	}
	res := resultForVerify(rp.agg, rp.now)
	if res == nil || res.DecisionSet == nil || res.DecisionSet.Hash == "" {
		f.t.Fatalf("聚合出口没有产出 decisionSet.hash（state=%s）：干净 fixture 必须能算出结论指纹",
			rp.agg.Room.State)
	}
	f.decisionHash = res.DecisionSet.Hash
	th, err := transcriptOf(f.ops, rp.agg)
	if err != nil {
		f.t.Fatalf("算 transcriptHash 失败：%v", err)
	}
	f.transcriptHash = th
}

func (f *fixture) bundle() *oplog.Bundle {
	return &oplog.Bundle{
		Manifest: oplog.Manifest{
			Ref: f.ref, ResultSchema: room.ResultSchemaVersion, VoteRule: "1",
			HashDomain: oplog.HashDomain, Seats: 2, InviteIDs: []string{"iv_1"},
			MaxRounds: 6, MaxAgents: 3, MaxMinutes: 30, MaxOutput: 800, Topic: fixtureTopic,
		},
		Ops:            f.ops,
		Acks:           f.acks,
		Result:         json.RawMessage(`{"schemaVersion":"2","decisionSet":{"hash":"` + f.decisionHash + `"}}`),
		DecisionHash:   f.decisionHash,
		TranscriptHash: f.transcriptHash,
	}
}

// stepIdx 返回某类 op 的第一条（0 基）。
func (f *fixture) stepIdx(op string) int {
	for i, s := range f.steps {
		if s.op == op {
			return i
		}
	}
	f.t.Fatalf("fixture 里没有 %s op", op)
	return -1
}

func (f *fixture) stepIdxOf(seat, op string) int {
	for i, s := range f.steps {
		if s.op == op && s.seat == seat {
			return i
		}
	}
	f.t.Fatalf("fixture 里没有 %s 发的 %s op", seat, op)
	return -1
}

// cleanFixture 返回一场封好、且已记录声明值的小会。
func cleanFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.buildClean()
	f.seal()
	f.captureDeclared()
	return f
}

// =============================================================================
// 必测 6 条 + 几条补强
// =============================================================================

func TestVerifyCleanPass(t *testing.T) {
	f := cleanFixture(t)

	rep, err := Run(f.bundle())
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.State != StateVerified {
		t.Fatalf("干净证据包应判 verified，实际 %s；code=%s reasons=%v", rep.State, rep.Code, rep.Reasons)
	}
	if !rep.DecisionMatch || !rep.TranscriptMatch {
		t.Fatalf("两条指纹都应一致，实际 decision=%v transcript=%v", rep.DecisionMatch, rep.TranscriptMatch)
	}
	if rep.SigsValid != rep.SigsTotal || rep.SigsTotal != len(f.ops) {
		t.Fatalf("签名应全部有效：%d/%d，op 数 %d", rep.SigsValid, rep.SigsTotal, len(f.ops))
	}
	if rep.AckNote == "" {
		t.Fatal("AckNote 不该为空：收据覆盖度必须报告出来")
	}
	if rep.ExitCode() != 0 {
		t.Fatalf("verified 的退出码应为 0，实际 %d", rep.ExitCode())
	}
	if rep.Code != CodeOK {
		t.Fatalf("verified 的主 reason code 应为 %q，实际 %q", CodeOK, rep.Code)
	}
}

// TestVerifyTamperedVoteFails：改一票（不重签）→ failed，且 FailAt 指向那条 op。
func TestVerifyTamperedVoteFails(t *testing.T) {
	f := cleanFixture(t)

	idx := f.stepIdxOf("m_1", "vote")
	f.ops[idx].P = json.RawMessage(`{"agentId":"m_1","alias":"阿蓝","proposalId":"p_1","reason":"被改过"}`)

	rep, err := Run(f.bundle())
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.State != StateFailed {
		t.Fatalf("改一票应判 failed，实际 %s（code=%s）", rep.State, rep.Code)
	}
	if rep.FailAt != idx+1 {
		t.Fatalf("FailAt 应指向被改的第 %d 条 op，实际 %d（reasons=%v）", idx+1, rep.FailAt, rep.Reasons)
	}
	if rep.Code != CodeOpHashMismatch {
		t.Fatalf("reason code 应为 %q，实际 %q", CodeOpHashMismatch, rep.Code)
	}
	if rep.ExitCode() != 1 {
		t.Fatalf("failed 的退出码应为 1，实际 %d", rep.ExitCode())
	}
}

// TestVerifyResignedVoteStillFails：改一票并**把整条链重新签对**（模拟宿主重写日志）
// → 链全绿，但结论指纹对不上（C-2：只按 decisionSet.hash 比，不看 ResultJSON 字节）。
func TestVerifyResignedVoteStillFails(t *testing.T) {
	f := cleanFixture(t)

	idx := f.stepIdxOf("m_1", "vote")
	f.steps[idx].payload["proposalId"] = "p_1"
	f.seal() // 重新封链 + 重新签 ack：签名与链都是"合法"的

	rep, err := Run(f.bundle())
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.SigsValid != rep.SigsTotal {
		t.Fatalf("重签后的链应该全绿（%d/%d），这是本用例的前提", rep.SigsValid, rep.SigsTotal)
	}
	if rep.State != StateFailed {
		t.Fatalf("重签过的篡改票应判 failed，实际 %s（code=%s）", rep.State, rep.Code)
	}
	if rep.DecisionMatch {
		t.Fatal("decisionSet.hash 应不一致：这是 decisionHash 覆盖票归属的直接证据")
	}
	if !rep.TranscriptMatch {
		t.Fatal("本用例没改发言正文，transcriptMatch 应仍为 true")
	}
	if rep.Code != CodeDecisionMismatch {
		t.Fatalf("reason code 应为 %q，实际 %q", CodeDecisionMismatch, rep.Code)
	}
	if rep.FailAt != endOpIndex(f.ops) {
		t.Fatalf("指纹级不一致的 FailAt 应指向 end op（%d），实际 %d", endOpIndex(f.ops), rep.FailAt)
	}
}

// TestVerifyTamperedTranscriptFails：改一条发言正文 → failed，且原因是 transcriptHash
// （C-1：decisionHash 根本不看正文，单靠它会在转写稿被换掉时报通过）。
func TestVerifyTamperedTranscriptFails(t *testing.T) {
	f := cleanFixture(t)

	idx := f.stepIdxOf("m_2", "post")
	f.steps[idx].payload["text"] = "我完全同意月付，没有顾虑。" // 原话被改写
	f.seal()                                       // 重封链：让"签名/断链"都无话可说，只剩转写指纹能抓

	rep, err := Run(f.bundle())
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.SigsValid != rep.SigsTotal {
		t.Fatalf("重封后的链应该全绿（%d/%d）", rep.SigsValid, rep.SigsTotal)
	}
	if rep.State != StateFailed {
		t.Fatalf("改一条发言应判 failed，实际 %s（code=%s）", rep.State, rep.Code)
	}
	if rep.TranscriptMatch {
		t.Fatal("transcriptHash 应不一致：转写稿被改过")
	}
	if !rep.DecisionMatch {
		t.Fatal("本用例没改票，decisionMatch 应仍为 true")
	}
	if rep.Code != CodeTranscriptMismatch {
		t.Fatalf("reason code 应为 %q，实际 %q", CodeTranscriptMismatch, rep.Code)
	}
}

// TestVerifyBrokenChainFails：断链 → failed，报第几条。
func TestVerifyBrokenChainFails(t *testing.T) {
	f := cleanFixture(t)

	const at = 3 // 0 基，即第 4 条 op
	f.ops[at].PH = strings.Repeat("f", 64)

	rep, err := Run(f.bundle())
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.State != StateFailed {
		t.Fatalf("断链应判 failed，实际 %s（code=%s）", rep.State, rep.Code)
	}
	if rep.FailAt != at+1 {
		t.Fatalf("FailAt 应指向断链的那条（第 %d 条），实际 %d", at+1, rep.FailAt)
	}
	if rep.Code != CodeChainBroken {
		t.Fatalf("reason code 应为 %q，实际 %q", CodeChainBroken, rep.Code)
	}
}

// TestVerifyMissingAckIsUnverifiable：缺 ack → unverifiable（不是 failed，C-7）。
func TestVerifyMissingAckIsUnverifiable(t *testing.T) {
	f := cleanFixture(t)

	b := f.bundle()
	b.Acks = nil

	rep, err := Run(b)
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.State != StateUnverifiable {
		t.Fatalf("缺 ack 应判 unverifiable，实际 %s（code=%s reasons=%v）", rep.State, rep.Code, rep.Reasons)
	}
	if rep.Code != CodeAckMissing {
		t.Fatalf("reason code 应为 %q，实际 %q", CodeAckMissing, rep.Code)
	}
	if !rep.DecisionMatch || !rep.TranscriptMatch {
		t.Fatal("指纹其实都对：缺收据不该被说成结论/转写不一致")
	}
	if rep.ExitCode() != 2 {
		t.Fatalf("unverifiable 的退出码应为 2，实际 %d", rep.ExitCode())
	}
}

// TestVerifyMissingAckForOneOpIsUnverifiable：只缺一条参与者 op 的收据也要降级。
func TestVerifyMissingAckForOneOpIsUnverifiable(t *testing.T) {
	f := cleanFixture(t)

	voteIdx := f.stepIdxOf("m_2", "vote")
	b := f.bundle()
	kept := make([]oplog.Ack, 0, len(b.Acks))
	for _, a := range b.Acks {
		if a.I != voteIdx+1 {
			kept = append(kept, a)
		}
	}
	b.Acks = kept

	rep, err := Run(b)
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.State != StateUnverifiable || rep.Code != CodeAckMissing {
		t.Fatalf("缺一条收据应判 unverifiable/ack_missing，实际 %s/%s", rep.State, rep.Code)
	}
	if !strings.Contains(strings.Join(rep.Reasons, " "), fmt.Sprintf("第 %d 条", voteIdx+1)) {
		t.Fatalf("Reasons 应指出缺的是第 %d 条 op，实际 %v", voteIdx+1, rep.Reasons)
	}
}

// TestVerifyForgedAckFails：收据签名对不上 → failed（收据本身是假证据）。
func TestVerifyForgedAckFails(t *testing.T) {
	f := cleanFixture(t)

	_, otherPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("生成诱饵密钥失败：%v", err)
	}
	op := f.ops[f.stepIdxOf("m_1", "post")]
	bad, err := oplog.SignAck(otherPriv, f.ref, op.I, op.H, op.T)
	if err != nil {
		t.Fatalf("签诱饵 ack 失败：%v", err)
	}
	b := f.bundle()
	for i := range b.Acks {
		if b.Acks[i].I == op.I {
			b.Acks[i] = bad
		}
	}

	rep, err := Run(b)
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.State != StateFailed || rep.Code != CodeAckSigInvalid {
		t.Fatalf("伪造收据应判 failed/ack_sig_invalid，实际 %s/%s（%v）", rep.State, rep.Code, rep.Reasons)
	}
	if rep.FailAt != op.I {
		t.Fatalf("FailAt 应指向那张收据对应的第 %d 条 op，实际 %d", op.I, rep.FailAt)
	}
}

func TestVerifyRuleVersionMismatchIsUnverifiable(t *testing.T) {
	f := cleanFixture(t)

	b := f.bundle()
	b.Manifest.ResultSchema = "1"

	rep, err := Run(b)
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.State != StateUnverifiable || rep.Code != CodeManifestVersionDiff {
		t.Fatalf("规则版本不匹配应判 unverifiable/manifest_version_mismatch，实际 %s/%s", rep.State, rep.Code)
	}
}

func TestVerifyMissingSnapshotIsUnverifiable(t *testing.T) {
	f := cleanFixture(t)

	// 把 end op 改造成"没有 snapshot"的 end，并重封链（结构合法但缺终态的流）。
	f.steps[f.stepIdx("end")].payload = map[string]any{"reason": "summary_done"}
	f.seal()

	rep, err := Run(f.bundle())
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.State != StateUnverifiable || rep.Code != CodeSnapshotMissing {
		t.Fatalf("缺终态快照应判 unverifiable/snapshot_missing，实际 %s/%s（%v）", rep.State, rep.Code, rep.Reasons)
	}
}

func TestVerifyEmptyBundleIsUnverifiable(t *testing.T) {
	rep, err := Run(&oplog.Bundle{})
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.State != StateUnverifiable || rep.Code != CodeBundleEmpty {
		t.Fatalf("空 bundle 应判 unverifiable/bundle_empty，实际 %s/%s", rep.State, rep.Code)
	}
}

// TestVerifyBundleFileRoundTrip：走 oplog 的 zip 写读，证明 CLI 的输入路径可用。
// 若设置 AGCHAT_VERIFY_DEMO_OUT，还会把这份包写到该路径（验收脚本用）。
func TestVerifyBundleFileRoundTrip(t *testing.T) {
	f := cleanFixture(t)
	want := *f.bundle()

	path := filepath.Join(t.TempDir(), "bundle.zip")
	if err := oplog.SaveBundle(path, want); err != nil {
		t.Fatalf("写 bundle.zip 失败：%v", err)
	}
	loaded, err := oplog.LoadBundle(path)
	if err != nil {
		t.Fatalf("读 bundle.zip 失败：%v", err)
	}
	rep, err := Run(loaded)
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.State != StateVerified {
		t.Fatalf("从 zip 读回来的干净包应判 verified，实际 %s/%s（%v）", rep.State, rep.Code, rep.Reasons)
	}
}

// TestWriteDemoBundles 只在设置了 AGCHAT_VERIFY_DEMO_OUT 时跑：把三份演示证据包
// （干净 / 改一票 / 缺收据）写到指定目录前缀，供 CLI 验收脚本直接喂给 agchat verify。
func TestWriteDemoBundles(t *testing.T) {
	prefix := os.Getenv("AGCHAT_VERIFY_DEMO_OUT")
	if prefix == "" {
		t.Skip("未设置 AGCHAT_VERIFY_DEMO_OUT：跳过演示包生成")
	}

	f := cleanFixture(t)
	cleanPath := prefix + ".zip"
	if err := oplog.SaveBundle(cleanPath, *f.bundle()); err != nil {
		t.Fatalf("写干净包失败：%v", err)
	}

	// 改一票（不改签名）：链上第 N 条 op 的 h 立刻对不上 —— CLI 应报第几条。
	tampered := cleanFixture(t)
	voteIdx := tampered.stepIdxOf("m_1", "vote")
	tampered.ops[voteIdx].P = json.RawMessage(
		`{"agentId":"m_1","alias":"阿蓝","proposalId":"p_1","reason":"被改过"}`)
	tamperedPath := prefix + "-tampered-vote.zip"
	if err := oplog.SaveBundle(tamperedPath, *tampered.bundle()); err != nil {
		t.Fatalf("写篡改包失败：%v", err)
	}

	// 缺收据：结论/转写都对，只是无从证明宿主没漏记 → unverifiable。
	noAck := cleanFixture(t)
	nb := noAck.bundle()
	nb.Acks = nil
	noAckPath := prefix + "-noack.zip"
	if err := oplog.SaveBundle(noAckPath, *nb); err != nil {
		t.Fatalf("写缺收据包失败：%v", err)
	}

	t.Logf("演示包：%s（clean）｜%s（第 %d 条 op 被改）｜%s（缺收据）",
		cleanPath, tamperedPath, voteIdx+1, noAckPath)
}

// =============================================================================
// G15 静态门禁（B-13 / B-12 的可执行形态）
// =============================================================================

// TestVerifyNoCreateJoinCalls 用 go/ast 扫 verify 路径的源码，断言：
//   - 不存在 Create(/Join( 调用（B-13：重放不许走引擎副作用入口）；
//   - 不存在 now() 调用、不 import crypto/rand（B-12：不造新时间/身份）。
//
// 扫的是**非测试源码**：测试里允许构造数据（那正是 fixture 的职责）。
func TestVerifyNoCreateJoinCalls(t *testing.T) {
	files := []string{"verify.go", "chain.go", "replay.go", "ack.go"}
	for _, name := range files {
		scanVerifySource(t, name)
	}
	// CLI 也在同一条门禁上：verify 子命令的入口不许把引擎副作用搬回来。
	scanVerifySource(t, filepath.Join("..", "..", "cmd", "agchat", "verify.go"))
}

func scanVerifySource(t *testing.T, path string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.AllErrors)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v", path, err)
	}
	at := func(n ast.Node) string {
		p := fset.Position(n.Pos())
		return fmt.Sprintf("%s:%d", p.Filename, p.Line)
	}

	for _, imp := range file.Imports {
		p := strings.Trim(imp.Path.Value, `"`)
		switch p {
		case "crypto/rand", "math/rand", "math/rand/v2":
			t.Errorf("%s: verify 路径不许 import %q（B-12：重放不造新身份/随机数）", at(imp), p)
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			switch fun.Sel.Name {
			case "Create", "Join":
				t.Errorf("%s: verify 路径不许调用 %s(...)（B-13：重放禁止走 Create/Join 副作用）",
					at(call), fun.Sel.Name)
			case "Now":
				t.Errorf("%s: verify 路径不许读真实时钟 Now()（B-12：时间只能来自 op）", at(call))
			}
		case *ast.Ident:
			if fun.Name == "now" {
				t.Errorf("%s: verify 路径不许调用 now()（B-12：时间只能来自 op）", at(call))
			}
		}
		return true
	})
}
