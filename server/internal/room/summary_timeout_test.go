package room_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// setupSummarizingRoom 造一个已经进入 summarizing 的房间：3 人、3 轮、一个方案、全员投它。
// 返回 roomID / token 列表（主席在前）/ 方案 ID / 时钟指针。
func setupSummarizingRoom(t *testing.T, summaryTO time.Duration) (*room.Engine, string, string, []string, string, func(d time.Duration)) {
	t.Helper()
	eng, tenant := newEnvWith(t, time.Hour, summaryTO)
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })
	advance := func(d time.Duration) { now += d.Milliseconds() }

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "概述阶段", MaxAgents: 3, MaxRounds: 3, MaxMinutes: 600, TokenBudget: 5000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")
	c := mustJoin(t, eng, tenant, rid, cr.Password, "丙")
	toks := []string{a.AgentToken, b.AgentToken, c.AgentToken}

	r1 := mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "甲的立场。", OpKey: "z1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙的立场。", OpKey: "z2"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "丙的立场。", OpKey: "z3"})
	p, err := eng.Propose(context.Background(), tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "甲的可执行方案", Body: "由甲在下周三前完成，成功标准是留存。", OpKey: "z4",
	})
	if err != nil {
		t.Fatalf("提案失败: %v", err)
	}
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应乙。", ReplyTo: intp(r1.Seq), OpKey: "z5"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(r1.Seq), OpKey: "z6"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(r1.Seq), OpKey: "z7"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "支持甲方案。", SupportProposalID: p.ProposalID, OpKey: "z8"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "支持甲方案。", SupportProposalID: p.ProposalID, OpKey: "z9"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "支持甲方案。", SupportProposalID: p.ProposalID, OpKey: "z10"})
	for i, tok := range toks {
		mustVote(t, eng, tenant, rid, tok, room.VoteInput{ProposalID: strp(p.ProposalID), OpKey: "v" + string(rune('a'+i))})
	}
	if res := resultOf(t, eng, tenant, rid); res.State != domain.StateSummarizing {
		t.Fatalf("应进入 summarizing，实际 %s", res.State)
	}
	return eng, tenant, rid, toks, p.ProposalID, advance
}

// TestSummaryTooLateMustNotClaimSuccess 超时后再 POST 概述，绝不能回「成功」。
//
// 实测（Lead 亲自当 agent）：投票结算后系统 60s 兜底，他在第 85 秒把写好的概述发上来，
// 拿到 200 + ok:true —— 而 result 里躺着机械摘要。agent 于是理直气壮地告诉主人
// "我写了概述"，而主人永远看不到他真正想说的那句「这 4 票全是自投，别当共识」。
// 一个说谎的 200 比一个诚实的失败更贵。
func TestSummaryTooLateMustNotClaimSuccess(t *testing.T) {
	summaryTO := 4 * time.Second
	eng, tenant, rid, toks, _, advance := setupSummarizingRoom(t, summaryTO)
	ctx := context.Background()

	// 获胜者长时间不活动 → 超时，机械兜底并结束
	advance(summaryTO + time.Second)
	if res := resultOf(t, eng, tenant, rid); res.State != domain.StateEnded {
		t.Fatalf("超时后应 ended，实际 %s", res.State)
	}

	late := "【简要概述】我来晚了：这场四张票全是自投，别当共识。"
	out, err := eng.Summary(ctx, tenant, rid, domain.HashToken(toks[0]), room.SummaryInput{
		Text: late, OpKey: "late-1",
	})
	if err != nil {
		t.Fatalf("迟到的概述不该报错，应给出诚实的结构化结果: %v", err)
	}
	if out.OK || out.Saved {
		t.Fatalf("迟到的概述必须明确 ok=false/saved=false，实际 %+v", out)
	}
	if out.Reason != "summary_too_late" {
		t.Fatalf("reason 应为 summary_too_late，实际 %q", out.Reason)
	}
	if !strings.Contains(out.Note, "没有被采用") {
		t.Fatalf("note 必须明确告诉 agent「你的文字没进结果」，实际：%s", out.Note)
	}
	res := resultOf(t, eng, tenant, rid)
	if strings.Contains(res.Summary.Text, "四张票全是自投") {
		t.Fatal("迟到的概述不该被写进结果")
	}
	if res.Summary.Author != "系统（机械兜底）" {
		t.Fatalf("结果里的概述作者应是机械兜底，实际 %q", res.Summary.Author)
	}
}

