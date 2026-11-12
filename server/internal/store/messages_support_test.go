package store

import (
	"context"
	"testing"

	"agchat/internal/domain"
)

// TestMessageSupportProposalRoundTrip 锁住「收尾轮表态支持哪个方案」真的落库。
//
// 迁移 003 加了 messages.support_proposal_id 这一列，但 store 的列清单（msgCols）一直
// 没带上它 —— 写入时被丢、读回来永远是空串。实证：一次真实演练里，收尾轮明确带了
// supportProposalId，结果 positions 拿不到表态，立场矩阵退回"最长那条"（第 1 轮）。
// 列存在 ≠ 数据存在，这条用例盯的就是这一步。
func TestMessageSupportProposalRoundTrip(t *testing.T) {
	s, err := Open("file:t_msg_support?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const tenant = "t_support"
	ctx := context.Background()

	if err := s.WithTenant(ctx, tenant, func(tx *Tx) error {
		if err := tx.InsertMessage(&domain.Message{
			RoomID: "r_1", Seq: 1, Round: 1, AgentID: "m_1", Alias: "甲", Model: "m",
			Text: "第 1 轮发言。", TS: "t", OpKey: "op1",
		}); err != nil {
			return err
		}
		return tx.InsertMessage(&domain.Message{
			RoomID: "r_1", Seq: 2, Round: 3, AgentID: "m_1", Alias: "甲", Model: "m",
			Text: "收尾表态：我支持 p_1。", SupportProposalID: "p_1", TS: "t", OpKey: "op2",
		})
	}); err != nil {
		t.Fatalf("写消息失败: %v", err)
	}

	var got []*domain.Message
	if err := s.WithTenant(ctx, tenant, func(tx *Tx) error {
		var err error
		got, err = tx.ListMessages("r_1", 0)
		return err
	}); err != nil {
		t.Fatalf("读消息失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应有 2 条消息，实际 %d", len(got))
	}
	if got[0].SupportProposalID != "" {
		t.Fatalf("第 1 轮那条不该有 supportProposalId，实际 %q", got[0].SupportProposalID)
	}
	if got[1].SupportProposalID != "p_1" {
		t.Fatalf("收尾轮那条的 supportProposalId 丢了：%q（列存在但没写进去）", got[1].SupportProposalID)
	}
	if got[1].Round != 3 {
		t.Fatalf("round 也要一起回来，实际 %d", got[1].Round)
	}
}
