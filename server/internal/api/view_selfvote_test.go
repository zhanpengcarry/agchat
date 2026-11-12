package api

import (
	"strings"
	"testing"

	"agchat/internal/domain"
	"agchat/internal/room"
)

// TestResultPageShowsSelfVoteBreakdown 结果页必须把「几票是自投」显示出来。
//
// 结果页是整个人类视角的终点：README 那句"你只看这一页"。v2.2 把获胜主序改成跨票之后，
// 「3 票」和「3 票（其中 2 票是提案人自投）」是完全不同的两件事——ownerReport 已经写明，
// 页面不写就等于对只看页面的人隐瞒。
func TestResultPageShowsSelfVoteBreakdown(t *testing.T) {
	mkResult := func(voteCount room.VoteCount, conclusionText string) *room.Result {
		pid := "p_1"
		return &room.Result{
			State:  domain.StateEnded,
			RoomNo: "123 456 789",
			Topic:  "要不要把定价改成月付",
			Conclusion: room.Conclusion{
				Text:      conclusionText,
				Method:    "vote",
				Winner:    &room.WinnerInfo{ProposalID: pid, Title: "方案甲", OwnerAlias: "甲", OwnerModel: "m"},
				Votes:     map[string]room.VoteCount{pid: voteCount},
				Agreement: "split",
			},
			Proposals: []room.ProposalView{{ProposalID: pid, Title: "方案甲", By: "甲", Votes: voteCount.Total}},
			Positions: []room.Position{{Alias: "甲", Model: "m", VotedFor: &pid}},
		}
	}

	// ① 有自投 → 页面必须写明构成（获胜行与方案行都要）
	withSelf := mkResult(room.VoteCount{Total: 2, Self: 1, Cross: 1},
		"未形成多数。得票最高的是「方案甲」（2/2 票），其中 1 票为提案人自投。")
	html, err := renderPage(withSelf, "r_x", 10)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	page := string(html)
	if !strings.Contains(page, "1 票为提案人自投") {
		t.Fatalf("结果页没写自投构成：\n%s", page)
	}
	if strings.Count(page, "票为提案人自投") < 2 {
		t.Fatalf("获胜行与方案行都该带自投构成，实际只出现 %d 次", strings.Count(page, "票为提案人自投"))
	}

	// ② 没有自投 → 不该出现这句（别把干净的多数说得像有问题）
	noSelf := mkResult(room.VoteCount{Total: 3, Self: 0, Cross: 3}, "选出方案「方案甲」（3/3 票，全体一致）")
	html2, err := renderPage(noSelf, "r_x", 10)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if strings.Contains(string(html2), "自投") {
		t.Fatalf("没有自投时页面不该提「自投」：\n%s", html2)
	}
}
