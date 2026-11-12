package room_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestPromptUsesMaxAgents 锁住：prompt 里的人数必须是 maxAgents，而不是"此刻进房人数"。
//
// 历史 bug：prompt 在 join 时生成，第一个人看到"共 1 位参会者"，
// 会让 agent 按"一个人的会议"组织语言。真实名单在开场后由 inbox.roster 下发。
func TestPromptUsesMaxAgents(t *testing.T) {
	eng, tenant := newEnvWith(t, time.Hour, time.Hour)
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "prompt 人数", MaxAgents: 6, MaxRounds: 10, MaxMinutes: 600,
	})
	// 只进 3 个人（没满员），briefing 还没结束
	var joins []*room.JoinResult
	for _, name := range []string{"甲", "乙", "丙"} {
		joins = append(joins, mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, name))
	}

	for i, j := range joins {
		if !strings.Contains(j.Brief.Prompt, "最多 6 位参会者") {
			t.Fatalf("第 %d 份 prompt 没用 maxAgents 表述：\n%s", i+1, j.Brief.Prompt)
		}
		if strings.Contains(j.Brief.Prompt, "共 1 位参会者") || strings.Contains(j.Brief.Prompt, "共 2 位参会者") {
			t.Fatalf("第 %d 份 prompt 用了当时的实际人数：\n%s", i+1, j.Brief.Prompt)
		}
	}
	// 每份 prompt 的人数口径必须完全一致：人数那一行必须是"最多 maxAgents 位"的表述
	for i, j := range joins {
		line := ""
		for _, l := range strings.Split(j.Brief.Prompt, "\n") {
			if strings.Contains(l, "位参会者") {
				line = l
				break
			}
		}
		if !strings.Contains(line, "最多 6 位参会者") {
			t.Fatalf("第 %d 份 prompt 的人数口径不对（应为「最多 6 位参会者」）:\n  %s", i+1, line)
		}
	}

	// 开场后：每个人看到的 roster 长度 = 实际人数，且彼此一致
	now += 31 * 1000 // 越过 briefing 截止（30s）
	for _, j := range joins {
		snap, err := eng.Inbox(context.Background(), tenant, cr.Room.ID, domain.HashToken(j.AgentToken), nil, 0)
		if err != nil {
			t.Fatalf("inbox 失败: %v", err)
		}
		if len(snap.Roster) != len(joins) {
			t.Fatalf("%s 看到的 roster 长度 = %d，应为 %d", j.AgentID, len(snap.Roster), len(joins))
		}
		mods := 0
		for _, r := range snap.Roster {
			if r.IsModerator {
				mods++
			}
		}
		if mods != 1 {
			t.Fatalf("roster 里主席应恰好 1 个，实际 %d", mods)
		}
		joined := strings.Join(snap.Notes, " | ")
		if !strings.Contains(joined, "实际参会 3 人") {
			t.Fatalf("notes 里应说明实际参会人数: %v", snap.Notes)
		}
	}
}

// TestDisagreementNeverMentionsWinner 锁住：分歧段绝不能出现获胜方案的标题。
func TestDisagreementNeverMentionsWinner(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "分歧判定", MaxAgents: 3, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	c := mustJoin(t, eng, tenant, rid, cr.Password, "丙")

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "立场甲。", OpKey: "g1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "立场乙。", OpKey: "g2"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "立场丙。", OpKey: "g3"})

	p1, _ := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "获胜的方案标题", Body: "方案一正文", OpKey: "g4",
	})
	p2, _ := eng.Propose(ctx, tenant, rid, domain.HashToken(b.AgentToken), room.ProposeInput{
		Title: "落选的方案标题", Body: "方案二正文", OpKey: "g5",
	})

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应乙。", ReplyTo: intp(2), OpKey: "g6"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(1), OpKey: "g7"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(1), OpKey: "g8"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "支持方案一。", SupportProposalID: p1.ProposalID, OpKey: "g9"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "支持方案二。", SupportProposalID: p2.ProposalID, OpKey: "g10"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "支持方案一。", SupportProposalID: p1.ProposalID, OpKey: "g11"})

	// 造出 {p_1:2, p_2:1, abstain:0}
	mustVote(t, eng, tenant, rid, a.AgentToken, room.VoteInput{ProposalID: strp(p2.ProposalID), OpKey: "g12"})
	mustVote(t, eng, tenant, rid, b.AgentToken, room.VoteInput{ProposalID: strp(p1.ProposalID), OpKey: "g13"})
	mustVote(t, eng, tenant, rid, c.AgentToken, room.VoteInput{ProposalID: strp(p1.ProposalID), OpKey: "g14"})

	res := resultOf(t, eng, tenant, rid)
	if res.Conclusion.Winner == nil || res.Conclusion.Winner.ProposalID != p1.ProposalID {
		t.Fatalf("p_1 应获胜: %+v", res.Conclusion.Winner)
	}
	if res.Conclusion.Votes[p1.ProposalID].Total != 2 || res.Conclusion.Votes[p2.ProposalID].Total != 1 {
		t.Fatalf("票型应为 {p_1:2,p_2:1}: %+v", res.Conclusion.Votes)
	}

	disagree := room.DisagreementLine(res)
	if strings.Contains(disagree, "获胜的方案标题") {
		t.Fatalf("分歧段出现了获胜方案标题：%s", disagree)
	}
	if !strings.Contains(disagree, "落选的方案标题") || !strings.Contains(disagree, "1 票") {
		t.Fatalf("分歧段应列出落选方案与票数：%s", disagree)
	}

	// 三处共用同一段文案
	for _, s := range res.Summary.Sections {
		if s.H == "分歧" && s.P != disagree {
			t.Fatalf("summary 的「分歧」段与 ownerReport 不一致:\n%s\nvs\n%s", s.P, disagree)
		}
	}
	report := room.OwnerReportOf(res, "http://x/room/1")
	// 获胜标题当然会出现在"最终选了"和"一句话结论"里 —— 硬约束只是**分歧段**不能有它
	if strings.Contains(strings.Split(report, "主要分歧：")[1], "获胜的方案标题") {
		t.Fatalf("ownerReport 的分歧段出现了获胜方案标题:\n%s", report)
	}
}
