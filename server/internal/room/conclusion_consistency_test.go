package room_test

import (
	"context"
	"strings"
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// endIfSummarizing 让会议走到 ended：由"唯一有权写概述的那个人"提交一份概述。
//
// 为什么要这一步：summarizing 阶段 conclusion.text 是进度提示
// （"投票已结束，正在等待获胜者撰写概述。"），真正的结论文案只在 ended 之后生成。
// 顺带验证"能被授权写概述的人恰好一个"。
func endIfSummarizing(t *testing.T, eng *room.Engine, tenant, rid string, tokens []string) {
	t.Helper()
	res := resultOf(t, eng, tenant, rid)
	if res.State == domain.StateEnded {
		return
	}
	if res.State != domain.StateSummarizing {
		t.Fatalf("投票结束后应处于 summarizing，实际 %s", res.State)
	}
	ok := 0
	for i, tok := range tokens {
		_, err := eng.Summary(context.Background(), tenant, rid, domain.HashToken(tok), room.SummaryInput{
			Text:  "【简要概述】\n1. 结论：按票数记录。\n2. 主要分歧：见票数构成。\n3. 下一步：人工复核。",
			OpKey: "sum-" + string(rune('a'+i)),
		})
		if err == nil {
			ok++
			break
		}
	}
	if ok != 1 {
		t.Fatalf("应当恰好有一个 agent 能提交概述，实际 %d 个", ok)
	}
	if res := resultOf(t, eng, tenant, rid); res.State != domain.StateEnded {
		t.Fatalf("概述提交后应 ended，实际 %s", res.State)
	}
}

// assertConsistent 校验同一份 result 里的三处说法互不矛盾：
//
//	conclusion.agreement（机器判定） —— conclusion.text（一句话结论） —— ownerReport（贴给主人那段）
//
// 为什么要这条：acceptance-meeting-full 跑出过这样的结果 ——
//
//	conclusion.agreement = "split"（自投占了半数，按 v2.2 不算多数）
//	conclusion.text      = "选出方案「月付为主，年付保留折扣」（2/2 票）"   ← 假共识
//	ownerReport          = "未形成多数。…记录值仅供参考。"                  ← 又说实话
//
// 同一份 JSON 里三个字段互相打脸，而 agent 可能只贴其中一句给主人。
// 根因是 text 自己用 winVotes*2<=人数 重算了一遍"够不够多数"，没跟 agreement 同源。
func assertConsistent(t *testing.T, res *room.Result, report string) {
	t.Helper()
	switch res.Conclusion.Agreement {
	case "unanimous", "majority":
		if !strings.Contains(res.Conclusion.Text, "选出方案") {
			t.Fatalf("agreement=%s 时结论应写「选出方案」，实际：%s", res.Conclusion.Agreement, res.Conclusion.Text)
		}
		if strings.Contains(res.Conclusion.Text, "未形成多数") {
			t.Fatalf("agreement=%s 时结论不该写「未形成多数」：%s", res.Conclusion.Agreement, res.Conclusion.Text)
		}
		if !strings.Contains(report, "最终选了") {
			t.Fatalf("agreement=%s 时 ownerReport 应写「最终选了」：\n%s", res.Conclusion.Agreement, report)
		}
	case "split", "tie":
		if strings.Contains(res.Conclusion.Text, "选出方案") {
			t.Fatalf("agreement=%s 时结论绝不能写「选出方案」（假共识）：%s", res.Conclusion.Agreement, res.Conclusion.Text)
		}
		if !strings.Contains(res.Conclusion.Text, "未形成多数") {
			t.Fatalf("agreement=%s 时结论应写「未形成多数」：%s", res.Conclusion.Agreement, res.Conclusion.Text)
		}
		if strings.Contains(report, "最终选了") {
			t.Fatalf("agreement=%s 时 ownerReport 绝不能写「最终选了」：\n%s", res.Conclusion.Agreement, report)
		}
	default:
		t.Fatalf("未覆盖的 agreement 取值：%q", res.Conclusion.Agreement)
	}
}

// TestConclusionTextMatchesAgreement 四种多数形态下，三处说法必须一致。
func TestConclusionTextMatchesAgreement(t *testing.T) {
	// setupVotingRoom：甲提 p_1、乙提 p_2、三人参战（甲=主席）
	cases := []struct {
		name  string
		votes []struct {
			who  int
			prop int // -1 = 弃权
		}
		wantAgreement string
	}{
		{
			name: "全体一致（含 1 张自投）",
			votes: []struct {
				who  int
				prop int
			}{{0, 1}, {1, 1}, {2, 1}},
			wantAgreement: "unanimous",
		},
		{
			name: "普通多数 2/3",
			votes: []struct {
				who  int
				prop int
			}{{0, 1}, {1, 1}, {2, 0}},
			wantAgreement: "majority",
		},
		{
			name: "参与度不足 1/3",
			votes: []struct {
				who  int
				prop int
			}{{0, 0}, {1, -1}, {2, -1}},
			wantAgreement: "split",
		},
		{
			name: "1:1 平票",
			votes: []struct {
				who  int
				prop int
			}{{0, 1}, {1, 0}, {2, -1}},
			wantAgreement: "tie",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eng, tenant := newEnv(t)
			f := setupVotingRoom(t, eng, tenant)
			for i, v := range c.votes {
				var pid *string
				if v.prop >= 0 {
					pid = strp(f.props[v.prop])
				}
				mustVote(t, eng, tenant, f.roomID, f.tokens[v.who], room.VoteInput{
					ProposalID: pid, OpKey: "vt" + string(rune('a'+i)),
				})
			}
			endIfSummarizing(t, eng, tenant, f.roomID, f.tokens)
			res := resultOf(t, eng, tenant, f.roomID)
			if res.Conclusion.Agreement != c.wantAgreement {
				t.Fatalf("agreement 期望 %q，实际 %q（votes=%+v）",
					c.wantAgreement, res.Conclusion.Agreement, res.Conclusion.VoteTotals)
			}
			report := room.OwnerReportOf(res, "http://x/room/1")
			t.Logf("agreement=%s text=%s", res.Conclusion.Agreement, res.Conclusion.Text)
			assertConsistent(t, res, report)
		})
	}
}

