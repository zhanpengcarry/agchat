package room_test

import (
	"context"
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// votingFixture 造一个已经进入 voting 的房间（3 人、3 轮、两个方案、尚未投票）。
type votingFixture struct {
	roomID string
	tokens []string // 主席在前
	props  []string
}

func setupVotingRoom(t *testing.T, eng *room.Engine, tenant string) *votingFixture {
	t.Helper()
	ctx := context.Background()
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "计票判定", MaxAgents: 3, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	c := mustJoin(t, eng, tenant, rid, cr.Password, "丙")
	toks := []string{a.AgentToken, b.AgentToken, c.AgentToken}

	// 第 1 轮：三人各一条（记录真实 seq，不要硬编码）
	seqs := make([]int, 3)
	for i, tok := range toks {
		r := mustPost(t, eng, tenant, rid, tok, room.PostInput{
			Text: "我的立场号码 " + string(rune('1'+i)) + "。", OpKey: "p1-" + string(rune('a'+i)),
		})
		seqs[i] = r.Seq
	}

	// 第 2 轮允许提方案
	p1, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "先提的方案", Body: "方案 A 的内容", OpKey: "prop-a",
	})
	if err != nil {
		t.Fatalf("提案失败: %v", err)
	}
	p2, err := eng.Propose(ctx, tenant, rid, domain.HashToken(b.AgentToken), room.ProposeInput{
		Title: "后提的方案", Body: "方案 B 的内容", OpKey: "prop-b",
	})
	if err != nil {
		t.Fatalf("提案失败: %v", err)
	}
	// 第 2 轮发言（必须 replyTo 别人）
	for i, tok := range toks {
		mustPost(t, eng, tenant, rid, tok, room.PostInput{
			Text:    "我回应第 " + string(rune('1'+(i+1)%3)) + " 位的观点。",
			ReplyTo: intp(seqs[(i+1)%3]), OpKey: "p2-" + string(rune('a'+i)),
		})
	}
	// 第 3 轮（收尾轮）：说明支持哪个方案
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "我支持方案一。", SupportProposalID: p1.ProposalID, OpKey: "p3-a"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "我支持方案二。", SupportProposalID: p2.ProposalID, OpKey: "p3-b"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "我也支持方案一。", SupportProposalID: p1.ProposalID, OpKey: "p3-c"})

	res := resultOf(t, eng, tenant, rid)
	if res.State != domain.StateVoting {
		t.Fatalf("最后一轮结束应进入 voting，实际 %s", res.State)
	}
	return &votingFixture{roomID: rid, tokens: toks, props: []string{p1.ProposalID, p2.ProposalID}}
}

// TestTallyLevel1MostVotes 第一级：票多者胜。
func TestTallyLevel1MostVotes(t *testing.T) {
	eng, tenant := newEnv(t)
	f := setupVotingRoom(t, eng, tenant)

	mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[1]), OpKey: "v1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "v2"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[2], room.VoteInput{ProposalID: strp(f.props[1]), OpKey: "v3"})

	res := resultOf(t, eng, tenant, f.roomID)
	if res.Conclusion.Winner == nil || res.Conclusion.Winner.ProposalID != f.props[1] {
		t.Fatalf("2:1 应取票多的方案二，实际 %+v", res.Conclusion.Winner)
	}
}

// TestTallyLevel2ModeratorVoteWins 第二级：平票时主席票优先。
func TestTallyLevel2ModeratorVoteWins(t *testing.T) {
	eng, tenant := newEnv(t)
	f := setupVotingRoom(t, eng, tenant)

	// 主席（甲）投方案一，乙投方案二，丙还没投 → 1:1 平票
	mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[1]), OpKey: "v1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "v2"})

	res := resultOf(t, eng, tenant, f.roomID)
	if res.State != domain.StateVoting {
		t.Fatalf("丙还没投，应仍在 voting，实际 %s", res.State)
	}
	if res.Conclusion.Winner == nil || res.Conclusion.Winner.ProposalID != f.props[1] {
		t.Fatalf("平票应取主席投的方案二，实际 %+v", res.Conclusion.Winner)
	}
}

// TestTallyLevel3EarliestProposal 第三级：主席没投（弃权）仍平 → 最早提交的方案。
func TestTallyLevel3EarliestProposal(t *testing.T) {
	eng, tenant := newEnv(t)
	f := setupVotingRoom(t, eng, tenant)

	// 主席弃权，乙投方案二，丙投方案一 → 1:1 平票，主席票不在平票里
	mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: nil, Reason: "弃权", OpKey: "v1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "v2"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[2], room.VoteInput{ProposalID: strp(f.props[1]), OpKey: "v3"})

	res := resultOf(t, eng, tenant, f.roomID)
	if res.Conclusion.Winner == nil || res.Conclusion.Winner.ProposalID != f.props[0] {
		t.Fatalf("主席弃权且平票时应取最早提交的方案一，实际 %+v", res.Conclusion.Winner)
	}
}

// TestVoteOverwrite 改票覆盖，不重复计票（§八 #14）。
func TestVoteOverwrite(t *testing.T) {
	eng, tenant := newEnv(t)
	f := setupVotingRoom(t, eng, tenant)

	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "v1"})
	r := mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: nil, OpKey: "v1b"})
	if r.Cast != 1 {
		t.Fatalf("改票后票数仍应是 1，实际 %d", r.Cast)
	}
	if r.Votes[f.props[0]] != 0 || r.Votes["abstain"] != 1 {
		t.Fatalf("改票未覆盖: %+v", r.Votes)
	}
}

// TestNoProposalWinnerIsNull 无方案时 winner 为 null（§八 #8）。
func TestNoProposalWinnerIsNull(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	cr := mustCreate(t, eng, tenant, room.CreateInput{Topic: "无方案", MaxAgents: 2, MaxRounds: 2, MaxMinutes: 60})
	a := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "乙")
	rid := cr.Room.ID

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "立场甲。", OpKey: "n1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "立场乙。", OpKey: "n2"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "收尾甲。", OpKey: "n3"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "收尾乙。", OpKey: "n4"})

	mustVote(t, eng, tenant, rid, a.AgentToken, room.VoteInput{FinalStance: domain.StanceAgree, OpKey: "n5"})
	mustVote(t, eng, tenant, rid, b.AgentToken, room.VoteInput{FinalStance: domain.StanceAbstain, OpKey: "n6"})

	res := resultOf(t, eng, tenant, rid)
	if res.Conclusion.Winner != nil {
		t.Fatalf("无方案时 winner 应为 null，实际 %+v", res.Conclusion.Winner)
	}
	if res.Conclusion.Method != "stance" {
		t.Fatalf("应走 stance 降级，实际 %s", res.Conclusion.Method)
	}
	_ = ctx
}
