package api

import (
	_ "embed"
	"fmt"
	"html/template"
	"strconv"
	"strings"

	"agchat/internal/domain"
	"agchat/internal/room"
)

//go:embed view.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

var stateLabel = map[domain.State]string{
	domain.StateBriefing:    "等待入场",
	domain.StateDebating:    "讨论中",
	domain.StateVoting:      "投票中",
	domain.StateSummarizing: "撰写概述",
	domain.StateEnded:       "会议结束",
}

type proposalRow struct {
	ID    string
	Title string
	By    string
	Votes int
	// VoteNote 是票数构成注脚（v2.2）：只有存在自投时才有内容。
	VoteNote string
	Bar      template.HTMLAttr
	Body     string
}

type posRow struct {
	Alias     string
	Model     string
	Summary   string
	VotedFor  string
	VoteClass string
}

type tlRow struct {
	Seq   int
	Round int
	Alias string
	Gist  string
}

type pageModel struct {
	Topic          string
	RoomNo         string
	MetaLine       string
	BadgeClass     string
	BadgeText      string
	VerdictClass   string
	ConclusionText string
	WinnerLine     string
	Disagreement   string

	Proposals []proposalRow
	Positions []posRow
	Timeline  []tlRow

	SummaryText   string
	SummaryAuthor string
	CostLine      string
	RoomID        string
}

// renderPage 渲染只读结果页（v2）。
//
// 与 v1 视觉语言一致：标题 / 结论大字 / 票数条 / 概述 / 方案表 / 立场矩阵 / 时间线 / 成本脚注。
// 全部内联样式，不引任何外链。
func renderPage(r *room.Result, roomID string, maxRounds int) ([]byte, error) {
	ended := r.State == domain.StateEnded
	badgeClass := "pending"
	switch {
	case !ended:
		badgeClass = "pending"
	case r.Conclusion.Agreement == "unanimous":
		badgeClass = "ok"
	case r.Conclusion.Agreement == "majority":
		badgeClass = "mid"
	default:
		badgeClass = "bad"
	}
	badgeText := stateLabel[r.State]
	if badgeText == "" {
		badgeText = string(r.State)
	}

	// 票数条：按方案（或降级立场）比例
	maxVotes := 1
	for _, p := range r.Proposals {
		if p.Votes > maxVotes {
			maxVotes = p.Votes
		}
	}
	props := make([]proposalRow, 0, len(r.Proposals))
	for _, p := range r.Proposals {
		pct := int(float64(p.Votes) / float64(maxVotes) * 100)
		props = append(props, proposalRow{
			ID: p.ProposalID, Title: p.Title, By: p.By, Votes: p.Votes,
			VoteNote: voteNote(r.Conclusion.Votes[p.ProposalID]),
			Bar:      template.HTMLAttr(fmt.Sprintf(`style="width:%d%%"`, pct)),
			Body:     p.Body,
		})
	}

	positions := make([]posRow, 0, len(r.Positions))
	titleByID := make(map[string]string, len(r.Proposals))
	for _, pr := range r.Proposals {
		titleByID[pr.ProposalID] = pr.Title
	}
	for _, p := range r.Positions {
		row := posRow{Alias: p.Alias, Model: p.Model, Summary: p.Summary, VotedFor: "弃权", VoteClass: "none"}
		if p.VotedFor != nil {
			// 页面是给人的：显示方案标题，不要甩一个 p_1 让人去别处对照（拿不到标题才退回 ID）
			if title := titleByID[*p.VotedFor]; title != "" {
				row.VotedFor = title
			} else {
				row.VotedFor = *p.VotedFor
			}
			row.VoteClass = "agree"
		}
		positions = append(positions, row)
	}

	timeline := make([]tlRow, 0, len(r.Timeline))
	for _, t := range r.Timeline {
		timeline = append(timeline, tlRow{Seq: t.Seq, Round: t.Round, Alias: t.Alias, Gist: t.Gist})
	}

	roundsText := "等待入场"
	if r.State != domain.StateBriefing {
		roundsText = fmt.Sprintf("%d/%d", r.Cost.Rounds, maxRounds)
	}
	metaLine := fmt.Sprintf("%d 位 agent · %s · %d 条发言 · 用时 %v 分钟 · 估算 %s est_tokens",
		len(r.Positions), roundsText, r.Cost.Messages, r.Cost.Minutes, comma(r.Cost.EstTokens))

	winnerLine := ""
	if w := r.Conclusion.Winner; w != nil {
		vc := r.Conclusion.Votes[w.ProposalID]
		winnerLine = fmt.Sprintf("获胜方案：%s（提交者 %s）· %d 票%s",
			w.Title, w.OwnerAlias, vc.Total, voteNote(vc))
	}
	disagreement := room.DisagreementLine(r)

	verdictClass := ""
	if ended {
		verdictClass = "is-ended"
	}
	costLine := ""
	for _, s := range r.Summary.Sections {
		if s.H == "成本" {
			costLine = s.P
		}
	}

	m := pageModel{
		Topic: r.Topic, RoomNo: r.RoomNo, MetaLine: metaLine,
		BadgeClass: badgeClass, BadgeText: badgeText, VerdictClass: verdictClass,
		ConclusionText: r.Conclusion.Text, WinnerLine: winnerLine, Disagreement: disagreement,
		Proposals: props, Positions: positions, Timeline: timeline,
		SummaryText: r.Summary.Text, SummaryAuthor: r.Summary.Author,
		CostLine: costLine, RoomID: roomID,
	}

	var sb strings.Builder
	if err := pageTmpl.Execute(&sb, m); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

// renderMissing 是会议不存在时的 404 页。
func renderMissing(roomID string) []byte {
	esc := template.HTMLEscapeString(roomID)
	return []byte(`<!doctype html><meta charset="utf-8"><title>会议不存在</title>
       <div style="font:16px/1.8 -apple-system,'PingFang SC',sans-serif;max-width:560px;margin:120px auto;text-align:center;color:#475569">
         <h1 style="font-size:22px;color:#0f172a">会议不存在或已过期</h1>
         <p>` + esc + `</p></div>`)
}

// voteNote 把「多少票是提案人自己投的」写进结果页。
//
// 为什么非要在页面上说：v2.2 的获胜主序改成了跨票，人类看的那一页却只显示总票数。
// 「3 票」和「3 票（其中 2 票是提案人自投）」在决策意义上完全不同 ——
// ownerReport 已经写明这件事，页面不写，等于只对不看报告的读者隐瞒。
func voteNote(vc room.VoteCount) string {
	if vc.Self <= 0 {
		return ""
	}
	return fmt.Sprintf("（其中 %d 票为提案人自投、%d 票来自其他参会者）", vc.Self, vc.Cross)
}

func comma(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, ",")
	if neg {
		out = "-" + out
	}
	return out
}
