package room_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestVoteWindowFillsAbstainForSilentMember 投票窗口到期时"没投的人"必须被计成弃权票。
//
// 实测（Lead 亲自当 agent）：3 人参会、2 人投票，窗口一到期房间就 locked，
// 结果是 votes = {p_2:1, p_3:1, abstain:0} —— 各项之和 2，参会人数 3。
// docs/07 §十五 的不变量「voteTotals 各项之和 == 实际投票人数」当场破了：
// 分母错了，任何"多数/分裂"判断都不可信；页面上那句"未投票（视为弃权）"只是文案，
// 机器读到的字段仍然少一个人。
func TestVoteWindowFillsAbstainForSilentMember(t *testing.T) {
	eng, tenant := newEnvWith(t, time.Hour, time.Hour)
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })
	ctx := context.Background()

	f := setupVotingRoom(t, eng, tenant) // 3 人、p_1 / p_2
	// 甲投 p_1、乙投 p_2，丙整场不投
	mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "q1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[1]), OpKey: "q2"})
	if res := resultOf(t, eng, tenant, f.roomID); res.State != domain.StateVoting {
		t.Fatalf("丙没投，不该结算，实际 %s", res.State)
	}

	// 推进时钟越过投票窗口（VotingWindow = 60s）
	now += 61 * 1000
	if _, err := eng.Inbox(ctx, tenant, f.roomID, domain.HashToken(f.tokens[0]), nil, 0); err != nil {
		t.Fatal(err)
	}

	res := resultOf(t, eng, tenant, f.roomID)
	if res.State != domain.StateVoting && res.State != domain.StateSummarizing && res.State != domain.StateEnded {
		t.Fatalf("窗口到期后应推进，实际 %s", res.State)
	}
	sum := 0
	for _, v := range res.Conclusion.VoteTotals {
		sum += v
	}
	members := len(res.Positions)
	if sum != members {
		t.Fatalf("不变量被破坏：voteTotals 之和 %d != 参会人数 %d（%+v）", sum, members, res.Conclusion.VoteTotals)
	}
	if res.Conclusion.VoteTotals["abstain"] != 1 {
		t.Fatalf("未投票的丙应计为 1 张弃权，实际 %+v", res.Conclusion.VoteTotals)
	}
	for _, p := range res.Positions {
		if p.Alias == "丙" && p.VotedFor != nil {
			t.Fatalf("丙没投票，votedFor 应为 null，实际 %q", *p.VotedFor)
		}
	}
	t.Logf("窗口到期后的票型：%+v（合计 %d / 参会 %d）", res.Conclusion.VoteTotals, sum, members)
}

// TestTieBreakTextIsAccurate 平票文案必须说清"用的是哪条规则"。
//
// 实测：主席根本没投票，属于"最早提交"破平，ownerReport 却写「按主席优先规则选定」。
func TestTieBreakTextIsAccurate(t *testing.T) {
	// 情形一：主席（甲）投了 p_1 → 主席票优先
	eng, tenant := newEnv(t)
	f := setupVotingRoom(t, eng, tenant)
	mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "t1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: strp(f.props[1]), OpKey: "t2"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[2], room.VoteInput{ProposalID: nil, OpKey: "t3"})

	endIfSummarizing(t, eng, tenant, f.roomID, f.tokens)
	res := resultOf(t, eng, tenant, f.roomID)
	if res.Conclusion.Agreement != "tie" || res.Conclusion.TieBreak != "moderator" {
		t.Fatalf("1:1 且主席投了获胜方 → agreement=tie / tieBreak=moderator，实际 %q / %q",
			res.Conclusion.Agreement, res.Conclusion.TieBreak)
	}
	report := room.OwnerReportOf(res, "http://x/room/1")
	if !strings.Contains(report, "主席票优先") {
		t.Fatalf("ownerReport 应写「主席票优先」：\n%s", report)
	}

	// 情形二：主席弃权 → 最早提交破平，文案不许再写"主席优先"
	eng2, tenant2 := newEnv(t)
	f2 := setupVotingRoom(t, eng2, tenant2)
	mustVote(t, eng2, tenant2, f2.roomID, f2.tokens[0], room.VoteInput{ProposalID: nil, OpKey: "u1"})
	mustVote(t, eng2, tenant2, f2.roomID, f2.tokens[1], room.VoteInput{ProposalID: strp(f2.props[0]), OpKey: "u2"})
	mustVote(t, eng2, tenant2, f2.roomID, f2.tokens[2], room.VoteInput{ProposalID: strp(f2.props[1]), OpKey: "u3"})

	endIfSummarizing(t, eng2, tenant2, f2.roomID, f2.tokens)
	res2 := resultOf(t, eng2, tenant2, f2.roomID)
	if res2.Conclusion.Agreement != "tie" || res2.Conclusion.TieBreak != "earliest" {
		t.Fatalf("主席弃权时平票 → tieBreak 应为 earliest，实际 %q", res2.Conclusion.TieBreak)
	}
	report2 := room.OwnerReportOf(res2, "http://x/room/1")
	if strings.Contains(report2, "主席优先") {
		t.Fatalf("主席没投票，绝不能写「按主席优先规则选定」：\n%s", report2)
	}
	if !strings.Contains(report2, "最早提交") {
		t.Fatalf("应写「按最早提交规则选定」：\n%s", report2)
	}
	t.Logf("主席弃权时的 ownerReport 首行：%s", strings.SplitN(report2, "\n", 2)[1])
}
