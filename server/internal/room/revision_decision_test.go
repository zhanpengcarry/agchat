package room_test

import (
	"context"
	"strings"
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// driveToProposalRound 把房间推到第 2 轮（方案窗口打开）：两人各发一条立场。
func driveToProposalRound(t *testing.T, eng *room.Engine, tenant, rid string, a, b *room.JoinResult) {
	t.Helper()
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲的立场。", OpKey: "r1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙的立场。", OpKey: "r2"})
}

// TestProposalRevisionSupersedes 方案可以被自己的新版本取代（正文不可改，但可以换版本）。
//
// 两轮多 agent 复验里，每场都有方案正文的条款被作者当场撤回，而正文改不了 ——
// 主人最后拿到的方案里可能写着会上已公认错误的机制。
func TestProposalRevisionSupersedes(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "修订链", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	driveToProposalRound(t, eng, tenant, rid, a, b)

	p1, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "方案 v1", Body: "含一条后来被撤回的机制。", OpKey: "p1",
	})
	if err != nil {
		t.Fatalf("首个提案失败: %v", err)
	}
	rev, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "方案 v2", Body: "去掉那条被撤回的机制，只留可证伪的验收。", OpKey: "p2", Revises: p1.ProposalID,
	})
	if err != nil {
		t.Fatalf("修订失败: %v", err)
	}
	if rev.ProposalID == p1.ProposalID {
		t.Fatal("修订应产生新的 proposalId，而不是原地改（正文必须可追溯）")
	}
	if rev.Supersedes != p1.ProposalID {
		t.Fatalf("修订版应记录 supersedes=%s，实际 %q", p1.ProposalID, rev.Supersedes)
	}
	if rev.Count != 1 {
		t.Fatalf("修订不该占用方案配额（根方案数应仍为 1），实际 %d", rev.Count)
	}

	// 旧版本被标记取代，新版本采纳中；result 里两个都在，带链条
	res := resultOf(t, eng, tenant, rid)
	byID := map[string]room.ProposalView{}
	for _, p := range res.Proposals {
		byID[p.ProposalID] = p
	}
	if byID[p1.ProposalID].SupersededBy != rev.ProposalID {
		t.Fatalf("旧版本应标记 supersededBy=%s，实际 %q", rev.ProposalID, byID[p1.ProposalID].SupersededBy)
	}
	if byID[rev.ProposalID].Supersedes != p1.ProposalID {
		t.Fatalf("新版本应记录 supersedes=%s，实际 %q", p1.ProposalID, byID[rev.ProposalID].Supersedes)
	}
	if byID[p1.ProposalID].Body == "" || byID[rev.ProposalID].Body == "" {
		t.Fatal("两个版本的正文都要保留（可追溯）")
	}

	// inbox 只列采纳中的版本（旧版不能再投）
	snap, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(a.AgentToken), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Proposals) != 1 || snap.Proposals[0].ProposalID != rev.ProposalID {
		t.Fatalf("inbox 应只列采纳中的 %s，实际 %+v", rev.ProposalID, snap.Proposals)
	}

	// 别人的方案不能修订
	if _, err := eng.Propose(ctx, tenant, rid, domain.HashToken(b.AgentToken), room.ProposeInput{
		Title: "越权修订", Body: "改别人的方案。", OpKey: "p3", Revises: rev.ProposalID,
	}); err == nil {
		t.Fatal("不该允许修订别人的方案")
	} else if codeOf(err) != "wrong_round" {
		t.Fatalf("越权修订应回 403 wrong_round，实际 %v", err)
	}
}

