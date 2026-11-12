package room_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestInboxReadCursorAndNotes 锁住「两套游标」不再靠 agent 猜。
//
// 实测事故：两位干净 agent 都把 inbox 的 cursor 当成 /messages 的 since 传了进去
// （前者是事件序号，远大于消息 seq），拿到的永远是空数组，而 notes 还在说"有 N 条未读"。
// 于是他们各重复读了一次，白付了读取费用，最后靠试错才发现是两套指针。
//
// 修法不是写文档，而是让服务端**把该传的数字说出来**：
//   - inbox 暴露 readCursor（消息 seq 水位）
//   - notes 里直接写 GET /messages?since=<readCursor>
func TestInboxReadCursorAndNotes(t *testing.T) {
	eng, tenant := newEnv(t)
	ctx := context.Background()

	cr := mustCreate(t, eng, tenant, room.CreateInput{
		Topic: "两套游标", MaxAgents: 2, MaxRounds: 2, MaxMinutes: 60, TokenBudget: 2000000,
	})
	rid := cr.Room.ID
	a := mustJoin(t, eng, tenant, rid, cr.Password, "甲")
	b := mustJoin(t, eng, tenant, rid, cr.Password, "乙")

	// 甲先发言，乙什么都不读
	if _, err := eng.Post(ctx, tenant, rid, domain.HashToken(a.AgentToken), room.PostInput{Text: "甲的第一条。", OpKey: "c1"}); err != nil {
		t.Fatalf("发言失败: %v", err)
	}

	snap, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(b.AgentToken), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ReadCursor != 0 {
		t.Fatalf("乙还没读过任何消息，readCursor 应为 0，实际 %d", snap.ReadCursor)
	}
	if snap.Cursor <= int64(snap.ReadCursor) {
		t.Logf("提示：事件 cursor=%d、readCursor=%d（正常情况下前者更大，这正是两者不能混用的原因）",
			snap.Cursor, snap.ReadCursor)
	}
	joined := strings.Join(snap.Notes, "\n")
	want := fmt.Sprintf("GET /messages?since=%d", snap.ReadCursor)
	if !strings.Contains(joined, want) {
		t.Fatalf("notes 应直接给出 %q，让 agent 不用猜该传哪个指针：\n%s", want, joined)
	}

	// 乙读完之后：readCursor 推到最新 seq，未读提示消失
	rr, err := eng.Read(ctx, tenant, rid, domain.HashToken(b.AgentToken), 0, true)
	if err != nil {
		t.Fatalf("读正文失败: %v", err)
	}
	if len(rr.Messages) == 0 {
		t.Fatalf("按 readCursor=0 读应该拿到甲那一条，实际空数组")
	}
	snap2, err := eng.Inbox(ctx, tenant, rid, domain.HashToken(b.AgentToken), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if snap2.ReadCursor != rr.NextSince && snap2.ReadCursor < rr.Messages[len(rr.Messages)-1].Seq {
		t.Fatalf("读完后 readCursor 应推进到最新 seq（收到 %d，nextSince %d）", snap2.ReadCursor, rr.NextSince)
	}
	if strings.Contains(strings.Join(snap2.Notes, "\n"), "条未读") {
		t.Fatalf("读完后不该再提示未读：\n%s", strings.Join(snap2.Notes, "\n"))
	}
}
