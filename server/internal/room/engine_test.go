package room_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agchat/internal/domain"
	"agchat/internal/room"
	"agchat/internal/store"
)

func newEnv(t *testing.T) (*room.Engine, string) { return newEnvWith(t, time.Hour, time.Hour) }

func newEnvWith(t *testing.T, turnTO, summaryTO time.Duration) (*room.Engine, string) {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("打开内存库: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	eng := room.New(st, turnTO, 30*time.Second, summaryTO)
	eng.SetVoteGrace(0) // 用例默认关掉宽限期（否则每次全员投完都要等 10s）；宽限期本身有专门用例
	tenant, err := st.NewTenant(context.Background())
	if err != nil {
		t.Fatalf("建租户: %v", err)
	}
	return eng, tenant
}

func mustCreate(t *testing.T, eng *room.Engine, tenant string, in room.CreateInput) *room.CreateResult {
	t.Helper()
	res, err := eng.Create(context.Background(), tenant, in)
	if err != nil {
		t.Fatalf("建会失败: %v", err)
	}
	return res
}

func mustJoin(t *testing.T, eng *room.Engine, tenant, roomID, password, alias string) *room.JoinResult {
	t.Helper()
	res, err := eng.Join(context.Background(), tenant, roomID, room.JoinInput{
		Password: password, Alias: alias, Model: "test-model",
	}, "test.local")
	if err != nil {
		t.Fatalf("加入失败(%s): %v", alias, err)
	}
	return res
}

func codeOf(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func mustPost(t *testing.T, eng *room.Engine, tenant, roomID, token string, in room.PostInput) *room.PostResult {
	t.Helper()
	res, err := eng.Post(context.Background(), tenant, roomID, domain.HashToken(token), in)
	if err != nil {
		t.Fatalf("发言失败: %v (code=%s)", err, codeOf(err))
	}
	return res
}

func mustVote(t *testing.T, eng *room.Engine, tenant, roomID, token string, in room.VoteInput) *room.VoteResult {
	t.Helper()
	res, err := eng.Vote(context.Background(), tenant, roomID, domain.HashToken(token), in)
	if err != nil {
		t.Fatalf("投票失败: %v (code=%s)", err, codeOf(err))
	}
	return res
}

func resultOf(t *testing.T, eng *room.Engine, tenant, roomID string) *room.Result {
	t.Helper()
	var res *room.Result
	if err := eng.View(context.Background(), tenant, roomID, func(agg *room.Agg) error {
		res = eng.ResultFor(agg)
		return nil
	}); err != nil {
		t.Fatalf("取结果失败: %v", err)
	}
	return res
}

func intp(v int) *int       { return &v }
func strp(v string) *string { return &v }

// =============================================================================
// 1. 主席唯一性（并发 join 下恰好一个）
// =============================================================================

func TestModeratorIsExactlyOneUnderConcurrentJoin(t *testing.T) {
	eng, tenant := newEnv(t)
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "并发入场", MaxAgents: 6, MaxRounds: 3, MaxMinutes: 60,
	})

	const n = 6
	var wg sync.WaitGroup
	results := make([]*room.JoinResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = eng.Join(context.Background(), tenant, cr.Room.ID, room.JoinInput{
				Password: cr.Password, Alias: "a" + string(rune('A'+i)), Model: "m",
			}, "h")
		}(i)
	}
	wg.Wait()

	mods := 0
	orders := map[int]bool{}
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("并发 join 失败: %v", errs[i])
		}
		if r.IsModerator {
			mods++
		}
		if orders[r.SpeakingOrder] {
			t.Fatalf("speakingOrder 重复: %d", r.SpeakingOrder)
		}
		orders[r.SpeakingOrder] = true
	}
	if mods != 1 {
		t.Fatalf("主席必须恰好 1 个，实际 %d 个", mods)
	}
	// 主席必须是第 1 个进入的（speakingOrder == 1）
	for _, r := range results {
		if r.IsModerator && r.SpeakingOrder != 1 {
			t.Fatalf("主席的 speakingOrder 应为 1，实际 %d", r.SpeakingOrder)
		}
	}
}

