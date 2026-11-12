package room_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestSelfVoteAllowedAndBreakdown 允许自投，但票数要区分「自投 / 跨票」。
//
// 曾经禁止自投：防住了"人人投自己导致必然平票"，却把"唯一认真提了方案的人"
// 逼去投更差的方案 —— 干净 agent 的原话是"这个 2/3 多数是规则产物"。
// 现在的取舍：允许自投，但获胜按 cross 排、自投不能单独构成多数。
func TestSelfVoteAllowedAndBreakdown(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	f := setupVotingRoom(t, eng, tenant) // 甲提 p_1，乙提 p_2

	// 甲投自己（现在是允许的）
	if _, err := eng.Vote(ctx, tenant, f.roomID, domain.HashToken(f.tokens[0]), room.VoteInput{
		ProposalID: strp(f.props[0]), OpKey: "self1",
	}); err != nil {
		t.Fatalf("应允许投自己，实际 %v", err)
	}
	// 乙投甲（跨票）、丙投甲（跨票）
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "c1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[2], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "c2"})

	res := resultOf(t, eng, tenant, f.roomID)
	v := res.Conclusion.Votes[f.props[0]]
	if v.Total != 3 || v.Self != 1 || v.Cross != 2 {
		t.Fatalf("票数拆分应为 total=3 self=1 cross=2，实际 %+v", v)
	}
	if res.Conclusion.Winner == nil || res.Conclusion.Winner.ProposalID != f.props[0] {
		t.Fatalf("甲应获胜: %+v", res.Conclusion.Winner)
	}
	t.Logf("自投拆分：p_1 %+v（winner=%s, agreement=%s）", v, res.Conclusion.Winner.ProposalID, res.Conclusion.Agreement)
}

// TestPureSelfVotesIsNotMajority 纯自投堆出来的多数不算多数。
func TestPureSelfVotesIsNotMajority(t *testing.T) {
	eng, tenant := newEnv(t)
	f := setupVotingRoom(t, eng, tenant)

	// 甲投自己、乙投自己 → p_1:1(self) / p_2:1(self) 平票，丙弃权
	mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "s1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[1]), OpKey: "s2"})

	res := resultOf(t, eng, tenant, f.roomID)
	if res.Conclusion.Agreement == "majority" || res.Conclusion.Agreement == "unanimous" {
		t.Fatalf("纯自投票不能判为多数，实际 %q", res.Conclusion.Agreement)
	}
	for _, p := range res.Proposals {
		v := res.Conclusion.Votes[p.ProposalID]
		if v.Cross != 0 {
			t.Fatalf("这一轮不该有跨票：%+v", v)
		}
	}
	t.Logf("纯自投：agreement=%s（不是多数）", res.Conclusion.Agreement)
}

// TestTieIsNotConsensus 平票不许假装有赢家：agreement=tie，且 ownerReport 不能说「最终选了」。
func TestTieIsNotConsensus(t *testing.T) {
	eng, tenant := newEnv(t)
	f := setupVotingRoom(t, eng, tenant)

	// 甲(主席)→p_2，乙→p_1，丙弃权 ⇒ p_1:1, p_2:1 平票
	mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[1]), OpKey: "t1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "t2"})
	// 丙必须真的弃权：只投两票时房间还停在 voting，ownerReport 是"进行中"而不是结论
	mustVote(t, eng, tenant, f.roomID, f.tokens[2], room.VoteInput{ProposalID: nil, OpKey: "t3"})

	res := resultOf(t, eng, tenant, f.roomID)
	if res.Conclusion.Agreement != "tie" {
		t.Fatalf("1:1 平票应判定 agreement=tie，实际 %q（winner=%+v）", res.Conclusion.Agreement, res.Conclusion.Winner)
	}
	if res.Conclusion.Winner == nil {
		t.Fatal("winner 仍应保留（页面要能展示）")
	}
	if !strings.Contains(res.Conclusion.Text, "未形成多数") {
		t.Fatalf("结论文案应说明未形成多数: %s", res.Conclusion.Text)
	}

	report := room.OwnerReportOf(res, "http://x/room/1")
	if strings.Contains(report, "最终选了") {
		t.Fatalf("平票时 ownerReport 不能写「最终选了」:\n%s", report)
	}
	if !strings.Contains(report, "未形成多数") {
		t.Fatalf("平票时 ownerReport 应写「未形成多数」:\n%s", report)
	}
	t.Logf("平票 ownerReport:\n%s", report)
}

