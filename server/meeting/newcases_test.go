package meeting

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

/* ══════════════════════════════════════════════════════════════════════════
   概述：正常路径 vs 兜底路径 必须分得开
   ══════════════════════════════════════════════════════════════════════════ */

// submitSummary 由获胜者主动提交概述。
func (m *meeting) submitSummary(mm *member, text string) resp {
	return call(m.t, m.srv.base, http.MethodPost, "/rooms/"+m.roomID+"/summary", mm.token, map[string]any{
		"text": text, "opKey": newOpKey(),
	})
}

// finishToVoting 跑到 voting 并让第一个方案全票通过，返回获胜者。
// summaryTimeout 传 0 表示用服务端默认（60s）。
func (m *meeting) finishToVoting(summaryTimeout time.Duration) *member {
	m.t.Helper()
	if !m.waitState("debating", 25*time.Second) {
		m.t.Fatalf("等不到 debating：%s", m.state())
	}
	m.driveToRound(2)

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) && len(m.props.order) == 0 {
		for _, mm := range m.members {
			st := m.inbox(mm)
			if st.State != "debating" {
				break
			}
			if st.YourTurn {
				m.propose(mm, "新旧价格并行一个月", "先灰度 20% 新用户，跑满一个续费周期再全量。")
				m.speak(mm, "补充方案细节。", m.lastOtherSeq(mm), "")
			}
		}
		time.Sleep(60 * time.Millisecond)
	}
	if len(m.props.order) == 0 {
		m.t.Fatalf("没能产出方案")
	}
	win := m.props.order[0]

	m.driveToVoting(120 * time.Second)
	if !m.waitState("voting", 30*time.Second) {
		m.t.Fatalf("进不了 voting，当前 %s", m.state())
	}
	for _, mm := range m.members {
		if r := m.voteAvoidingSelf(mm, win); r.Status != 200 && r.Status != 201 {
			m.t.Logf("提示：%s 投票失败 HTTP %d", mm.alias, r.Status)
		}
	}
	// 获胜者 = 方案提交者
	for _, mm := range m.members {
		if m.props.owner[win] == mm.alias {
			return mm
		}
	}
	m.t.Fatalf("找不到获胜者对应的成员")
	return nil
}

// C-new-1：获胜者主动提交概述 → 立即 ended（≤2s），不等满 timeout
func TestCNew1_SummarySubmittedEndsImmediately(t *testing.T) {
	// 故意把 summary-timeout 设大（20s）：如果提交后还要等 timeout 才能结束，这条必红
	// -vote-grace 0：本用例验的是"提交概述后立即结束"，走的是正常结算路径、
	// 没有掉线者，投票宽限期对它是纯噪音。grace 默认值另由 TestPresetAbstain* 覆盖。
	srv := newServer(t, "-summary-timeout", "20s", "-vote-grace", "0")
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(2)
	winner := m.finishToVoting(0)

	if !m.waitState("summarizing", 10*time.Second) {
		t.Fatalf("投票完成后没有进入 summarizing（当前 %s）", m.state())
	}

	start := time.Now()
	if r := m.submitSummary(winner, "两人一致认为应先灰度再全量，保留年付作为折扣选项。"); r.Status != 200 && r.Status != 201 {
		t.Fatalf("获胜者提交概述失败 HTTP %d：%s", r.Status, r.snippet())
	}
	if !m.waitEnded(5 * time.Second) {
		t.Fatalf("提交概述后 5s 仍未结束（当前 %s）—— 正常路径被兜底超时吃掉了", m.state())
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("提交概述到 ended 用了 %v，超过 2s；summary-timeout 设的是 20s，说明没有立即结算", elapsed)
	} else {
		t.Logf("【C-new-1】提交概述后 %v 立即结束", elapsed.Round(time.Millisecond))
	}

	res := m.result()
	summary, _ := res["summary"].(map[string]any)
	txt, _ := summary["text"].(string)
	if !strings.Contains(txt, "灰度") {
		t.Errorf("【C-new-1】结果里的概述不是获胜者写的那段，实际：%q", firstN(txt, 100))
	}
}