// =============================================================================
// 2/3/6. 计票与降级投票见 tally_test.go（独立文件，避免硬编码 seq）

// =============================================================================
// 3. 概述超时兜底
// =============================================================================

func TestSummaryTimeoutFallsBackMechanically(t *testing.T) {
	eng, tenant := newEnvWith(t, time.Hour, time.Second)
	ctx := context.Background()
	_ = ctx
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })

	cr := mustCreate(t, eng, tenant, room.CreateInput{Topic: "概述兜底", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 600})
	a := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "乙")
	rid := cr.Room.ID

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "立场甲。", OpKey: "s1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "立场乙。", OpKey: "s2"})

	// 必须**有方案**才会进 summarizing：无获胜者时按 §八 #8 直接 ended
	p1, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "唯一方案", Body: "方案内容", OpKey: "s2p",
	})
	if err != nil {
		t.Fatalf("提案失败: %v", err)
	}

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应乙。", ReplyTo: intp(2), OpKey: "s3"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(1), OpKey: "s4"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "支持唯一方案。", SupportProposalID: p1.ProposalID, OpKey: "s5"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "也支持。", SupportProposalID: p1.ProposalID, OpKey: "s6"})

	mustVote(t, eng, tenant, rid, a.AgentToken, room.VoteInput{ProposalID: nil, OpKey: "s7"}) // 提案人这次弃权（v2.2 也允许自投，这里只要一张 abstain）
	mustVote(t, eng, tenant, rid, b.AgentToken, room.VoteInput{ProposalID: strp(p1.ProposalID), OpKey: "s8"})

	res := resultOf(t, eng, tenant, rid)
	if res.State != domain.StateSummarizing {
		t.Fatalf("有获胜者且全员投完应进入 summarizing，实际 %s", res.State)
	}

	// 没人写概述 → 推进时钟触发兜底
	now += 2000
	res = resultOf(t, eng, tenant, rid)
	if res.State != domain.StateEnded {
		t.Fatalf("概述超时应结束会议，实际 %s", res.State)
	}
	if res.Summary.Text == "" {
		t.Fatal("必须产出概述（机械兜底）")
	}
	if !strings.Contains(res.Summary.Author, "机械兜底") {
		t.Fatalf("兜底概述的作者应标注为机械兜底，实际 %q", res.Summary.Author)
	}
}

// =============================================================================
// 4. 连续超时跳轮（6 次 → 直接进投票）
// =============================================================================

func TestTextTooLongRejectedWithoutCharge(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	_ = ctx
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "长度上限", MaxAgents: 2, MaxRounds: 2, MaxMinutes: 60, MaxOutputTokens: 10,
	})
	a := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "甲")
	_ = mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "乙") // 满员才开场
	rid := cr.Room.ID

	before := resultOf(t, eng, tenant, rid).Cost.EstTokens
	long := strings.Repeat("这是一段太长的发言。", 10)
	_, err := eng.Post(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.PostInput{Text: long, OpKey: "L1"})
	if codeOf(err) != "text_too_long" {
		t.Fatalf("超长发言应 422 text_too_long，实际 %v", err)
	}
	after := resultOf(t, eng, tenant, rid).Cost.EstTokens
	if after != before {
		t.Fatalf("被拒的发言不应计费: before=%d after=%d", before, after)
	}
}

// =============================================================================
// 6. 无方案降级 finalStance
// =============================================================================

