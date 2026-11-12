package room_test

import (
	"context"
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestInboxYouAreWinner 锁住 inbox.youAreWinner：获胜者在 summarizing 阶段必须读到 true。
//
// 这个字段一度只写在 skills/agchat/SKILL.md 和 mvp/join.md 里，服务端**根本没有实现**。
// 后果不是报错，而是静默降级：照着文档判断的 agent 永远等不到 true，只能靠 notes 里
// 一句话碰运气；碰不到就干等满 60 秒，最后由机械摘要顶掉——「获胜者撰写简要概述」
// 这一步在真实 agent 手里从来没成功过（Lead 的 tools/real-agent-lead.mjs 是硬编码
// state=="summarizing" 才绕过去的）。
//
// 判据同时锁三件事：voting 阶段谁都不是 winner / summarizing 阶段只有获胜者是 /
// inbox 说的和 POST /summary 允许的是同一个人。
func TestInboxYouAreWinner(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	f := setupVotingRoom(t, eng, tenant) // 甲提 p_1，乙提 p_2

	assertWinnerFlag := func(stage string, want map[int]bool) {
		t.Helper()
		for i, tok := range f.tokens {
			snap, err := eng.Inbox(ctx, tenant, f.roomID, domain.HashToken(tok), nil, 0)
			if err != nil {
				t.Fatalf("%s：第 %d 位 agent 拉 inbox 失败: %v", stage, i+1, err)
			}
			if snap.YouAreWinner != want[i] {
				t.Fatalf("%s：第 %d 位 agent 的 youAreWinner=%v，期望 %v（state=%s）",
					stage, i+1, snap.YouAreWinner, want[i], snap.State)
			}
		}
	}

	// ① 投票阶段谁都不是获胜者
	assertWinnerFlag("voting", map[int]bool{0: false, 1: false, 2: false})

	// ② 甲自投 + 乙丙跨票 → p_1 三票全取，甲获胜
	mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "w1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "w2"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[2], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "w3"})

	res := resultOf(t, eng, tenant, f.roomID)
	if res.State != domain.StateSummarizing {
		t.Fatalf("全员投完且有获胜者，应进入 summarizing，实际 %s", res.State)
	}
	if res.Conclusion.Winner == nil || res.Conclusion.Winner.OwnerAlias != "甲" {
		t.Fatalf("获胜者应为甲，实际 %+v", res.Conclusion.Winner)
	}

	// ③ summarizing 阶段只有获胜者是 true
	assertWinnerFlag("summarizing", map[int]bool{0: true, 1: false, 2: false})

	// ④ inbox 说的与 POST /summary 的授权必须一致：非获胜者被拒
	if _, err := eng.Summary(ctx, tenant, f.roomID, domain.HashToken(f.tokens[1]), room.SummaryInput{
		Text: "我不是获胜者，不该写成。", OpKey: "bad-sum",
	}); err == nil {
		t.Fatal("非获胜者提交概述应被拒，实际成功")
	} else if codeOf(err) != "wrong_round" {
		t.Fatalf("非获胜者提交概述应回 403 wrong_round，实际 %v", err)
	}

	// ⑤ 获胜者提交成功，会议结束
	if _, err := eng.Summary(ctx, tenant, f.roomID, domain.HashToken(f.tokens[0]), room.SummaryInput{
		Text: "【简要概述】\n1. 结论：采用 p_1。\n2. 分歧：无。\n3. 下一步：按方案执行。", OpKey: "good-sum",
	}); err != nil {
		t.Fatalf("获胜者提交概述应成功，实际 %v", err)
	}
	if res := resultOf(t, eng, tenant, f.roomID); res.State != domain.StateEnded {
		t.Fatalf("概述提交后应 ended，实际 %s", res.State)
	}
}

// TestInboxYouAreWinnerForSoloAgent 只有 1 人参会时（规格 §八 #2：不辩论、不投票），
// 写概述的就是这个人 —— youAreWinner 必须给它 true。
//
// 这是 summaryAuthor 里"无获胜方案时退到主席"的分支被真正走到的场景：
// 一个人开一场会，它既没有对手也没有投票，照样要交一段概述。
func TestInboxYouAreWinnerForSoloAgent(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "一个人开会", MaxAgents: 1, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	solo := mustJoin(t, eng, tenant, rid, cr.Password, "独苗丙")

	// 名额满了（1/1）→ 开场即 summarizing
	snap, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(solo.AgentToken), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != domain.StateSummarizing {
		t.Fatalf("单人房间应直接进 summarizing，实际 %s", snap.State)
	}
	if !snap.YouAreWinner {
		t.Fatal("单人房间里唯一的人必须拿到 youAreWinner=true，否则没人能写概述")
	}

	// 它得真的写得进去
	if _, err := eng.Summary(ctx, tenant, rid, domain.HashToken(solo.AgentToken), room.SummaryInput{
		Text: "【简要概述】\n1. 结论：我一个人开完了。\n2. 分歧：无。\n3. 下一步：无。", OpKey: "solo-sum",
	}); err != nil {
		t.Fatalf("单人会议室唯一的人应能提交概述，实际 %v", err)
	}
}
