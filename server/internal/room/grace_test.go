package room_test

import (
	"context"
	"testing"
	"time"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// graceFixture 造一个 3 人会议：甲、乙活着，丙掉线；进入 voting 后丙被预置弃权。
func graceFixture(t *testing.T, grace time.Duration) (*room.Engine, string, string, string, string, *int64) {
	t.Helper()
	turnTO := time.Second
	eng, tenant := newEnvWith(t, turnTO, time.Hour)
	eng.SetVoteGrace(grace)
	ctx := context.Background()
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "voting 宽限期", MaxAgents: 3, MaxRounds: 2, MaxMinutes: 600, TokenBudget: 5000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	c := mustJoin(t, eng, tenant, rid, cr.Password, "丙")

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲立场。", OpKey: "g1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙立场。", OpKey: "g2"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "丙立场。", OpKey: "g3"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲收尾。", OpKey: "g4"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙收尾。", OpKey: "g5"})
	now += turnTO.Milliseconds()
	if _, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(a.AgentToken), nil, 0); err != nil {
		t.Fatal(err)
	}

	// 甲乙保活，丙失联到 3×turnTO 之外
	for i := 0; i < 10; i++ {
		now += (500 * time.Millisecond).Milliseconds()
		for _, tok := range []string{a.AgentToken, b.AgentToken} {
			if _, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(tok), nil, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 甲乙投票 → 加上丙的预置弃权，此刻"全员已投"
	if _, err := eng.Vote(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.VoteInput{FinalStance: domain.StanceAgree, OpKey: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Vote(ctx, tenant, rid, domain.HashToken(b.AgentToken), room.VoteInput{FinalStance: domain.StanceAgree, OpKey: "v2"}); err != nil {
		t.Fatal(err)
	}
	return eng, tenant, rid, a.AgentToken, c.AgentToken, &now
}

// TestVoteGraceLetsLateMemberCorrect 宽限期内回来补投 → 覆盖预置弃权。
func TestVoteGraceLetsLateMemberCorrect(t *testing.T) {
	ctx := context.Background()
	eng, tenant, rid, _, cToken, now := graceFixture(t, 10*time.Second)

	res := resultOf(t, eng, tenant, rid)
	if res.State != domain.StateVoting {
		t.Fatalf("全员投完后应进入宽限期（仍在 voting），实际 %s", res.State)
	}

	// 宽限期内丙回来投 agree
	if _, err := eng.Vote(ctx, tenant, rid, domain.HashToken(cToken), room.VoteInput{
		FinalStance: domain.StanceDisagree, OpKey: "late",
	}); err != nil {
		t.Fatalf("宽限期内补投应成功，实际 %v", err)
	}
	var stance string
	if err := eng.View(ctx, tenant, rid, func(agg *room.Agg) error {
		if v := agg.VoteByAgent("m_3"); v != nil {
			stance = v.FinalStance
			if v.Reason == room.AutoAbstainReason {
				t.Fatal("预置弃权没有被覆盖")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if stance != domain.StanceDisagree {
		t.Fatalf("丙的票应为 disagree，实际 %q", stance)
	}

	// 宽限期满 → 结算
	*now += 11_000
	res = resultOf(t, eng, tenant, rid)
	if res.State == domain.StateVoting {
		t.Fatalf("宽限期满应结算，实际仍在 voting")
	}
	if res.Conclusion.Votes[domain.StanceAbstain].Total != 0 {
		t.Fatalf("丙已补投，不应再有弃权票：%+v", res.Conclusion.Votes)
	}
	t.Logf("宽限期内补投成功 → 票型 %+v", res.Conclusion.Votes)
}

// TestVoteGraceExpiresThenSettles 没人回来 → 宽限期满后结算，票型保留 abstain。
func TestVoteGraceExpiresThenSettles(t *testing.T) {
	eng, tenant, rid, _, _, now := graceFixture(t, 10*time.Second)

	res := resultOf(t, eng, tenant, rid)
	if res.State != domain.StateVoting {
		t.Fatalf("宽限期内不应结算，实际 %s", res.State)
	}
	*now += 11_000
	res = resultOf(t, eng, tenant, rid)
	if res.State == domain.StateVoting {
		t.Fatal("宽限期满应结算")
	}
	if res.Conclusion.Votes[domain.StanceAbstain].Total != 1 {
		t.Fatalf("掉线的丙应保留 1 票弃权：%+v", res.Conclusion.Votes)
	}
	t.Logf("无人补投 → 宽限期满结算，票型 %+v", res.Conclusion.Votes)
}

// TestVoteGraceZeroSettlesImmediately -vote-grace 0 = 关闭宽限，全员投完立刻结算。
func TestVoteGraceZeroSettlesImmediately(t *testing.T) {
	eng, tenant, rid, _, _, _ := graceFixture(t, 0)

	res := resultOf(t, eng, tenant, rid)
	if res.State == domain.StateVoting {
		t.Fatal("grace=0 时全员投完应立即结算")
	}
	t.Logf("grace=0：立即结算，票型 %+v", res.Conclusion.Votes)
}
