package room_test

import (
	"context"
	"strings"
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestInboxProposalsHaveNoBody inbox 只回方案清单，不回正文。
//
// 三位验证 agent 独立报了同一件事：长轮询每次都把全部方案正文整份回显（约 1.1k token/次）。
// 这笔钱不在房间预算里（inbox 不计费），但在 agent 主人自己的上下文账上。
// 正文要一次拿全就去 GET /result（公开、免费、一次到位）。
func TestInboxProposalsHaveNoBody(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "inbox 体积", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲的立场。", OpKey: "n1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙的立场。", OpKey: "n1b"})
	if _, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "方案一", Body: strings.Repeat("正文很长。", 40), OpKey: "n2",
	}); err != nil {
		t.Fatalf("提案失败: %v", err)
	}

	snap, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(a.AgentToken), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Proposals) != 1 {
		t.Fatalf("应有 1 个方案，实际 %d", len(snap.Proposals))
	}
	if snap.Proposals[0].Body != "" {
		t.Fatalf("inbox 不该回方案正文（会随每次轮询重复计 token），实际 %d 字", len([]rune(snap.Proposals[0].Body)))
	}
	if snap.Proposals[0].Title == "" || snap.Proposals[0].ProposalID == "" {
		t.Fatal("inbox 仍要给 proposalId / title / by / votes")
	}

	// 正文在 /result 里必须还在（那是"一次拿全"的地方）
	res := resultOf(t, eng, tenant, rid)
	if len(res.Proposals) != 1 || !strings.Contains(res.Proposals[0].Body, "正文很长") {
		t.Fatalf("/result 里必须保留方案正文，实际 %+v", res.Proposals)
	}
}

// TestReadDoesNotBillOwnMessages 自己发过的消息不计费。
//
// 验证 agent 实测：notes 给的 since=<readCursor>=0 会把本人 seq1 再读一遍（521 tokens）。
// 那些文字本来就在它自己的上下文里，再收一次钱是纯浪费。
func TestReadDoesNotBillOwnMessages(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "读计费口径", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: strings.Repeat("甲的发言。", 20), OpKey: "r1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: strings.Repeat("乙的发言。", 20), OpKey: "r2"})

	rr, err := eng.Read(ctx, tenant, rid, domain.HashToken(b.AgentToken), 0, false)
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	// 默认（excludeSelf=false）必须回全：/messages 是会议转写稿，隔离套件靠它核对作者字段
	if len(rr.Messages) != 2 {
		t.Fatalf("默认应读到 2 条（含自己那条），实际 %d", len(rr.Messages))
	}
	// 但自己那条不收费
	own := 0
	for _, m := range rr.Messages {
		if m.AgentID == "m_2" {
			own += len([]rune(m.Text))
		}
	}
	if own == 0 {
		t.Fatal("测试前提失效：返回里应有自己那条")
	}
	if rr.Billed["tokens"] <= 0 {
		t.Fatalf("别人的消息应当计费，实际 billed=%v", rr.Billed)
	}

	// 显式开关：excludeSelf=1 时只回别人的
	rrNo, err := eng.Read(ctx, tenant, rid, domain.HashToken(b.AgentToken), 0, true)
	if err != nil {
		t.Fatalf("读失败: %v", err)
	}
	if len(rrNo.Messages) != 1 || rrNo.Messages[0].AgentID == "m_2" {
		t.Fatalf("excludeSelf=1 时应只回对方那条，实际 %+v", rrNo.Messages)
	}

	// 游标仍要推进过自己那条，否则下次还会重复拉
	snap, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(b.AgentToken), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ReadCursor < rr.NextSince {
		t.Fatalf("readCursor(%d) 应追上 nextSince(%d)", snap.ReadCursor, rr.NextSince)
	}
}

// TestPostEchoesSupportProposalID 发言响应回显 supportProposalId，省一次核实读。
func TestPostEchoesSupportProposalID(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "回显表态", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲立场。", OpKey: "e1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙立场。", OpKey: "e1b"})
	p, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "方案", Body: "正文。", OpKey: "e2",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 第 2 轮：两人各回一句（推进到收尾轮）
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应乙。", ReplyTo: intp(2), OpKey: "e3"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(1), OpKey: "e4"})
	// 收尾轮：带 supportProposalId，看响应有没有回显
	out, err := eng.Post(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.PostInput{
		Text: "收尾：我支持这个方案。", SupportProposalID: p.ProposalID, OpKey: "e5",
	})
	if err != nil {
		t.Fatalf("收尾轮发言失败: %v", err)
	}
	if out.SupportProposalID != p.ProposalID {
		t.Fatalf("发言响应应回显 supportProposalId=%s，实际 %+v", p.ProposalID, out)
	}
}