// TestPresetAbstainIsRevocable 预置弃权可以被本人撤销（误判不至于变成永久错误）。
func TestPresetAbstainIsRevocable(t *testing.T) {
	turnTO := time.Second
	eng, tenant := newEnvWith(t, turnTO, time.Hour)
	ctx := context.Background()
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })

	// 4 人：甲、乙、丁保持心跳，丙掉线；丁始终不投票 → 房间停在 voting，丙来得及回来
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "撤销预置弃权", MaxAgents: 4, MaxRounds: 2, MaxMinutes: 600, TokenBudget: 5000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	c := mustJoin(t, eng, tenant, rid, cr.Password, "丙")
	d := mustJoin(t, eng, tenant, rid, cr.Password, "丁")

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲立场。", OpKey: "r1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙立场。", OpKey: "r2"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "丙立场。", OpKey: "r3"})
	mustPost(t, eng, tenant, rid, d.AgentToken, room.PostInput{Text: "丁立场。", OpKey: "r4"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲收尾。", OpKey: "r5"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙收尾。", OpKey: "r6"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "丙收尾。", OpKey: "r7"})
	mustPost(t, eng, tenant, rid, d.AgentToken, room.PostInput{Text: "丁收尾。", OpKey: "r8"})
	now += turnTO.Milliseconds()
	if _, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(a.AgentToken), nil, 0); err != nil {
		t.Fatal(err)
	}

	// 推进时间：只有丙掉线（甲乙丁持续心跳）
	for i := 0; i < 8; i++ {
		now += (500 * time.Millisecond).Milliseconds()
		for _, tok := range []string{a.AgentToken, b.AgentToken, d.AgentToken} {
			if _, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(tok), nil, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	res := resultOf(t, eng, tenant, rid)
	if res.State != domain.StateVoting {
		t.Fatalf("应仍在投票（丁没投），实际 %s", res.State)
	}

	// 丙"回来"了：撤销预置弃权，改投 agree
	if _, err := eng.Vote(ctx, tenant, rid, domain.HashToken(c.AgentToken), room.VoteInput{
		FinalStance: domain.StanceAgree, OpKey: "late",
	}); err != nil {
		t.Fatalf("掉线者回来投票应允许撤销预置弃权，实际 %v", err)
	}
	var myStance string
	if err := eng.View(ctx, tenant, rid, func(agg *room.Agg) error {
		if v := agg.VoteByAgent(c.AgentID); v != nil {
			myStance = v.FinalStance
			if v.Reason == room.AutoAbstainReason {
				t.Fatalf("预置弃权没有被撤销")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if myStance != domain.StanceAgree {
		t.Fatalf("丙的票应被改成 agree，实际 %q", myStance)
	}
	t.Logf("预置弃权可撤销：丙回来投票后计为 %s（不再算弃权）", myStance)
}
func TestGoneAgentPresetAbstain(t *testing.T) {
	turnTO := time.Second
	eng, tenant := newEnvWith(t, turnTO, time.Hour) // summary 超时长一点，避免干扰
	ctx := context.Background()
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "掉线预置弃权", MaxAgents: 3, MaxRounds: 2, MaxMinutes: 600, TokenBudget: 5000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	c := mustJoin(t, eng, tenant, rid, cr.Password, "丙")

	// 第 1 轮三人各发一条
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲的立场。", OpKey: "q1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙的立场。", OpKey: "q2"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "丙的立场。", OpKey: "q3"})
	// 第 2 轮（收尾轮）：只有甲、乙说话，丙从此失联
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲收尾。", OpKey: "q4"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙收尾。", OpKey: "q5"})
	now += turnTO.Milliseconds()
	if _, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(a.AgentToken), nil, 0); err != nil {
		t.Fatal(err)
	}

	res := resultOf(t, eng, tenant, rid)
	if res.State != domain.StateVoting {
		t.Fatalf("应进入投票，实际 %s", res.State)
	}

	// 丙失联期间，甲乙一直保持心跳（真实 agent 会持续长轮询）；
	// 时钟推过 2×turnTO 后丙被判掉线并预置弃权，甲乙仍然新鲜。
	startAfterGone := now
	for i := 0; i < 8; i++ {
		now += (500 * time.Millisecond).Milliseconds()
		for _, tok := range []string{a.AgentToken, b.AgentToken} {
			if _, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(tok), nil, 0); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 甲乙投票（无方案 → 降级 finalStance）；若已结算则跳过
	for i, tok := range []string{a.AgentToken, b.AgentToken} {
		_, err := eng.Vote(ctx, tenant, rid, domain.HashToken(tok), room.VoteInput{
			FinalStance: domain.StanceAgree, OpKey: "v" + string(rune('1'+i)),
		})
		if err != nil && codeOf(err) != "wrong_round" {
			t.Fatalf("投票失败: %v", err)
		}
	}

	res = resultOf(t, eng, tenant, rid)
	if res.State == domain.StateVoting {
		t.Fatalf("丙已掉线且甲乙投完，应立即结算，实际仍在 voting（白等了 %.0fs）",
			float64(now-startAfterGone)/1000)
	}
	// 掉线者应被记为弃权（活着的甲乙投的是 agree）
	if res.Conclusion.Votes[domain.StanceAbstain].Total < 1 {
		t.Fatalf("掉线的丙应被计为弃权，实际票型 %+v", res.Conclusion.Votes)
	}
	elapsedMs := now - startAfterGone
	if elapsedMs > 5000 {
		t.Fatalf("结算耗时 %dms，应远小于 60s 投票窗口", elapsedMs)
	}
	t.Logf("掉线预置弃权：甲乙投完后 %dms 结算（时钟只推进了 %.1f 秒，远小于 60s 窗口）",
		elapsedMs, float64(elapsedMs)/1000)
}