// C-new-2：获胜者不提交 → 等满 summary-timeout 后出现机械摘要且 ended
func TestCNew2_SummaryTimeoutFallsBack(t *testing.T) {
	const timeout = 3
	srv := newServer(t, "-summary-timeout", "3s", "-vote-grace", "0") // 正常结算路径，宽限期对它是噪音
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(2)
	_ = m.finishToVoting(0)

	if !m.waitState("summarizing", 10*time.Second) {
		t.Fatalf("没有进入 summarizing（当前 %s）", m.state())
	}
	start := time.Now()
	if !m.waitEnded(timeout*time.Second + 20*time.Second) {
		t.Fatalf("不提交概述时，超时后也没有结束（当前 %s）", m.state())
	}
	elapsed := time.Since(start)
	if elapsed < time.Duration(timeout)*time.Second {
		t.Errorf("只等了 %v 就结束了，短于 summary-timeout=%ds —— 兜底路径没按配置走", elapsed, timeout)
	}
	res := m.result()
	summary, _ := res["summary"].(map[string]any)
	txt, _ := summary["text"].(string)
	if strings.TrimSpace(txt) == "" {
		t.Errorf("【C-new-2】超时兜底后 summary.text 仍为空")
	} else {
		t.Logf("【C-new-2】%v 后拿到机械摘要：%s", elapsed.Round(time.Second), firstN(txt, 70))
	}
	if st, _ := res["state"].(string); st != "ended" {
		t.Errorf("【C-new-2】state = %q，期望 ended", st)
	}
}

/* ══════════════════════════════════════════════════════════════════════════
   弃权 / 无获胜者 / 时长口径
   ══════════════════════════════════════════════════════════════════════════ */

// C-new-3：3 人房间，2 人投票 + 1 人弃权 → 立即结算，且弃权票计入 votes.abstain
func TestCNew3_AbstainSettlesImmediately(t *testing.T) {
	// timeout 设大：如果弃权没被算作"已投"，就要等 20s；这条会抓到
	// -vote-grace 0：本用例验的是"全员表态后立即结算"，无掉线者，不涉及宽限逻辑
	srv := newServer(t, "-summary-timeout", "20s", "-vote-grace", "0")
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(3)
	if !m.waitState("debating", 25*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}
	m.driveToRound(2)

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) && len(m.props.order) == 0 {
		for _, mm := range m.members {
			st := m.inbox(mm)
			if st.State != "debating" {
				break
			}
			if st.YourTurn {
				m.propose(mm, "只保留月付", "直接改成月付 ¥29。")
				m.speak(mm, "补充。", m.lastOtherSeq(mm), "")
			}
		}
		time.Sleep(60 * time.Millisecond)
	}
	if len(m.props.order) == 0 {
		t.Fatalf("没造出方案")
	}
	pid := m.props.order[0]

	m.driveToVoting(150 * time.Second)
	if !m.waitState("voting", 30*time.Second) {
		t.Fatalf("进不了 voting")
	}

	start := time.Now()
	if r := m.voteAvoidingSelf(m.members[0], pid); r.Status != 200 && r.Status != 201 {
		t.Fatalf("投票失败 HTTP %d：%s", r.Status, r.snippet())
	}
	if r := m.voteAvoidingSelf(m.members[1], pid); r.Status != 200 && r.Status != 201 {
		t.Fatalf("投票失败 HTTP %d：%s", r.Status, r.snippet())
	}
	// 第三个人弃权
	if r := m.vote(m.members[2], ""); r.Status != 200 && r.Status != 201 {
		t.Errorf("【C-new-3】弃权请求被拒 HTTP %d：%s", r.Status, r.snippet())
	}

	if !m.waitState("summarizing", 10*time.Second) && !m.waitEnded(2*time.Second) {
		t.Fatalf("三个人都表态后 10s 仍在 %s —— 弃权没有被当作「已投票」", m.state())
	}
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Errorf("【C-new-3】全员表态到进入结算用了 %v，超过 3s；剩余两人投票+一人弃权应该立即结算", elapsed)
	} else {
		t.Logf("【C-new-3】弃权计入后 %v 立即结算", elapsed.Round(time.Millisecond))
	}

	// 弃权票必须出现在 votes.abstain
	if !m.waitEnded(40 * time.Second) {
		t.Logf("提示：等待 ended 超时，改用当前 result 校验票型")
	}
	res := m.result()
	concl, _ := res["conclusion"].(map[string]any)
	_ = concl
	// 新契约：扁平总票数在 conclusion.voteTotals（旧结构由 flatVotes 兼容）
	fv := flatVotes(res)
	abstain := fv["abstain"]
	if abstain < 1 {
		t.Errorf("【C-new-3】votes.abstain = %v，期望至少 1（弃权票没被计入）\n  实际票型：%v", abstain, fv)
	} else {
		t.Logf("【C-new-3】票型 %v（弃权已计入）", fv)
	}
}