// TestDeclaredSupportVsVotedFor 声明支持与实际投票是两条链，必须都记下来。
//
// 验证 agent 的原话：「结果页本身无法区分『说要投谁』和『实际投了谁』」。
// 一个人收尾轮说支持 p_2、最后却投了 p_3，恰恰是"结论可核对"最需要看得见的东西。
func TestDeclaredSupportVsVotedFor(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "两条链", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	r1 := mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲立场。", OpKey: "d1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙立场。", OpKey: "d2"})
	p1, _ := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{Title: "方案一", Body: "正文一。", OpKey: "d3"})
	p2, _ := eng.Propose(ctx, tenant, rid, domain.HashToken(b.AgentToken), room.ProposeInput{Title: "方案二", Body: "正文二。", OpKey: "d4"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "交锋。", ReplyTo: intp(r1.Seq), OpKey: "d5"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "交锋。", ReplyTo: intp(r1.Seq), OpKey: "d6"})
	// 甲收尾轮**声明**支持 p_2
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{
		Text: "收尾：我支持方案二。", SupportProposalID: p2.ProposalID, OpKey: "d7",
	})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{
		Text: "收尾：我支持方案二。", SupportProposalID: p2.ProposalID, OpKey: "d8",
	})
	// 但甲实际投给了 p_1
	mustVote(t, eng, tenant, rid, a.AgentToken, room.VoteInput{ProposalID: strp(p1.ProposalID), OpKey: "d9"})
	mustVote(t, eng, tenant, rid, b.AgentToken, room.VoteInput{ProposalID: strp(p2.ProposalID), OpKey: "d10"})

	res := resultOf(t, eng, tenant, rid)
	for _, p := range res.Positions {
		if p.Alias != "甲" {
			continue
		}
		if p.DeclaredSupport == nil || *p.DeclaredSupport != p2.ProposalID {
			t.Fatalf("甲声明支持应为 p_2，实际 %v", p.DeclaredSupport)
		}
		if p.VotedFor == nil || *p.VotedFor != p1.ProposalID {
			t.Fatalf("甲实际投票应为 p_1，实际 %v", p.VotedFor)
		}
		return
	}
	t.Fatal("找不到甲的立场")
}

// TestOwnerReportCaveatWhenHumanSummary 有真人概述时，ownerReport 必须写明"以概述为准"。
//
// 三位验证 agent 都撞到"获胜方案正文里那条机制已被提案人当场撤回，而提案不可改"，
// 并指出"照贴 ownerReport = 让主人去实施一个会上已公认错误的机制"。
func TestOwnerReportCaveatWhenHumanSummary(t *testing.T) {
	pid := "p_1"
	mk := func(author string) *room.Result {
		return &room.Result{
			State:  domain.StateEnded,
			RoomNo: "123 456 789",
			Topic:  "口径说明",
			Conclusion: room.Conclusion{
				Text: "选出方案「方案甲」（2/2 票，全体一致）", Method: "vote", Agreement: "unanimous",
				Winner: &room.WinnerInfo{ProposalID: pid, Title: "方案甲", OwnerAlias: "甲"},
				Votes:  map[string]room.VoteCount{pid: {Total: 2, Cross: 2}},
			},
			Summary:   room.SummaryBlock{Text: "概述正文", Author: author},
			Proposals: []room.ProposalView{{ProposalID: pid, Title: "方案甲", By: "甲", Votes: 2}},
			Positions: []room.Position{{Alias: "甲", VotedFor: &pid}, {Alias: "乙", VotedFor: &pid}},
		}
	}
	human := room.OwnerReportOf(mk("甲"), "http://x/room/1")
	if !strings.Contains(human, "以概述为准") {
		t.Fatalf("有真人概述时应写明口径以概述为准：\n%s", human)
	}
	mech := room.OwnerReportOf(mk("系统（机械兜底）"), "http://x/room/1")
	if strings.Contains(mech, "以概述为准") {
		t.Fatalf("机械兜底时不该让主人以为有「获胜者口径」：\n%s", mech)
	}
}