func TestNoProposalDegradesToFinalStance(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	_ = ctx
	cr := mustCreate(t, eng, tenant, room.CreateInput{Topic: "降级投票", MaxAgents: 2, MaxRounds: 2, MaxMinutes: 60})
	a := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "乙")
	rid := cr.Room.ID

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "立场甲。", OpKey: "d1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "立场乙。", OpKey: "d2"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "收尾甲。", OpKey: "d3"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "收尾乙。", OpKey: "d4"})

	// 有方案时不允许用 finalStance —— 这里没有方案，所以允许
	mustVote(t, eng, tenant, rid, a.AgentToken, room.VoteInput{FinalStance: domain.StanceAgree, OpKey: "d5"})
	mustVote(t, eng, tenant, rid, b.AgentToken, room.VoteInput{FinalStance: domain.StanceAgree, OpKey: "d6"})

	res := resultOf(t, eng, tenant, rid)
	if res.Conclusion.Method != "stance" {
		t.Fatalf("无方案时应走 stance 降级，实际 %s", res.Conclusion.Method)
	}
	if res.Conclusion.Winner != nil {
		t.Fatalf("无方案时 winner 应为 null")
	}
	// §九：旧字段保留
	if _, ok := res.Conclusion.Votes[domain.StanceAgree]; !ok {
		t.Fatalf("降级时 votes 应含 agree 字段: %+v", res.Conclusion.Votes)
	}
}

// =============================================================================
// 完整流程 + ownerReport
// =============================================================================

func TestFullMeetingFlowAndOwnerReport(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	_ = ctx
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "把定价从年付改成月付，要不要？", Rules: "不许客套",
		MaxAgents: 3, MaxRounds: 3, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "小周 Claude")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "小李 千问")
	c := mustJoin(t, eng, tenant, rid, cr.Password, "老张 DeepSeek")

	if !strings.Contains(a.Brief.Prompt, "你是本次会议的主席") {
		t.Fatal("主席的 prompt 应含主席版说明")
	}
	if strings.Contains(b.Brief.Prompt, "你是本次会议的主席") {
		t.Fatal("普通参会者的 prompt 不应含主席版说明")
	}
	if !strings.Contains(a.Brief.Prompt, "est_tokens（中文约等于字数）") {
		t.Fatal("prompt 里的长度上限措辞应按 Lead 裁决用 est_tokens")
	}

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "我支持改月付，年付门槛太高。", OpKey: "f1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "我反对，续费率会掉。", OpKey: "f2"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "折中：月付默认 + 年付折扣。", OpKey: "f3"})

	p1, _ := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "新旧价格并行一个月", Body: "先灰度 20% 新用户……", OpKey: "f4",
	})
	p2, _ := eng.Propose(ctx, tenant, rid, domain.HashToken(c.AgentToken), room.ProposeInput{
		Title: "月付默认 + 年付 8 折", Body: "两条腿走路", OpKey: "f5",
	})

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应小李的续费率担心。", ReplyTo: intp(2), OpKey: "f6"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "回应小周的门槛论点。", ReplyTo: intp(1), OpKey: "f7"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "回应两人的分歧。", ReplyTo: intp(2), OpKey: "f8"})

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "我支持方案一。", SupportProposalID: p1.ProposalID, OpKey: "f9"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "我支持方案二。", SupportProposalID: p2.ProposalID, OpKey: "f10"})
	mustPost(t, eng, tenant, rid, c.AgentToken, room.PostInput{Text: "我支持方案一。", SupportProposalID: p1.ProposalID, OpKey: "f11"})

	mustVote(t, eng, tenant, rid, a.AgentToken, room.VoteInput{ProposalID: strp(p2.ProposalID), OpKey: "f12"})
	mustVote(t, eng, tenant, rid, b.AgentToken, room.VoteInput{ProposalID: strp(p1.ProposalID), OpKey: "f13"})
	mustVote(t, eng, tenant, rid, c.AgentToken, room.VoteInput{ProposalID: strp(p1.ProposalID), OpKey: "f14"})

	// 获胜者（a）写概述
	sr, err := eng.Summary(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.SummaryInput{
		Text: "会议结论：新旧价格并行一个月，用数据决定谁留下。", OpKey: "f15",
	})
	if err != nil {
		t.Fatalf("获胜者写概述失败: %v", err)
	}
	if !sr.OK {
		t.Fatal("概述应成功")
	}

	res := resultOf(t, eng, tenant, rid)
	if res.State != domain.StateEnded {
		t.Fatalf("写完后应 ended，实际 %s", res.State)
	}
	if res.Conclusion.Winner == nil || res.Conclusion.Winner.ProposalID != p1.ProposalID {
		t.Fatalf("获胜方案应为 p_1: %+v", res.Conclusion.Winner)
	}
	if res.Conclusion.Agreement != "majority" {
		t.Fatalf("2/3 应为 majority，实际 %s", res.Conclusion.Agreement)
	}
	if res.Summary.Author != "小周 Claude" {
		t.Fatalf("概述作者应为获胜者，实际 %q", res.Summary.Author)
	}
	if len(res.Proposals) != 2 || len(res.Positions) != 3 || len(res.Timeline) == 0 {
		t.Fatalf("结果结构不完整: proposals=%d positions=%d timeline=%d",
			len(res.Proposals), len(res.Positions), len(res.Timeline))
	}

	report := room.OwnerReportOf(res, "http://x/room/123456789")
	for _, want := range []string{"【agchat 会议结果】", "最终选了：", "一句话结论：", "主要分歧：", "完整结果页："} {
		if !strings.Contains(report, want) {
			t.Fatalf("ownerReport 缺 %q：\n%s", want, report)
		}
	}
}

