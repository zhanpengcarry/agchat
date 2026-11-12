package room_test

import (
	"context"
	"testing"
	"time"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestReclaimReturnsSameAgent 认回：同 password + 同 alias → 新 token、同 agentId、不占新座位。
func TestReclaimReturnsSameAgent(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "认回", MaxAgents: 4, MaxRounds: 5, MaxMinutes: 600,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "稳妥派-mock0")

	// 模拟 agent 环境重启：token 丢了、座位还占着。
	// 30 秒内它没再发过请求（把时钟往前推），此时允许认回。
	now += room.ActiveWindowMs + 1000

	re, err := eng.Join(ctx, tenant, rid, room.JoinInput{
		Password: cr.Password, Alias: "稳妥派-mock0", Model: "scripted", Reclaim: true,
	}, "h")
	if err != nil {
		t.Fatalf("认回失败: %v", err)
	}
	if !re.Reclaimed {
		t.Fatal("响应应标记 reclaimed=true")
	}
	if re.AgentID != a.AgentID {
		t.Fatalf("认回必须回到同一个 agentId：原 %s，现 %s", a.AgentID, re.AgentID)
	}
	if re.AgentToken == a.AgentToken {
		t.Fatal("认回必须换发新 token")
	}
	// 新 token 立刻可用
	if _, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(re.AgentToken), nil, 0); err != nil {
		t.Fatalf("新 token 应立即可用: %v", err)
	}
	// 旧 token 立刻失效
	if _, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(a.AgentToken), nil, 0); err == nil {
		t.Fatal("旧 token 必须立刻失效")
	}
}

// TestReclaimRejectedWhenActive 活着的时候不允许顶掉它（防双开）。
func TestReclaimRejectedWhenActive(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()

	cr := mustCreate(t, eng, tenant, room.CreateInput{Topic: "防双开", MaxAgents: 4, MaxRounds: 5, MaxMinutes: 600})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "小李 千问")

	// 刚刚 join 过 → 30 秒内有活动
	_, err := eng.Join(ctx, tenant, rid, room.JoinInput{
		Password: cr.Password, Alias: "小李 千问", Model: "x", Reclaim: true,
	}, "h")
	if codeOf(err) != "member_active" {
		t.Fatalf("活跃成员认回应 409 member_active，实际 %v", err)
	}

	// 它自己正常用 token 仍然没问题
	if _, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(a.AgentToken), nil, 0); err != nil {
		t.Fatalf("活跃成员的原 token 应正常: %v", err)
	}
}

// TestReclaimUnknownAliasCreatesNewMember alias 不存在 → 走正常新建。
func TestReclaimUnknownAliasCreatesNewMember(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()

	cr := mustCreate(t, eng, tenant, room.CreateInput{Topic: "新人", MaxAgents: 4, MaxRounds: 5, MaxMinutes: 600})
	rid := cr.Room.ID
	mustJoin(t, eng, tenant, rid, cr.Password, "甲")

	res, err := eng.Join(ctx, tenant, rid, room.JoinInput{
		Password: cr.Password, Alias: "从没来过的人", Model: "x", Reclaim: true,
	}, "h")
	if err != nil {
		t.Fatalf("alias 不存在时应正常新建: %v", err)
	}
	if res.Reclaimed {
		t.Fatal("新建不应标记 reclaimed")
	}
	if res.SpeakingOrder != 2 {
		t.Fatalf("应是第 2 位入场，实际 %d", res.SpeakingOrder)
	}
}

// TestReclaimDoesNotTakeExtraSeat 认回不占新座位：满员后原成员仍可认回，第 N+1 人仍然 room_full。
func TestReclaimDoesNotTakeExtraSeat(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })

	const n = 4
	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "满员认回", MaxAgents: n, MaxRounds: 5, MaxMinutes: 600,
	})
	rid := cr.Room.ID
	joined := make([]*room.JoinResult, 0, n)
	for i := 0; i < n; i++ {
		joined = append(joined, mustJoin(t, eng, tenant, rid, cr.Password, "成员"+string(rune('A'+i))))
	}

	// 满员后第 5 个人进不来
	if _, err := eng.Join(ctx, tenant, rid, room.JoinInput{Password: cr.Password, Alias: "第五人", Model: "x"}, "h"); codeOf(err) != "room_full" {
		t.Fatalf("满员应 409 room_full，实际 %v", err)
	}

	// 成员 A 认回：座位没变多，仍然满员
	now += room.ActiveWindowMs + 1000
	re, err := eng.Join(ctx, tenant, rid, room.JoinInput{
		Password: cr.Password, Alias: "成员A", Model: "x", Reclaim: true,
	}, "h")
	if err != nil {
		t.Fatalf("满员房间里原成员应能认回: %v", err)
	}
	if re.AgentID != joined[0].AgentID {
		t.Fatalf("认回应拿到原来的 agentId")
	}

	// 认回之后，新面孔依然进不来（说明没有偷偷多占一个座位）
	if _, err := eng.Join(ctx, tenant, rid, room.JoinInput{Password: cr.Password, Alias: "第六人", Model: "x"}, "h"); codeOf(err) != "room_full" {
		t.Fatalf("认回不应扩大容量，新面孔仍应 room_full，实际 %v", err)
	}
}

// TestReclaimNeedsPassword 只凭 alias 认回必须被拒（否则谁猜到名字就能接管）。
func TestReclaimNeedsPassword(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()
	now := int64(1_700_000_000_000)
	eng.SetClock(func() int64 { return now })

	cr := mustCreate(t, eng, tenant, room.CreateInput{Topic: "密码边界", MaxAgents: 4, MaxRounds: 5, MaxMinutes: 600})
	rid := cr.Room.ID
	mustJoin(t, eng, tenant, rid, cr.Password, "主席")
	now += room.ActiveWindowMs + 1000

	_, err := eng.Join(ctx, tenant, rid, room.JoinInput{
		Password: "WRONG-PASS", Alias: "主席", Model: "x", Reclaim: true,
	}, "h")
	if codeOf(err) != "wrong_password" {
		t.Fatalf("密码错时认回必须 401 wrong_password（不能只凭 alias 接管），实际 %v", err)
	}
}

var _ = time.Second
