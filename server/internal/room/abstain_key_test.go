package room_test

import (
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestVoteTotalsAlwaysHasAbstain 锁住：voteTotals / votes 里 abstain 是**恒定键**。
//
// 一个"有时存在有时不存在"的字段就是陷阱：消费者读到 nil 和读到 0 分不清。
// 这个坑今天绊了两个人（eng-scout 的用例 + 一位干净 agent）。
func TestVoteTotalsAlwaysHasAbstain(t *testing.T) {
	assertHasAbstain := func(t *testing.T, res *room.Result, want int, label string) {
		t.Helper()
		v, ok := res.Conclusion.VoteTotals["abstain"]
		if !ok {
			t.Fatalf("%s：voteTotals 里没有 abstain 键（实际 %+v）", label, res.Conclusion.VoteTotals)
		}
		if v != want {
			t.Fatalf("%s：abstain 应为 %d，实际 %d", label, want, v)
		}
		if _, ok := res.Conclusion.Votes["abstain"]; !ok {
			t.Fatalf("%s：votes 里也没有 abstain 键", label)
		}
	}

	// 场景 1：有方案、但没人弃权 → abstain 必须存在且为 0
	t.Run("有方案无人弃权", func(t *testing.T) {
		eng, tenant := newEnv(t)
		f := setupVotingRoom(t, eng, tenant)
		mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "a1"})
		mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "a2"})
		res := resultOf(t, eng, tenant, f.roomID)
		assertHasAbstain(t, res, 0, "有方案无人弃权")
		t.Logf("有方案无人弃权：voteTotals=%+v", res.Conclusion.VoteTotals)
	})

	// 场景 2：有方案、有人弃权 → abstain 等于弃权人数
	t.Run("有方案有人弃权", func(t *testing.T) {
		eng, tenant := newEnv(t)
		f := setupVotingRoom(t, eng, tenant)
		mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "b1"})
		mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: nil, OpKey: "b2"})
		res := resultOf(t, eng, tenant, f.roomID)
		assertHasAbstain(t, res, 1, "有方案有人弃权")
		t.Logf("有方案有人弃权：voteTotals=%+v", res.Conclusion.VoteTotals)
	})

	// 场景 3：无方案降级模式 → abstain 照样存在
	t.Run("降级模式", func(t *testing.T) {
		eng, tenant := newEnv(t)
		cr := mustCreate(t, eng, tenant, room.CreateInput{Topic: "降级", MaxAgents: 2, MaxRounds: 2, MaxMinutes: 60})
		rid := cr.Room.ID
		a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
		b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
		mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "立场甲。", OpKey: "d1"})
		mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "立场乙。", OpKey: "d2"})
		mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "收尾甲。", OpKey: "d3"})
		mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "收尾乙。", OpKey: "d4"})
		mustVote(t, eng, tenant, rid, a.AgentToken, room.VoteInput{FinalStance: domain.StanceAgree, OpKey: "d5"})
		mustVote(t, eng, tenant, rid, b.AgentToken, room.VoteInput{FinalStance: domain.StanceAgree, OpKey: "d6"})
		res := resultOf(t, eng, tenant, rid)
		assertHasAbstain(t, res, 0, "降级模式")
		t.Logf("降级模式：voteTotals=%+v", res.Conclusion.VoteTotals)
	})
}
