package room_test

import (
	"strings"
	"testing"
	"time"

	"agchat/internal/room"
)

// TestPromptVoteDisciplineMatchesV22 锁住 prompt 里的投票纪律必须是 v2.2 口径。
//
// 为什么值得一条测试：
//
//	v2.2 把「禁止自投」改成了「允许自投 + 跨票主序」，代码改了、单测也加了，
//	但 prompt 里那句「不要投给自己提的方案」没人注意 —— 而 prompt 才是
//	每个 agent 进房后真正会读的那段话。文案不改，代码改了也白改：
//	agent 仍然会被要求投给别人，2/3 的"多数"仍然是规则产物（docs/07 §十五）。
//
// 这条测试盯的是"规则的**发布形态**"，不是数据字段。
func TestPromptVoteDisciplineMatchesV22(t *testing.T) {
	eng, tenant := newEnvWith(t, time.Hour, time.Hour)
	eng.SetClock(func() int64 { return 1_700_000_000_000 })

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "prompt 投票纪律", MaxAgents: 6, MaxRounds: 10, MaxMinutes: 600,
	})
	j := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "甲")
	p := j.Brief.Prompt

	// ① 必须正面允许自投（v2.2）
	if !strings.Contains(p, "可以投给你自己提的方案") {
		t.Fatalf("prompt 没说「可以投给你自己提的方案」，v2.2 的规则没有发布出去：\n%s", p)
	}
	// ② 绝不能残留 v2.1 的禁令 —— 它是这条规则失效的唯一原因
	if strings.Contains(p, "不要投给自己") {
		t.Fatalf("prompt 仍写着「不要投给自己」，与 v2.2 相反：\n%s", p)
	}
	// ③ 服务端已不存在这个错误码，prompt 里也不许承诺它
	if strings.Contains(p, "self_vote_forbidden") {
		t.Fatalf("prompt 仍在承诺已删除的错误码 self_vote_forbidden：\n%s", p)
	}
	// ④ 主序与"自投堆不出多数"必须说清楚（措辞与 skill.md 一致，别再写成一条额外审查规则）
	for _, want := range []string{"跨票", "堆不出多数"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt 缺少 v2.3 机制说明「%s」：\n%s", want, p)
		}
	}
}

// TestPromptVoteDisciplineSameForModeratorAndMembers 保证主席版和普通版的投票纪律一致。
//
// 主席在平票时是权重票，如果两份 prompt 的投票纪律措辞不同，主席会按另一套规则投票。
func TestPromptVoteDisciplineSameForModeratorAndMembers(t *testing.T) {
	eng, tenant := newEnvWith(t, time.Hour, time.Hour)
	eng.SetClock(func() int64 { return 1_700_000_000_000 })

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "两份 prompt", MaxAgents: 3, MaxRounds: 10, MaxMinutes: 600,
	})
	mod := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "主席")
	plain := mustJoin(t, eng, tenant, cr.Room.ID, cr.Password, "普通")

	if !mod.IsModerator || plain.IsModerator {
		t.Fatalf("主席判定不对：mod=%v plain=%v", mod.IsModerator, plain.IsModerator)
	}
	discipline := func(prompt string) []string {
		var out []string
		for _, l := range strings.Split(prompt, "\n") {
			if strings.Contains(l, "投票") || strings.Contains(l, "跨票") || strings.Contains(l, "自投") {
				out = append(out, l)
			}
		}
		return out
	}
	a, b := discipline(mod.Brief.Prompt), discipline(plain.Brief.Prompt)
	if strings.Join(a, "\n") != strings.Join(b, "\n") {
		t.Fatalf("主席与普通成员的投票纪律不一致：\n主席：\n%s\n普通：\n%s",
			strings.Join(a, "\n"), strings.Join(b, "\n"))
	}
	if len(a) == 0 {
		t.Fatal("prompt 里找不到任何投票相关句子，测试自身失效了")
	}
}
