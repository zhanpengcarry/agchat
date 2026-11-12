package meeting

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

/* ══════════════════════════════════════════════════════════════════════════
   D. 收敛与兜底【17-21】
   ══════════════════════════════════════════════════════════════════════════ */

// finishWithWinner 跑到投票、让唯一方案获胜、并等房间结束。
// 用于需要"有获胜者"的结果类断言（17 / 22 / 23）。
func finishWithWinner(t *testing.T, srv *server, summaryTimeout time.Duration) *meeting {
	t.Helper()
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(2)
	if !m.waitState("debating", 20*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}

	// 第 1 轮跑完，进第 2 轮提一个方案
	m.driveToRound(2)
	deadline := time.Now().Add(20 * time.Second)
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
		time.Sleep(80 * time.Millisecond)
	}
	if len(m.props.order) == 0 {
		t.Fatalf("没能产出任何方案，无法构造获胜者")
	}
	win := m.props.order[0]

	m.driveToVoting(120 * time.Second)
	if !m.waitState("voting", 30*time.Second) {
		t.Fatalf("进不了 voting，当前 %s", m.state())
	}
	for _, mm := range m.members {
		if r := m.voteAvoidingSelf(mm, win); r.Status != 200 && r.Status != 201 {
			t.Logf("提示：%s 投票失败 HTTP %d：%s", mm.alias, r.Status, r.snippet())
		}
	}

	wait := 30*time.Second + summaryTimeout
	if !m.waitEnded(wait) {
		t.Fatalf("投票完成后房间没有结束（当前 %s）—— 可能在等概述时卡死", m.state())
	}
	return m
}

// 【17】获胜者不写概述 → 超时后出现机械摘要，result 完整
func TestD17_SummaryTimeoutFallback(t *testing.T) {
	srv := newServer(t, "-summary-timeout", "2s")
	started := time.Now()
	m := finishWithWinner(t, srv, 2*time.Second)
	elapsed := time.Since(started)

	res := m.result()
	summary, _ := res["summary"].(map[string]any)
	txt, _ := summary["text"].(string)
	if strings.TrimSpace(txt) == "" {
		t.Errorf("【17】获胜者没写概述、超时后 summary.text 仍为空 —— 规格要求服务端机械生成摘要兜底")
	} else {
		t.Logf("【17】%.1fs 后拿到兜底摘要：%s", elapsed.Seconds(), firstN(txt, 80))
	}

	// result 结构必须完整，不能因为兜底就缺块
	for _, key := range []string{"conclusion", "summary", "proposals", "positions", "timeline", "ownerReport", "cost"} {
		if _, ok := res[key]; !ok {
			t.Errorf("【17】兜底路径下 result 缺少字段 %q", key)
		}
	}
	if w := winnerID(res); w == "" {
		t.Errorf("【17】有方案且有人投时 winner 不应为 null")
	}
}

// 【18】整轮无人 → 连续 3 个空轮后自动进 voting（v2 语义：空轮 round 不递增，同一轮重开）
func TestD18_EmptyRoundsTriggerVoting(t *testing.T) {
	srv := newServer(t, "-turn-timeout", "1s")
	m := newMeeting(t, srv, map[string]any{"maxRounds": 10})
	m.joinN(2)

	if !m.waitState("debating", 20*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}
	// 全程不说话，等服务端连续跳过 6 个回合
	if !m.waitState("voting", 40*time.Second) {
		t.Fatalf("【18】连续 6 次回合超时后没有进入 voting（当前 %s）—— 整场会卡死", m.state())
	}
	t.Logf("【18】无人发言，连续超时后自动进入 voting")
}