// TestTwoPersonAllVotedIsUnanimous 2 人房间「作者自投 + 对方也投它」= 2/2 票全员一致。
//
// 这是两位干净 agent 独立报上来的同一条：v2.2 的 `self×2 >= 参会人数 → split` 在 2 人房里
// 恒成立（自投 1 张就满足 1×2 ≥ 2），于是"两个人都投了同一个方案"被判成「未形成多数」，
// ownerReport 还写着"其余参会者的票不足以构成多数"——把真共识说成分裂。
// v2.3 去掉这条额外审查：每个 agent 只有 1 票、每个方案只有 1 个作者，自投最多 1 张，
// 过半必然含跨票，这条约束由票权结构天然成立。agreement 只按总票数算。
func TestTwoPersonAllVotedIsUnanimous(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "两人房的自投", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")

	r1 := mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲的立场。", OpKey: "z1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙的立场。", OpKey: "z2"})
	p1, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "甲的可执行方案", Body: "由甲在下周三前完成，成功标准是留存。", OpKey: "z3",
	})
	if err != nil {
		t.Fatalf("提案失败: %v", err)
	}
	if _, err := eng.Propose(ctx, tenant, rid, domain.HashToken(b.AgentToken), room.ProposeInput{
		Title: "乙的一句话", Body: "改。", OpKey: "z4",
	}); err != nil {
		t.Fatalf("提案失败: %v", err)
	}
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应乙。", ReplyTo: intp(r1.Seq), OpKey: "z5"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(r1.Seq), OpKey: "z6"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "我支持我的方案。", SupportProposalID: p1.ProposalID, OpKey: "z7"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "我也支持甲那个。", SupportProposalID: p1.ProposalID, OpKey: "z8"})

	// 甲投自己、乙也投甲 → p_1 拿到 2/2
	mustVote(t, eng, tenant, rid, a.AgentToken, room.VoteInput{ProposalID: strp(p1.ProposalID), OpKey: "z9"})
	mustVote(t, eng, tenant, rid, b.AgentToken, room.VoteInput{ProposalID: strp(p1.ProposalID), OpKey: "z10"})

	endIfSummarizing(t, eng, tenant, rid, []string{a.AgentToken, b.AgentToken})
	res := resultOf(t, eng, tenant, rid)
	v := res.Conclusion.Votes[p1.ProposalID]
	if v.Total != 2 || v.Self != 1 || v.Cross != 1 {
		t.Fatalf("票数结构应为 total=2 self=1 cross=1，实际 %+v", v)
	}
	if res.Conclusion.Agreement != "unanimous" {
		t.Fatalf("两人全投同一方案 = 2/2，应判 unanimous，实际 %q（text=%s）",
			res.Conclusion.Agreement, res.Conclusion.Text)
	}
	report := room.OwnerReportOf(res, "http://x/room/1")
	t.Logf("text=%s\n%s", res.Conclusion.Text, report)
	assertConsistent(t, res, report)
	// 自投构成仍然要写清楚 —— 少一分证据，多一分"这是真共识"的误读空间
	if !strings.Contains(report, "票数构成") {
		t.Fatalf("ownerReport 应写明票数构成（有自投时），实际：\n%s", report)
	}
	if strings.Contains(res.Conclusion.Text, "未形成多数") {
		t.Fatalf("2/2 票不该写「未形成多数」：%s", res.Conclusion.Text)
	}
}

// TestSelfVoteOnlySplitTextExplainsWhy 只有提案人自己那一票（1/3）时判 split，
// 且文案要把"这票是自投"说出来 —— split 分支里的自投提示仍然要覆盖到。
func TestSelfVoteOnlySplitTextExplainsWhy(t *testing.T) {
	eng, tenant := newEnv(t)
	f := setupVotingRoom(t, eng, tenant) // 甲提 p_1、乙提 p_2、三人参会

	// 甲只投自己那一票，乙丙弃权 ⇒ p_1:1（self=1），2 票之和 ≤ 3
	mustVote(t, eng, tenant, f.roomID, f.tokens[0], room.VoteInput{ProposalID: strp(f.props[0]), OpKey: "s1"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[1], room.VoteInput{ProposalID: nil, OpKey: "s2"})
	mustVote(t, eng, tenant, f.roomID, f.tokens[2], room.VoteInput{ProposalID: nil, OpKey: "s3"})

	endIfSummarizing(t, eng, tenant, f.roomID, f.tokens)
	res := resultOf(t, eng, tenant, f.roomID)
	if res.Conclusion.Agreement != "split" {
		t.Fatalf("1/3 票应判 split，实际 %q", res.Conclusion.Agreement)
	}
	report := room.OwnerReportOf(res, "http://x/room/1")
	t.Logf("text=%s\n%s", res.Conclusion.Text, report)
	assertConsistent(t, res, report)
	if !strings.Contains(res.Conclusion.Text, "自投") {
		t.Fatalf("结论文字应说明这一票是自投，实际：%s", res.Conclusion.Text)
	}
	if !strings.Contains(report, "票数构成") {
		t.Fatalf("ownerReport 应写明票数构成，实际：\n%s", report)
	}
}