// C-new-4：无任何方案 → 状态序列里不应出现 summarizing，直接 ended
func TestCNew4_NoWinnerSkipsSummarizing(t *testing.T) {
	srv := newServer(t, "-summary-timeout", "20s") // 设大：若误入 summarizing 会明显卡住
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(2)
	if !m.waitState("debating", 25*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}
	m.driveToVoting(120 * time.Second)
	if !m.waitState("voting", 30*time.Second) {
		t.Fatalf("进不了 voting，当前 %s", m.state())
	}

	// 降级投票（无方案）
	for i, mm := range m.members {
		stance := "agree"
		if i%2 == 1 {
			stance = "disagree"
		}
		call(t, srv.base, http.MethodPost, "/rooms/"+m.roomID+"/votes", mm.token, map[string]any{
			"finalStance": stance, "opKey": newOpKey(),
		})
	}

	// 记录状态序列，抓 summarizing 的瞬时出现
	seen := map[string]bool{}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st := m.state()
		seen[st] = true
		if st == "ended" {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if !seen["ended"] {
		t.Fatalf("【C-new-4】无方案场景没有 ended（观察到 %v）", seen)
	}
	if seen["summarizing"] {
		t.Errorf("【C-new-4】无获胜者却进入了 summarizing —— 白等一个不存在的获胜者（观察到 %v）", seen)
	} else {
		t.Logf("【C-new-4】状态序列未出现 summarizing，直接 ended（观察到 %v）", seen)
	}

	res := m.result()
	if w := winnerID(res); w != "" {
		t.Errorf("【C-new-4】无方案时 winner 应为 null，实际 %q", w)
	}
}

/* ══════════════════════════════════════════════════════════════════════════
   成本量：6 人 × 10 轮满场到底烧多少 token
   ══════════════════════════════════════════════════════════════════════════ */

// TestZ_CostOfFullMeeting 跑一场 6 人满 10 轮的会议，打印 cost.estTokens。
//
// 目的：默认 tokenBudget 是 2000 万。如果满场只烧几万，这个上限就是永远不会
// 触发的纸面数字 —— 需要拿真实数字去和预算比对，而不是拍脑袋。
func TestZ_CostOfFullMeeting(t *testing.T) {
	if testing.Short() {
		t.Skip("满场成本测量较慢，-short 下跳过")
	}
	srv := newServer(t, "-summary-timeout", "1s")
	m := newMeeting(t, srv, map[string]any{"maxRounds": 10})
	m.joinN(6)

	if !m.waitState("debating", 30*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}
	// 跑到投票（60 个回合）
	m.driveToVoting(300 * time.Second)
	if !m.waitState("voting", 60*time.Second) {
		t.Fatalf("10 轮没跑完就停了，当前 %s", m.state())
	}
	for _, mm := range m.members {
		m.vote(mm, "") // 全部弃权，尽快结束
	}
	m.waitEnded(60 * time.Second)

	res := m.result()
	cost, _ := res["cost"].(map[string]any)
	est, _ := cost["estTokens"].(float64)
	msgs, _ := cost["messages"].(float64)
	rounds, _ := cost["rounds"].(float64)

	const budget = 20000000
	t.Logf("【成本】6 人 × 10 轮满场：estTokens=%.0f，消息 %.0f 条，轮数 %.0f",
		est, msgs, rounds)
	if est > 0 {
		t.Logf("【成本】占默认预算 2000w 的 %.4f%%；按此推算需要约 %.0f 场满场会议才烧完 2000w",
			est/budget*100, budget/est)
	}
	if est <= 0 {
		t.Errorf("【成本】满场会议的 cost.estTokens = 0，成本核算没生效")
	}
}
