package room_test

import (
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestFrozenResultFromOlderSchemaIsRecomputed 升级后，老房间的冻结快照不能"冻住旧形状"。
//
// 从零验证实测到的坑：房间结束时会把结论序列化进 rooms.result_json，之后 /result
// 默认读那份快照。升级到 v2.5 后，**同一个接口在升级前后返回两种 schema**（老房间没有
// decisionSet、没有 schemaVersion）。对下游来说这比"慢一点"糟得多。
//
// 修法：快照带 schemaVersion；版本不一致就按当前代码重算（结论是确定性的，
// 重算只多出新字段，不会改变已有内容）。
func TestFrozenResultFromOlderSchemaIsRecomputed(t *testing.T) {
	eng, _ := newEnv(t)

	live := &domain.Room{
		TenantID: "t_x", ID: "r_x", RoomNo: "123456789", Topic: "直播议题",
		State: domain.StateEnded, MaxRounds: 3, MaxAgents: 2,
		TokenBudget: 1000000, BudgetUsed: 10, CreatedAt: 1, EndedAt: 2,
	}

	// ① 老版本冻结的快照（没有 schemaVersion）→ 必须重算，重算出来的 topic 是"直播议题"
	oldSnapshot := `{"state":"ended","roomNo":"123 456 789","topic":"冻结议题",
		"conclusion":{"text":"旧结论","method":"vote","agreement":"unanimous"},
		"cost":{"estTokens":1,"messages":0,"rounds":3,"minutes":1}}`
	aggOld := &room.Agg{Room: &domain.Room{}}
	*aggOld.Room = *live
	aggOld.Room.ResultJSON = oldSnapshot

	got := eng.ResultFor(aggOld)
	if got.SchemaVersion != room.ResultSchemaVersion {
		t.Fatalf("老快照应被判过旧并重算，实际 schemaVersion=%q（说明直接返回了旧形状）", got.SchemaVersion)
	}
	if got.DecisionSet == nil {
		t.Fatal("老快照重算后必须带 decisionSet")
	}
	if got.Topic != "直播议题" {
		t.Fatalf("重算应使用实时数据（topic=直播议题），实际 %q", got.Topic)
	}

	// ② 同版本快照 → 走快照（topic 为"冻结议题"，证明没有重算）
	newSnapshot := `{"schemaVersion":"` + room.ResultSchemaVersion + `","state":"ended",
		"roomNo":"123 456 789","topic":"冻结议题",
		"conclusion":{"text":"冻结结论","method":"vote","agreement":"unanimous"},
		"cost":{"estTokens":1,"messages":0,"rounds":3,"minutes":1}}`
	aggNew := &room.Agg{Room: &domain.Room{}}
	*aggNew.Room = *live
	aggNew.Room.ResultJSON = newSnapshot

	frozen := eng.ResultFor(aggNew)
	if frozen.Topic != "冻结议题" {
		t.Fatalf("同版本快照应直接返回（topic 应为「冻结议题」），实际 %q", frozen.Topic)
	}
}