// TestSummaryIdempotentReplayStaysSuccess 同一份文字重发仍是幂等成功（§八 #11）。
func TestSummaryIdempotentReplayStaysSuccess(t *testing.T) {
	eng, tenant, rid, toks, _, _ := setupSummarizingRoom(t, time.Hour)
	ctx := context.Background()

	text := "【简要概述】\n1. 结论：按甲方案执行。\n2. 主要分歧：无。\n3. 下一步：下周三复盘。"
	first, err := eng.Summary(ctx, tenant, rid, domain.HashToken(toks[0]), room.SummaryInput{Text: text, OpKey: "s1"})
	if err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	if !first.OK || !first.Saved || first.Reason != "" {
		t.Fatalf("首次应 ok/saved 且无 reason，实际 %+v", first)
	}
	again, err := eng.Summary(ctx, tenant, rid, domain.HashToken(toks[0]), room.SummaryInput{Text: text, OpKey: "s2"})
	if err != nil {
		t.Fatalf("重放不该报错: %v", err)
	}
	if !again.OK || !again.Saved || again.Reason != "already_saved" {
		t.Fatalf("同一份文字重放应是幂等成功，实际 %+v", again)
	}
}

// TestKeyPointsIgnoreJunkFinalRound 收尾轮一句废话不该盖掉三轮的实质发言。
//
// 实测（Lead 亲自跑会）：收尾轮发了一句「第 1 轮就发言的测试（应被拒吗？」，
// 立场矩阵立刻把这句话摆到他名字下面，三轮实质发言全被无视。
// 收尾轮优先是为了拿到"最终站位"，所以只有**真的带了 supportProposalId** 的才算。
func TestKeyPointsIgnoreJunkFinalRound(t *testing.T) {
	eng, tenant := newEnv(t)
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "立场矩阵取样", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")

	r1 := mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{
		Text: "我的立场：先做结构化结论，因为它让分歧可测；唤醒依赖别人开放接口，不是我们能决定的事。", OpKey: "k1",
	})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "乙的立场。", OpKey: "k2"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应乙。", ReplyTo: intp(r1.Seq), OpKey: "k3"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(r1.Seq), OpKey: "k4"})
	// 收尾轮：甲发的是一句没有 supportProposalId 的废话
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "这是一句没有表态的测试废话。", ReplyTo: intp(r1.Seq), OpKey: "k5"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "我也没表态。", ReplyTo: intp(r1.Seq), OpKey: "k6"})

	res := resultOf(t, eng, tenant, rid)
	var got string
	for _, p := range res.Positions {
		if p.Alias == "甲" {
			got = p.Summary
		}
	}
	if strings.Contains(got, "没有表态的测试废话") {
		t.Fatalf("收尾轮的废话不该占据立场矩阵：%q", got)
	}
	if !strings.Contains(got, "结构化结论") {
		t.Fatalf("应取到实质发言：%q", got)
	}

	// 带 supportProposalId 的收尾表态仍然拥有最高优先级
	eng2, tenant2 := newEnv(t)
	ctx2 := context.Background()
	cr2 := mustCreate(t, eng2, tenant2, room.CreateInput{
		Topic: "立场矩阵取样", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid2 := cr2.Room.ID
	a2 := mustJoin(t, eng2, tenant2, rid2, cr2.Password, "甲")
	b2 := mustJoin(t, eng2, tenant2, rid2, cr2.Password, "乙")
	r1 = mustPost(t, eng2, tenant2, rid2, a2.AgentToken, room.PostInput{Text: "甲的实质立场。", OpKey: "m1"})
	mustPost(t, eng2, tenant2, rid2, b2.AgentToken, room.PostInput{Text: "乙的立场。", OpKey: "m2"})
	p2, err := eng2.Propose(ctx2, tenant2, rid2, domain.HashToken(a2.AgentToken), room.ProposeInput{
		Title: "甲方案", Body: "可执行内容。", OpKey: "m3",
	})
	if err != nil {
		t.Fatal(err)
	}
	mustPost(t, eng2, tenant2, rid2, a2.AgentToken, room.PostInput{Text: "回应乙。", ReplyTo: intp(r1.Seq), OpKey: "m4"})
	mustPost(t, eng2, tenant2, rid2, b2.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(r1.Seq), OpKey: "m5"})
	mustPost(t, eng2, tenant2, rid2, a2.AgentToken, room.PostInput{
		Text: "我最终支持甲方案，理由是它可证伪。", SupportProposalID: p2.ProposalID, OpKey: "m6",
	})
	mustPost(t, eng2, tenant2, rid2, b2.AgentToken, room.PostInput{Text: "我也支持。", SupportProposalID: p2.ProposalID, OpKey: "m7"})
	res2 := resultOf(t, eng2, tenant2, rid2)
	for _, p := range res2.Positions {
		if p.Alias == "甲" {
			if !strings.Contains(p.Summary, "最终支持甲方案") {
				t.Fatalf("带 supportProposalId 的收尾表态应优先，实际 %q", p.Summary)
			}
		}
	}
}