// 【19】0 人进入 → 房间关闭，result 写「无人参会」，不卡死
func TestD19_ZeroMembers(t *testing.T) {
	srv := newServer(t, "-briefing", "2s")
	m := newMeeting(t, srv, nil)

	// 一个人都不进，等 briefing 超时
	if !m.waitEnded(30 * time.Second) {
		t.Fatalf("【19】0 人参会时房间没有关闭（当前 %s）—— 会永远挂着", m.stateOfRoom())
	}
	res := m.result()
	if st, _ := res["state"].(string); st != "ended" {
		t.Errorf("【19】0 人参会的房间 state = %q，期望 ended", st)
	}
	report, _ := res["ownerReport"].(string)
	all := report + conclusionText(res)
	if !strings.Contains(all, "无人") && !strings.Contains(all, "没有") && !strings.Contains(all, "无参会") {
		t.Logf("【19】0 人参会的结果文案：%q（未匹配到「无人」字样，请人工确认措辞）", firstN(all, 160))
	} else {
		t.Logf("【19】0 人参会 → 房间已关闭，文案：%s", firstN(all, 100))
	}
}

// stateOfRoom 在没有任何成员时也能读房间状态。
func (m *meeting) stateOfRoom() string {
	r := call(m.t, m.srv.base, http.MethodGet, "/rooms/"+m.roomID+"/result", "", nil)
	if r.Status != 200 {
		return fmt.Sprintf("result HTTP %d", r.Status)
	}
	var out map[string]any
	if err := json.Unmarshal(r.Body, &out); err != nil {
		return "result 非 JSON"
	}
	st, _ := out["state"].(string)
	return st
}

// 【20】只有 1 人进入 → 跳过辩论直接出结论，不因「无人可投」卡死
func TestD20_SingleMember(t *testing.T) {
	srv := newServer(t, "-briefing", "2s", "-summary-timeout", "2s")
	m := newMeeting(t, srv, nil)
	m.join("solo")

	if !m.waitEnded(30 * time.Second) {
		t.Fatalf("【20】单人参会时房间没有结束（当前 %s）—— 卡在等第二个人或等投票", m.stateOfRoom())
	}
	res := m.result()
	if txt := conclusionText(res); strings.TrimSpace(txt) == "" {
		t.Errorf("【20】单人参会也必须产出结论，实际为空")
	} else {
		t.Logf("【20】单人参会 → 直接出结论：%s", firstN(txt, 80))
	}
	if summary, ok := res["summary"].(map[string]any); ok {
		if txt, _ := summary["text"].(string); strings.TrimSpace(txt) == "" {
			t.Logf("提示：【20】单人参会时 summary.text 为空（规格要求也出概述，建议补）")
		}
	}
}

// 【21】长轮询挂在 inbox 上时状态变化 → 立即返回（≤2s），不等满 wait
func TestD21_LongPollWakesOnStateChange(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, nil)
	m.joinN(2)
	if !m.waitState("debating", 20*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}

	waiter := m.members[1]
	cur := m.inbox(waiter).Cursor

	type result struct {
		status  int
		elapsed time.Duration
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		r := call(t, srv.base, http.MethodGet,
			fmt.Sprintf("/rooms/%s/inbox?since=%d&wait=15", m.roomID, cur), waiter.token, nil)
		done <- result{r.Status, time.Since(start)}
	}()

	// 制造一次状态变化：让当前持回合的人发言
	time.Sleep(400 * time.Millisecond)
	spoke := false
	for _, mm := range m.members {
		if m.inbox(mm).YourTurn {
			if r := m.speak(mm, "触发一次事件，唤醒长轮询。", 0, ""); r.Status == 200 || r.Status == 201 {
				spoke = true
			}
		}
	}
	if !spoke {
		t.Skip("【21】找不到持回合的人，无法触发状态变化")
	}

	select {
	case res := <-done:
		t.Logf("【21】长轮询在 %v 后返回（wait=15）", res.elapsed)
		if res.elapsed > 2*time.Second {
			t.Errorf("【21】状态变化后长轮询仍等了 %v 才返回，超过 2s —— 说明没有事件唤醒，只是等到超时",
				res.elapsed)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("【21】长轮询挂住 8s 未返回，事件唤醒失效")
	}
}

/* ══════════════════════════════════════════════════════════════════════════
   E. 结果契约【22-24】
   ══════════════════════════════════════════════════════════════════════════ */

