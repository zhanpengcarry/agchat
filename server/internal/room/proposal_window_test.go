package room_test

import (
	"context"
	"strings"
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestProposalAllowedInFinalRound 收尾轮也能提方案。
//
// 实测事故（干净 agent 报的）：3 轮房的方案窗口本来只有第 2 轮，而第 2 轮每人只有
// 1 条发言额度 —— **最后一个发言的人一开口，轮次推进、窗口关闭**，他的方案永远提不出来。
// 那场会因此只剩一个方案，投票退化成"给唯一选项盖章"，对手整场的方案直接消失。
//
// 规则本意只是"第 1 轮先亮立场、别急着上方案"，所以窗口改成「第 2 轮到进入投票为止」。
func TestProposalAllowedInFinalRound(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "3 轮房能不能在收尾轮提方案", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")

	// 第 1 轮
	r1 := mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲的立场。", OpKey: "w1"})
	r2 := mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙的立场。", OpKey: "w2"})
	// 第 2 轮（原来的唯一窗口）：两人都发完，轮次推进到第 3 轮
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应乙。", ReplyTo: intp(r2.Seq), OpKey: "w3"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(r1.Seq), OpKey: "w4"})

	res := resultOf(t, eng, tenant, rid)
	if res.Cost.Rounds != 3 {
		t.Fatalf("应已进入第 3 轮（收尾轮），实际第 %d 轮（state=%s）", res.Cost.Rounds, res.State)
	}

	// 收尾轮提案必须成功 —— 这是"最后一个发言的人"唯一能补上方案的机会
	p, err := eng.Propose(ctx, tenant, rid, domain.HashToken(b.AgentToken), room.ProposeInput{
		Title: "乙的方案（收尾轮补交）", Body: "由乙在下周一前完成，成功标准是延期率下降。", OpKey: "w5",
	})
	if err != nil {
		t.Fatalf("收尾轮提方案应被允许，实际 %v", err)
	}
	if p.ProposalID == "" {
		t.Fatal("收尾轮提方案没有返回 proposalId")
	}

	// 第 1 轮仍然不许提（这是规则本意，别顺手放开）
	eng2, tenant2 := newEnv(t)
	cr2 := mustCreate(t, eng2, tenant2, room.CreateInput{
		Topic: "第 1 轮不许提方案", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid2 := cr2.Room.ID
	a2 := mustJoin(t, eng2, tenant2, rid2, cr2.Password, "甲")
	mustJoin(t, eng2, tenant2, rid2, cr2.Password, "乙")
	if _, err := eng2.Propose(context.Background(), tenant2, rid2, domain.HashToken(a2.AgentToken), room.ProposeInput{
		Title: "太早的方案", Body: "第 1 轮就上方案。", OpKey: "x1",
	}); err == nil {
		t.Fatal("第 1 轮不该允许提方案（本轮只亮立场）")
	} else if codeOf(err) != "wrong_round" {
		t.Fatalf("第 1 轮提方案应回 403 wrong_round，实际 %v", err)
	}
}

// TestSingleProposalDisagreementIsScoped 只有 1 个方案时，「主要分歧」只能断言"方案层面"。
//
// 实测事故：一场只有 1 个方案、发言里却硬碰了三处（失败信号放当场还是事后、结论页是不是
// 权威源）的会议，ownerReport 上写的是「本议题只有 1 个方案，无竞争。」—— 主人读到这句
// 会以为大家没分歧。机械聚合断不了语义，那就把话说小：只说方案层面，并指向能看分歧的地方。
func TestSingleProposalDisagreementIsScoped(t *testing.T) {
	pid := "p_1"
	res := &room.Result{
		State: domain.StateEnded,
		Conclusion: room.Conclusion{
			Winner: &room.WinnerInfo{ProposalID: pid, Title: "唯一方案", OwnerAlias: "甲"},
			Votes:  map[string]room.VoteCount{pid: {Total: 2}},
		},
		Proposals: []room.ProposalView{{ProposalID: pid, Title: "唯一方案", By: "甲", Votes: 2}},
		Positions: []room.Position{{Alias: "甲", VotedFor: &pid}, {Alias: "乙", VotedFor: &pid}},
	}
	line := room.DisagreementLine(res)
	t.Logf("单一方案的分歧文案：%s", line)

	if strings.Contains(line, "无竞争。") {
		t.Fatalf("不能直接说「无竞争」，那是把机械字段当成了「没有分歧」：%s", line)
	}
	for _, want := range []string{"无方案层面的竞争", "各方立场"} {
		if !strings.Contains(line, want) {
			t.Fatalf("单一方案的分歧文案应包含 %q（把断言限定在方案层面并指明去哪看分歧）：%s", want, line)
		}
	}
	// 多方案时仍是原来的口径（不出现获胜方案自己的标题）
	res.Proposals = append(res.Proposals, room.ProposalView{ProposalID: "p_2", Title: "落选方案", By: "乙", Votes: 1})
	res.Conclusion.Votes["p_2"] = room.VoteCount{Total: 1}
	line2 := room.DisagreementLine(res)
	if strings.Contains(line2, "唯一方案") {
		t.Fatalf("分歧段不许出现获胜方案标题：%s", line2)
	}
}
