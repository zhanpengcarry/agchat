package room_test

import (
	"strings"
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestOwnerReportIsPhaseAware ownerReport 在不同阶段必须说不同的话。
//
// 实测（Lead 亲自跑会后的自检）：result 是公开接口，投票还没结束就能读。
// 当时 ownerReport 头部按当前票数写了「未形成多数…」，紧接着「一句话结论：
// 投票已结束，正在等待获胜者撰写概述。」—— agent 把这两行一起贴给主人，
// 得到的是「结论：正在等概述」这种废话。阶段不同，能断言的东西就不同。
func TestOwnerReportIsPhaseAware(t *testing.T) {
	mk := func(state domain.State, text string) *room.Result {
		pid := "p_1"
		return &room.Result{
			State:  state,
			RoomNo: "123 456 789",
			Topic:  "阶段感知",
			Conclusion: room.Conclusion{
				Text: text, Method: "vote", Agreement: "tie",
				Winner: &room.WinnerInfo{ProposalID: pid, Title: "方案甲", OwnerAlias: "甲"},
				Votes:  map[string]room.VoteCount{pid: {Total: 1}},
			},
			Proposals: []room.ProposalView{{ProposalID: pid, Title: "方案甲", By: "甲", Votes: 1}},
			Positions: []room.Position{{Alias: "甲"}},
		}
	}

	// ① 投票中：只能说"进行中"，不许出现"未形成多数"这种结论口吻
	voting := room.OwnerReportOf(mk(domain.StateVoting, "正在投票（已投 1/3）。"), "http://x/room/1")
	if !strings.Contains(voting, "会议进行中") {
		t.Fatalf("投票中应说进行中：\n%s", voting)
	}
	if strings.Contains(voting, "未形成多数") || strings.Contains(voting, "最终选了") {
		t.Fatalf("投票中不许给结论：\n%s", voting)
	}

	// ② 等待概述：票已锁定，可以按票数说，但一句话结论不能是进度提示
	summarizing := room.OwnerReportOf(mk(domain.StateSummarizing, "投票已结束，正在等待获胜者撰写概述。"), "http://x/room/1")
	if strings.Contains(summarizing, "一句话结论：投票已结束，正在等待获胜者撰写概述。") {
		t.Fatalf("不能把进度提示当成一句话结论贴给主人：\n%s", summarizing)
	}
	if !strings.Contains(summarizing, "概述还在等获胜者撰写") {
		t.Fatalf("等待概述阶段应明确说概述还没到：\n%s", summarizing)
	}

	// ③ 已结束：正常给结论
	ended := room.OwnerReportOf(mk(domain.StateEnded, "2 个方案各得 1 票，未形成多数。"), "http://x/room/1")
	if !strings.Contains(ended, "一句话结论：2 个方案各得 1 票，未形成多数。") {
		t.Fatalf("结束后应带一句话结论：\n%s", ended)
	}
	if !strings.Contains(ended, "主要分歧：") {
		t.Fatalf("结束后应有分歧段：\n%s", ended)
	}
}
