package room_test

import (
	"context"
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// sumVotes 返回 voteTotals 各项之和。
func sumVotes(res *room.Result) int {
	n := 0
	for _, v := range res.Conclusion.VoteTotals {
		n += v
	}
	return n
}

// TestExplicitNullCountsAsAbstain 规格 §五：proposalId:null 表示弃权。
//
// 曾经 TallyDetail 只统计"投给某个方案"的票，显式 null 的弃权票被整个吞掉 ——
// 3 个人投了 3 次，计数里只有 1 票，结论可信度被严重高估。
func TestExplicitNullCountsAsAbstain(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()

	// 3 人、1 个方案（作者 = 甲）
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "显式弃权", MaxAgents: 3, MaxRounds: 3, MaxMinutes: 600, TokenBudget: 5000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	c := mustJoin(t, eng, tenant, rid, cr.Password, "丙")

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲立场。", OpKey: "x1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙立场。", OpKey: "x2"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "丙立场。", OpKey: "x3"})
	p1, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "唯一方案", Body: "方案正文", OpKey: "xp",
	})
	if err != nil {
		t.Fatalf("提案失败: %v", err)
	}
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应乙。", ReplyTo: intp(2), OpKey: "x4"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(1), OpKey: "x5"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(1), OpKey: "x6"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "支持唯一方案。", SupportProposalID: p1.ProposalID, OpKey: "x7"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "也支持。", SupportProposalID: p1.ProposalID, OpKey: "x8"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "也支持。", SupportProposalID: p1.ProposalID, OpKey: "x9"})

	// 甲（作者）回避自投 → 显式 null；乙也显式 null；丙投 p_1
	mustVote(t, eng, tenant, rid, a.AgentToken, room.VoteInput{ProposalID: nil, OpKey: "ya"})
	mustVote(t, eng, tenant, rid, b.AgentToken, room.VoteInput{ProposalID: nil, OpKey: "yb"})
	mustVote(t, eng, tenant, rid, c.AgentToken, room.VoteInput{ProposalID: strp(p1.ProposalID), OpKey: "yc"})

	res := resultOf(t, eng, tenant, rid)
	t.Logf("voteTotals=%+v（期望 abstain=2, p_1=1, 合计=3）", res.Conclusion.VoteTotals)

	if got := res.Conclusion.VoteTotals["abstain"]; got != 2 {
		t.Fatalf("abstain 应为 2（作者回避自投 1 + 乙显式弃权 1），实际 %d", got)
	}
	if got := res.Conclusion.VoteTotals[p1.ProposalID]; got != 1 {
		t.Fatalf("p_1 应为 1，实际 %d", got)
	}
	if got := sumVotes(res); got != 3 {
		t.Fatalf("voteTotals 各项之和应等于投票人数 3，实际 %d（有票被吞了）", got)
	}
}

// TestVoteTotalsSumInvariant 不变量：任何房间，voteTotals 之和 == 实际投票人数。
//
// 这条比逐条断言强：它能一次性挡住所有"投票被吞"的形态。
func TestVoteTotalsSumInvariant(t *testing.T) {
	eng, tenant := newEnv(t)
	f := setupVotingRoom(t, eng, tenant) // 3 人

	// 每人投一次（含一人弃权、一人改票）
	mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[1]), OpKey: "i1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "i2"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: nil, OpKey: "i2b"}) // 改票成弃权
	mustVote(t, eng, tenant, f.roomID, f.tokens[2], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "i3"})

	res := resultOf(t, eng, tenant, f.roomID)
	t.Logf("改票后 voteTotals=%+v（3 人各 1 票，合计必须是 3）", res.Conclusion.VoteTotals)
	if got := sumVotes(res); got != 3 {
		t.Fatalf("voteTotals 之和应为 3（改票只算最后一次），实际 %d: %+v", got, res.Conclusion.VoteTotals)
	}
	if got := res.Conclusion.VoteTotals["abstain"]; got != 1 {
		t.Fatalf("乙改票成弃权后 abstain 应为 1，实际 %d", got)
	}
}