// TestReplyToRequiredInDebateRound §四：第 2..N-1 轮必须 replyTo。
func TestReplyToRequiredInDebateRound(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	_ = ctx
	cr := mustCreate(t, eng, tenant, room.CreateInput{Topic: "必须回复", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60})
	a := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "乙")
	rid := cr.Room.ID

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "立场甲。", OpKey: "r1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "立场乙。", OpKey: "r2"})

	_, err := eng.Post(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.PostInput{Text: "自说自话。", OpKey: "r3"})
	if codeOf(err) != "reply_to_required" {
		t.Fatalf("交锋轮缺 replyTo 应 422 reply_to_required，实际 %v", err)
	}
}

// TestProposalLimit 每人最多 2 个方案。
func TestProposalLimit(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	_ = ctx
	cr := mustCreate(t, eng, tenant, room.CreateInput{Topic: "方案上限", MaxAgents: 2, MaxRounds: 4, MaxMinutes: 60})
	a := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "乙")
	rid := cr.Room.ID

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "立场甲。", OpKey: "q1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "立场乙。", OpKey: "q2"})

	for i := 0; i < room.MaxProposalsPerAgent; i++ {
		if _, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
			Title: "方案" + string(rune('A'+i)), Body: "正文", OpKey: "q" + string(rune('a'+i)),
		}); err != nil {
			t.Fatalf("第 %d 个方案应成功: %v", i+1, err)
		}
	}
	_, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "第三个", Body: "正文", OpKey: "qz",
	})
	if codeOf(err) != "too_many_proposals" {
		t.Fatalf("第 3 个方案应 409 too_many_proposals，实际 %v", err)
	}
}

// TestVoteFormGuard 有方案时禁止用 finalStance（Lead 追加的守卫）。
func TestVoteFormGuard(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	_ = ctx
	cr := mustCreate(t, eng, tenant, room.CreateInput{Topic: "票型守卫", MaxAgents: 2, MaxRounds: 3, MaxMinutes: 60})
	a := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "乙")
	rid := cr.Room.ID

	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "立场甲。", OpKey: "w1"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "立场乙。", OpKey: "w2"})
	if _, err := eng.Propose(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.ProposeInput{
		Title: "唯一方案", Body: "正文", OpKey: "w3",
	}); err != nil {
		t.Fatal(err)
	}
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "回应乙。", ReplyTo: intp(2), OpKey: "w4"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "回应甲。", ReplyTo: intp(1), OpKey: "w5"})
	mustPost(t, eng, tenant, rid, a.AgentToken, room.PostInput{Text: "收尾甲。", OpKey: "w6"})
	mustPost(t, eng, tenant, rid, b.AgentToken, room.PostInput{Text: "收尾乙。", OpKey: "w7"})

	_, err := eng.Vote(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.VoteInput{
		FinalStance: domain.StanceAgree, OpKey: "w8",
	})
	if codeOf(err) != "wrong_round" {
		t.Fatalf("有方案时用 finalStance 应 403 wrong_round，实际 %v", err)
	}
}
