package room_test

import (
	"context"
	"strings"
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestSplitAndKeyPoints 复现干净 agent 挖到的三个呈现 bug：
// 1) agreement=split 时不能写「最终选了」
// 2) 分歧段必须反映"有人没投票 / 参与度不足"
// 3) 任何发过言的 agent，keyPoints（positions[].summary）都不能为空
func TestSplitAndKeyPoints(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "要不要立刻把定价改成月付？", MaxAgents: 3, MaxRounds: 5, MaxMinutes: 600, TokenBudget: 5000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "测试员 Kimi")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "稳妥派-mock0")
	c := mustJoin(t, eng, tenant, rid, cr.Password, "激进派-mock1")

	// 第 1 轮：每人一条（注意 b/c 是"重复原话"型发言，用来验证 keyPoints 兜底）
	first := mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "我认为应该立刻改，因为续费率不是主要矛盾。", OpKey: "k1"})
	seqA := first.Seq
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "我倾向保守。", OpKey: "k2"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "拖下去的代价没人记账。", OpKey: "k3"})

	// 第 2 轮：三人各提一个方案
	p1, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{Title: "立刻改", Body: "直接切月付", OpKey: "q1"})
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := eng.Propose(ctx, tenant, rid, domain.HashToken(b.AgentToken), room.ProposeInput{Title: "再观察一个季度", Body: "先看数据", OpKey: "q2"})
	p3, _ := eng.Propose(ctx, tenant, rid, domain.HashToken(c.AgentToken), room.ProposeInput{Title: "小步灰度", Body: "20% 灰度", OpKey: "q3"})

	// 第 2、3 轮：只让 a 说话，b/c 每轮重复原话（模拟 low engagement）
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应保守派：续费率可以用灰度验证。", ReplyTo: intp(seqA), OpKey: "k4"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "我倾向保守。", ReplyTo: intp(seqA), OpKey: "k5"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "拖下去的代价没人记账。", ReplyTo: intp(seqA), OpKey: "k6"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "继续回应：灰度成本可控。", ReplyTo: intp(seqA), OpKey: "k7"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "我倾向保守。", ReplyTo: intp(seqA), OpKey: "k8"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "拖下去的代价没人记账。", ReplyTo: intp(seqA), OpKey: "k9"})
	// 第 4 轮
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "再回应：我坚持立刻改。", ReplyTo: intp(seqA), OpKey: "k10"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "我倾向保守。", ReplyTo: intp(seqA), OpKey: "k11"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "拖下去的代价没人记账。", ReplyTo: intp(seqA), OpKey: "k12"})
	// 第 5 轮（收尾）
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "我支持立刻改。", SupportProposalID: p1.ProposalID, OpKey: "k13"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "我支持再观察一个季度。", SupportProposalID: p2.ProposalID, OpKey: "k14"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "我支持小步灰度。", SupportProposalID: p3.ProposalID, OpKey: "k15"})

	// 投票：a 投 p3，b/c 弃权 ⇒ 票数 p_3:1，其余 0，最高票 1 < 3/2
	mustVote(t, eng, tenant, rid, a.AgentToken, room.VoteInput{ProposalID: strp(p3.ProposalID), OpKey: "v1"})
	mustVote(t, eng, tenant, rid, b.AgentToken, room.VoteInput{ProposalID: nil, OpKey: "v2"})
	mustVote(t, eng, tenant, rid, c.AgentToken, room.VoteInput{ProposalID: nil, OpKey: "v3"})

	res := resultOf(t, eng, tenant, rid)
	report := room.OwnerReportOf(res, "http://x/room/1")

	t.Logf("agreement=%s votes=%+v", res.Conclusion.Agreement, res.Conclusion.Votes)
	t.Logf("结论文案：%s", res.Conclusion.Text)
	t.Logf("ownerReport:\n%s", report)
	for _, p := range res.Positions {
		t.Logf("keyPoints[%s] = %q", p.Alias, p.Summary)
	}

	// Bug 1：split 不得写「最终选了」
	if res.Conclusion.Agreement == "split" {
		if strings.Contains(report, "最终选了") {
			t.Fatalf("split 时 ownerReport 不能出现「最终选了」:\n%s", report)
		}
		if !strings.Contains(report, "未形成多数") {
			t.Fatalf("split 时应写「未形成多数」:\n%s", report)
		}
	}
	// Bug 2：参与度事实
	if !strings.Contains(report, "未投票") {
		t.Fatalf("有人没投票时分歧段应说明:\n%s", report)
	}
	if !strings.Contains(report, "参与度不足") {
		t.Fatalf("最高票 1/3 时应提示参与度不足:\n%s", report)
	}
	// Bug A：KeyPoints 数组也必须非空（含"只发 text、不发 finalStance"的形态）
	for _, p := range res.Positions {
		if len(p.KeyPoints) == 0 {
			t.Fatalf("%s 发过言但 keyPoints 是空数组 —— 不许留空", p.Alias)
		}
	}
	// Bug 3：keyPoints 非空
	for _, p := range res.Positions {
		if strings.TrimSpace(p.Summary) == "" {
			t.Fatalf("%s 的 keyPoints 不能为空", p.Alias)
		}
	}
}