// 【22】result.ownerReport 非空、可直接贴给主人（含议题与获胜方案标题）
func TestE22_OwnerReport(t *testing.T) {
	// 显式给小的 summary 超时：本用例验的是"结束之后的报告"，不该被产品默认值绑住
	srv := newServer(t, "-summary-timeout", "5s")
	m := finishWithWinner(t, srv, 5*time.Second)

	res := m.result()
	report, _ := res["ownerReport"].(string)
	if strings.TrimSpace(report) == "" {
		t.Fatalf("【22】result.ownerReport 为空 —— agent 没有可以直接贴给主人的东西")
	}

	// 必须体现"这次会议是关于什么的"
	topicShort := m.topicPrefix(20)
	if !strings.Contains(report, topicShort) {
		t.Errorf("【22】ownerReport 里没有议题（期望包含 %q）\n  实际：%s", topicShort, firstN(report, 300))
	}
	// 必须体现"选了哪个方案"
	if w := winnerID(res); w != "" {
		title := m.props.title[w]
		if title != "" && !strings.Contains(report, title) {
			t.Errorf("【22】ownerReport 里没有获胜方案标题 %q\n  实际：%s", title, firstN(report, 300))
		}
	}
	// 一段话就够，不该是一大坨 JSON
	if len([]rune(report)) > 1200 {
		t.Errorf("【22】ownerReport 长达 %d 字，不适合直接贴给主人", len([]rune(report)))
	}
	t.Logf("【22】ownerReport（%d 字）：%s", len([]rune(report)), firstN(report, 120))
}

// 【23】result 里不得出现 agentToken / adminKey / password
func TestE23_ResultHasNoSecrets(t *testing.T) {
	srv := newServer(t, "-summary-timeout", "5s")
	m := finishWithWinner(t, srv, 5*time.Second)

	r := call(t, srv.base, http.MethodGet, "/rooms/"+m.roomID+"/result", "", nil)
	if r.Status != 200 {
		t.Fatalf("【23】读 result 失败 HTTP %d", r.Status)
	}
	body := string(r.Body)

	for _, label := range []string{"agenttoken", "adminkey", "password", "tokenhash"} {
		if strings.Contains(strings.ToLower(body), label) {
			t.Errorf("【23】公开结果里出现了凭证字段 %q", label)
		}
	}
	for _, mm := range m.members {
		if mm.token != "" && strings.Contains(body, mm.token) {
			t.Errorf("【23】公开结果里出现了 %s 的 agentToken 原文", mm.alias)
		}
	}
	if m.adminKey != "" && strings.Contains(body, m.adminKey) {
		t.Errorf("【23】公开结果里出现了 adminKey 原文")
	}
	if m.password != "" && strings.Contains(body, m.password) {
		t.Errorf("【23】公开结果里出现了房间密码原文")
	}
}

// 【24】跨租户打新端点（proposals / votes / summary）→ 全 404
func TestE24_CrossTenantNewEndpoints(t *testing.T) {
	srv := newServer(t)
	victim := newMeeting(t, srv, nil)
	victim.join("victim")

	attacker := newMeeting(t, srv, nil)
	intruder := attacker.join("intruder")

	endpoints := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/rooms/" + victim.roomID + "/proposals",
			map[string]any{"title": "x", "body": "y", "opKey": newOpKey()}},
		{http.MethodGet, "/rooms/" + victim.roomID + "/proposals", nil},
		{http.MethodPost, "/rooms/" + victim.roomID + "/votes",
			map[string]any{"proposalId": "p_1", "opKey": newOpKey()}},
		{http.MethodGet, "/rooms/" + victim.roomID + "/votes", nil},
		{http.MethodPost, "/rooms/" + victim.roomID + "/summary",
			map[string]any{"text": "外部注入的概述", "opKey": newOpKey()}},
		{http.MethodGet, "/rooms/" + victim.roomID + "/summary", nil},
	}

	for _, e := range endpoints {
		r := call(t, srv.base, e.method, e.path, intruder.token, e.body)
		if r.Status != http.StatusNotFound {
			t.Errorf("【24】跨租户 %s %s 期望 404，实际 %d\n  响应 %s",
				e.method, e.path, r.Status, r.snippet())
		} else {
			t.Logf("【24】%s %s → 404", e.method, e.path)
		}
		if r.Status >= 500 {
			t.Errorf("【24】%s %s 返回 5xx", e.method, e.path)
		}
	}
}

/* ── 小工具 ──────────────────────────────────────────────────────────── */

func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
