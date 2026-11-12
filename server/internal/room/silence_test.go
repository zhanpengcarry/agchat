package room_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestMixedSilenceOthersStillGetTurns 锁死这个形态：
// **主席全程沉默，另两人必须每轮都拿到发言权**，会议要能正常跑满 10 轮。
//
// 曾经的 bug：某人一超时，就把"这一轮"判成空轮并原地重开、又回到第一位，
// 于是只要第一个人不说话，后面所有人永远轮不到，最后被连续空轮拖进投票 ——
// 真实会议里"有人慢/有人掉线"是常态，这条不成立产品就不成立。
func TestMixedSilenceOthersStillGetTurns(t *testing.T) {
	turnTO := time.Second
	eng, tenant := newEnvWith(t, turnTO, time.Hour)
	ctx := context.Background()
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })

	const rounds = 10
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "混合沉默", MaxAgents: 3, MaxRounds: rounds, MaxMinutes: 600, TokenBudget: 5000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "A-沉默者") // 主席，全程不说话
	b := mustJoin(t, eng, tenant, rid, cr.Password, "B-正常")
	c := mustJoin(t, eng, tenant, rid, cr.Password, "C-正常")
	if !a.IsModerator {
		t.Fatal("第 1 个进场者应是主席")
	}

	spoken := 0
	for round := 1; round <= rounds; round++ {
		// 主席（第一个发言位）超时 → 只跳过它，发言权交给 B
		now += turnTO.Milliseconds()
		snap, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(b.AgentToken), nil, 0)
		if err != nil {
			t.Fatalf("R%d inbox 失败: %v", round, err)
		}
		if !snap.YourTurn {
			t.Fatalf("R%d：主席超时后应轮到 B，实际 yourTurn=false（round=%d notes=%v）",
				round, snap.Round, snap.Notes)
		}
		if snap.Round != round {
			t.Fatalf("R%d：轮次被错误推进/重置，实际 round=%d", round, snap.Round)
		}

		// B 发言
		rp := room.PostInput{Text: "B 在第 " + string(rune('0'+round%10)) + " 轮的观点。", OpKey: "b" + string(rune('a'+round))}
		if round >= 2 && round < rounds {
			rp.ReplyTo = intp(1)
		}
		if _, err := eng.Post(ctx, tenant, rid, domain.HashToken(b.AgentToken), rp); err != nil {
			t.Fatalf("R%d B 发言失败: %v", round, err)
		}
		spoken++

		// C 发言
		rc := room.PostInput{Text: "C 在第 " + string(rune('0'+round%10)) + " 轮的观点。", OpKey: "c" + string(rune('a'+round))}
		if round >= 2 && round < rounds {
			rc.ReplyTo = intp(2)
		}
		if _, err := eng.Post(ctx, tenant, rid, domain.HashToken(c.AgentToken), rc); err != nil {
			t.Fatalf("R%d C 发言失败: %v", round, err)
		}
		spoken++
	}

	res := resultOf(t, eng, tenant, rid)
	t.Logf("混合沉默：跑完 %d 轮，B/C 各发言 %d 次，cost.rounds=%d messages=%d state=%s",
		rounds, spoken/2, res.Cost.Rounds, res.Cost.Messages, res.State)

	if res.Cost.Rounds != rounds {
		t.Fatalf("应跑满 %d 轮，实际 rounds=%d", rounds, res.Cost.Rounds)
	}
	if res.Cost.Messages != rounds*2 {
		t.Fatalf("B/C 共应发言 %d 条，实际 %d", rounds*2, res.Cost.Messages)
	}
	if res.State != domain.StateVoting {
		t.Fatalf("跑满轮数后应进投票，实际 %s", res.State)
	}
}

// TestAllSilenceStillReachesVoting 对照组：3 人全沉默 → 3 个空轮后仍要进投票（原有行为别破坏）。
func TestAllSilenceStillReachesVoting(t *testing.T) {
	turnTO := time.Second
	eng, tenant := newEnvWith(t, turnTO, time.Hour)
	ctx := context.Background()
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "全沉默", MaxAgents: 3, MaxRounds: 10, MaxMinutes: 600,
	})
	rid := cr.Room.ID
	var toks []string
	for _, name := range []string{"甲", "乙", "丙"} {
		toks = append(toks, mustJoin(t, eng, tenant, rid, cr.Password, name).AgentToken)
	}

	// 推进 len(agents) 个 turnTO → 本轮所有人被跳过且零发言 → 空轮 1 次，round 不推进
	for i := 0; i < 3; i++ {
		now += turnTO.Milliseconds()
		if _, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(toks[0]), nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := eng.Inbox(ctx, tenant, rid, domain.HashToken(toks[0]), nil, 0)
	if snap.Round != 1 {
		t.Fatalf("一整轮无人发言不应推进 round，实际 round=%d", snap.Round)
	}
	skipped := false
	for _, n := range snap.Notes {
		if strings.Contains(n, "无人发言，已跳过") {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("notes 应提示空轮已跳过: %v", snap.Notes)
	}

	// 继续推进到 3 个空轮 → 进投票
	steps := 3
	for snap.State == domain.StateDebating && steps < 40 {
		now += turnTO.Milliseconds()
		if _, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(toks[0]), nil, 0); err != nil {
			t.Fatal(err)
		}
		res := resultOf(t, eng, tenant, rid)
		snap.State = res.State
		steps++
	}
	if snap.State != domain.StateVoting {
		t.Fatalf("连续空轮应进投票，实际 %s（steps=%d）", snap.State, steps)
	}
	t.Logf("全沉默：%d 个 turnTO 后进入 voting（= %d 空轮 × 3 人/轮）", steps, room.EmptyRoundsToVoting)
}