// TestRevisionLimitAndVoteRedirect 修订有上限；投给旧版本的票自动迁到新版本。
func TestRevisionLimitAndVoteRedirect(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "修订上限", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	driveToProposalRound(t, eng, tenant, rid, a, b)

	p1, _ := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{Title: "v1", Body: "正文一。", OpKey: "q1"})
	p2, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{Title: "v2", Body: "正文二。", OpKey: "q2", Revises: p1.ProposalID})
	if err != nil {
		t.Fatal(err)
	}
	p3, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{Title: "v3", Body: "正文三。", OpKey: "q3", Revises: p2.ProposalID})
	if err != nil {
		t.Fatalf("第二次修订应允许（上限 %d），实际 %v", room.MaxRevisionsPerProposal, err)
	}
	// 第三次修订必须被拒
	if _, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "v4", Body: "正文四。", OpKey: "q4", Revises: p3.ProposalID,
	}); err == nil {
		t.Fatalf("超过 %d 次修订应被拒", room.MaxRevisionsPerProposal)
	} else if codeOf(err) != "too_many_revisions" {
		t.Fatalf("应回 409 too_many_revisions，实际 %v", err)
	}

	// 推进到投票
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应乙。", ReplyTo: intp(2), OpKey: "q5"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(1), OpKey: "q6"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "收尾。", SupportProposalID: p3.ProposalID, OpKey: "q7"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "收尾。", SupportProposalID: p3.ProposalID, OpKey: "q8"})

	// 乙投给**最早那版** p_1：应自动改记到 p_3，并如实告知
	out, err := eng.Vote(ctx, tenant, rid, domain.HashToken(b.AgentToken), room.VoteInput{ProposalID: strp(p1.ProposalID), OpKey: "q9"})
	if err != nil {
		t.Fatalf("投给旧版本不该报错（应自动改投最新版），实际 %v", err)
	}
	if out.RedirectedTo != p3.ProposalID {
		t.Fatalf("应告知已改记到 %s，实际 %q", p3.ProposalID, out.RedirectedTo)
	}
	mustVote(t, eng, tenant, rid, a.AgentToken, room.VoteInput{ProposalID: strp(p3.ProposalID), OpKey: "q10"})

	res := resultOf(t, eng, tenant, rid)
	if res.Conclusion.Winner == nil || res.Conclusion.Winner.ProposalID != p3.ProposalID {
		t.Fatalf("采纳的应是最后一版 %s，实际 %+v", p3.ProposalID, res.Conclusion.Winner)
	}
	if res.Conclusion.Votes[p1.ProposalID].Total != 0 {
		t.Fatalf("旧版本不该再有票，实际 %+v", res.Conclusion.Votes[p1.ProposalID])
	}
}

// TestDecisionSetAndFingerprint 机读结论：结构化、可核对、指纹稳定。
func TestDecisionSetAndFingerprint(t *testing.T) {
	eng, tenant := newEnv(t)
	f := setupVotingRoom(t, eng, tenant) // 3 人、p_1 / p_2

	// 未结束：status=in_progress，无指纹
	mid := resultOf(t, eng, tenant, f.roomID)
	if mid.DecisionSet == nil || mid.DecisionSet.Status != "in_progress" {
		t.Fatalf("进行中应给 decisionSet.status=in_progress，实际 %+v", mid.DecisionSet)
	}
	if mid.DecisionSet.Hash != "" {
		t.Fatalf("进行中不该给结论指纹，实际 %q", mid.DecisionSet.Hash)
	}

	// 甲自投 + 乙丙跨投 → p_1 胜
	mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "h1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "h2"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[2], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "h3"})
	endIfSummarizing(t, eng, tenant, f.roomID, f.tokens)

	res := resultOf(t, eng, tenant, f.roomID)
	ds := res.DecisionSet
	if ds == nil {
		t.Fatal("结束后必须有 decisionSet")
	}
	if ds.Status != "decided" {
		t.Fatalf("3/3 一致应 status=decided，实际 %q", ds.Status)
	}
	if ds.Chosen == nil || ds.Chosen.ProposalID != f.props[0] {
		t.Fatalf("chosen 应是 %s，实际 %+v", f.props[0], ds.Chosen)
	}
	if ds.Chosen.Votes.Total != 3 || ds.Chosen.Votes.Self != 1 || ds.Chosen.Votes.Cross != 2 {
		t.Fatalf("chosen 的票数拆分不对：%+v", ds.Chosen.Votes)
	}
	if len(ds.Options) != 2 {
		t.Fatalf("应有两个候选选项，实际 %d", len(ds.Options))
	}
	if ds.Participation.Members != 3 || ds.Participation.Voted != 3 {
		t.Fatalf("参与度不对：%+v", ds.Participation)
	}
	if !strings.HasPrefix(ds.Hash, "sha256:") || len(ds.Hash) < 20 {
		t.Fatalf("指纹格式不对：%q", ds.Hash)
	}

	// 同一房间重复读 → 指纹必须一致
	again := resultOf(t, eng, tenant, f.roomID)
	if again.DecisionSet.Hash != ds.Hash {
		t.Fatalf("同一结论两次读取指纹不一致：%q vs %q", ds.Hash, again.DecisionSet.Hash)
	}

	// ownerReport 里也要带指纹
	report := room.OwnerReportOf(res, "http://x/room/1")
	if !strings.Contains(report, "结论指纹：sha256:") {
		t.Fatalf("ownerReport 应带结论指纹：\n%s", report)
	}
}
