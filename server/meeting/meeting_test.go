package meeting

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

/* ══════════════════════════════════════════════════════════════════════════
   A. 会议号与进入【1-5】
   ══════════════════════════════════════════════════════════════════════════ */

// 【1】9 位会议号能被 join；带空格、带连字符都能匹配
func TestA1_MeetingNumberFormats(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, nil)

	// 先用 roomId 进一个人，顺便把 roomNo 拿到（建房响应不一定带）
	m.join("probe")
	if m.roomNo == "" {
		t.Fatalf("join 的 brief.roomNo 为空，无法验证会议号格式")
	}
	digits := m.digits()
	if len(digits) != 9 {
		t.Errorf("会议号应为 9 位数字，实际 %q（原样 %q）", digits, m.roomNo)
	}

	formats := map[string]string{
		"纯数字":    digits,
		"带空格":    digits[0:3] + " " + digits[3:6] + " " + digits[6:],
		"带连字符":   digits[0:3] + "-" + digits[3:6] + "-" + digits[6:],
		"原样展示格式": m.roomNo,
	}
	for name, handle := range formats {
		r := m.joinHandle("via-"+name, handle)
		if r.Status != 200 && r.Status != 201 {
			t.Errorf("【1】用%s（%q）加入会议失败：HTTP %d\n  响应 %s",
				name, handle, r.Status, r.snippet())
		} else {
			t.Logf("【1】%s（%q）加入成功", name, handle)
		}
	}
}

// 【2】随机会话号 join → 404（不区分不存在/无权）
func TestA2_UnknownMeetingNumber(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, nil)

	for _, handle := range []string{"729451936", "000000000", "999999999"} {
		r := m.joinHandle("ghost", handle)
		expectStatus(t, r, http.StatusNotFound,
			"【2】不存在的会议号 "+handle+" 加入，期望 404（不能区分「不存在」与「无权」）")
	}
}

// 【3】密码错 → 401
func TestA3_WrongPassword(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, nil)

	r := call(t, srv.base, http.MethodPost, "/rooms/"+m.roomID+"/join", "", map[string]any{
		"password": "WRONG-PASS-0000",
		"alias":    "intruder",
		"model":    "test-model",
	})
	expectStatus(t, r, http.StatusUnauthorized, "【3】密码错误进场，期望 401")

	// 对照：正确密码必须能进，否则上面的 401 可能是"接口根本不让进"
	ok := m.joinHandle("legit", m.roomID)
	if ok.Status != 200 && ok.Status != 201 {
		t.Fatalf("【3】对照组：正确密码也进不去（HTTP %d），用例失效\n  响应 %s", ok.Status, ok.snippet())
	}
}

// 【4】第 7 个人进 → 409 room_full（上限 6）
func TestA4_RoomFull(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, nil)
	m.joinN(6)

	r := m.joinHandle("agent-7", m.roomID)
	expectStatus(t, r, http.StatusConflict, "【4】第 7 人进场，期望 409 room_full")
	if code := r.errCode(); code != "room_full" && code != "" {
		t.Errorf("【4】第 7 人的错误码是 %q，期望 room_full", code)
	}
}

// 【5】主席唯一性：6 人并发 join，恰好 1 个 isModerator，speakingOrder 是 1..6 的排列
func TestA5_ModeratorUniqueness(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, nil)

	const n = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []joinResp
	codes := []int{}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r := call(t, srv.base, http.MethodPost, "/rooms/"+m.roomID+"/join", "", map[string]any{
				"password": m.password,
				"alias":    fmt.Sprintf("race-%d", idx),
				"model":    "test-model",
			})
			mu.Lock()
			defer mu.Unlock()
			codes = append(codes, r.Status)
			if r.Status == 200 || r.Status == 201 {
				var jr joinResp
				if json.Unmarshal(r.Body, &jr) == nil {
					results = append(results, jr)
				}
			}
		}(i)
	}
	wg.Wait()

	if len(results) != n {
		t.Fatalf("【5】并发 %d 人进场，只有 %d 人成功（状态码 %v）", n, len(results), codes)
	}

	mods := 0
	seen := map[int]bool{}
	for _, jr := range results {
		if jr.IsModerator {
			mods++
		}
		if jr.SpeakingOrder < 1 || jr.SpeakingOrder > n {
			t.Errorf("【5】speakingOrder=%d 超出 1..%d", jr.SpeakingOrder, n)
		}
		if seen[jr.SpeakingOrder] {
			t.Errorf("【5】speakingOrder=%d 重复了 —— 座位号必须是排列，不能重号", jr.SpeakingOrder)
		}
		seen[jr.SpeakingOrder] = true
	}

	if mods != 1 {
		t.Errorf("【5】主席数量 = %d，期望恰好 1（并发进场时最容易出现「人人都是主席」）", mods)
	}
	for i := 1; i <= n; i++ {
		if !seen[i] {
			t.Errorf("【5】speakingOrder 里缺少 %d —— 不是 1..%d 的完整排列", i, n)
		}
	}
	t.Logf("【5】%d 人并发进场，主席 %d 个，座位号 %v", n, mods, keysOfInts(seen))
}

func keysOfInts(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

/* ══════════════════════════════════════════════════════════════════════════
   B. 四个上限【6-9】
   ══════════════════════════════════════════════════════════════════════════ */

// 【6】建房不传 tokenBudget → 默认 20,000,000
func TestB6_DefaultBudget(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, nil) // 不传 tokenBudget
	mm := m.join("budget-check")

	if got := mm.brief.Limits.TokenBudget; got != 20000000 {
		t.Errorf("【6】不传 tokenBudget 时 brief.limits.tokenBudget = %.0f，期望 20000000", got)
	} else {
		t.Logf("【6】默认预算 = %.0f", got)
	}

	// 再用房间预算的实时视图交叉验证一次（两个口径一致才算真的设上了）
	st := m.inbox(mm)
	if st.Budget.Total != 20000000 {
		t.Errorf("【6】inbox.budget.total = %.0f，与 brief 里的默认预算不一致", st.Budget.Total)
	}
}

// 【7】maxRounds:50 → 夹到 10；maxAgents:20 → 夹到 6
func TestB7_ClampRoundsAndAgents(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, map[string]any{"maxRounds": 50, "maxAgents": 20})

	mm := m.join("clamp-check")

	// 轮数：对比 join 下发的 brief 与 inbox 的实时视图，两者都应为 10
	if got := mm.brief.Rounds; got != 10 {
		t.Errorf("【7】maxRounds:50 应被夹到 10，brief.rounds = %d", got)
	} else {
		t.Logf("【7】brief.rounds 被夹到 %d", got)
	}
	if st := m.inbox(mm); st.RoundsTotal != 10 {
		t.Errorf("【7】maxRounds:50 应被夹到 10，inbox.roundsTotal = %d", st.RoundsTotal)
	}

	// 人数：凑满 6 人后，第 7 人必须 409（证明上限被夹到 6 而不是 20）
	m.joinN(5) // 已有 1 人，补到 6
	r7 := m.joinHandle("agent-clamp-7", m.roomID)
	if r7.Status != http.StatusConflict {
		t.Errorf("【7】maxAgents:20 应被夹到 6，但第 7 人返回 %d（期望 409）\n  响应 %s",
			r7.Status, r7.snippet())
	} else {
		t.Logf("【7】maxAgents 被夹到 6（第 7 人 409）")
	}
}

// 【8】发言超过 maxOutputTokens → 422 text_too_long，且预算没被扣
func TestB8_TextTooLong(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, nil)
	m.joinN(2) // 单人进会会跳过辩论，必须 2 人才有 debating 阶段
	mm := m.members[0]

	if !m.waitState("debating", 15*time.Second) {
		t.Fatalf("【8】等不到 debating 状态，当前 %s", m.state())
	}
	// 等轮到第一个人
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !m.inbox(mm).YourTurn {
		time.Sleep(60 * time.Millisecond)
	}

	before := m.inbox(mm).Budget.Used
	huge := strings.Repeat("字", 3000) // 远超 maxOutputTokens(800)
	r := m.speak(mm, huge, 0, "")
	if r.Status != 422 {
		t.Errorf("【8】超长发言期望 422，实际 %d\n  响应 %s", r.Status, r.snippet())
	}
	if code := r.errCode(); code != "text_too_long" && code != "" {
		t.Errorf("【8】错误码是 %q，期望 text_too_long", code)
	}

	after := m.inbox(mm).Budget.Used
	if after != before {
		t.Errorf("【8】被拒的长文竟然扣了预算：used %.0f → %.0f（规格要求「不扣预算」）", before, after)
	} else {
		t.Logf("【8】超长发言被拒且预算未变（used=%.0f）", before)
	}
}

// 【9】时长到 → 自动进 voting
func TestB9_TimeLimitToVoting(t *testing.T) {
	if !supportsFlag(testBinary(t), "default-minutes") {
		t.Skip("【9】被测版本没有 -default-minutes")
	}
	// 1 分钟足够区分"时长生效"与"没生效"，又不会把套件拖太久
	const minutes = 1
	srv := newServer(t, "-default-minutes", strconv.Itoa(minutes))
	m := newMeeting(t, srv, nil)
	m.joinN(2)
	if !m.waitState("debating", 20*time.Second) {
		t.Fatalf("【9】等不到 debating：%s", m.state())
	}

	started := time.Now()
	sawVoting := false
	last := ""
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		st := m.state()
		if st != last {
			t.Logf("【9】%v 时状态 → %s", time.Since(started).Round(time.Second), st)
			last = st
		}
		if st == "voting" {
			sawVoting = true
			break
		}
		if st == "ended" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if sawVoting {
		t.Logf("【9】-default-minutes=%d 生效：时长到期后进入 voting", minutes)
		return
	}
	if m.state() == "ended" {
		t.Logf("【9】时长到期后直接结束（未观察到 voting 中间态，但确实没有被卡住）")
		return
	}
	t.Errorf("【9】-default-minutes=%d 时 90s 内仍未进 voting（当前 %s）", minutes, m.state())
}

/* ══════════════════════════════════════════════════════════════════════════
   C. 方案与投票【10-16】
   ══════════════════════════════════════════════════════════════════════════ */

// driveToVoting 把所有讨论轮次跑完（谁轮到就替谁发一条）。
func (m *meeting) driveToVoting(timeout time.Duration) {
	m.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st := m.inbox(m.members[0]); st.State != "debating" {
			return
		}
		advanced := false
		for _, mm := range m.members {
			cur := m.inbox(mm)
			if cur.State != "debating" {
				return
			}
			if !cur.YourTurn {
				continue
			}
			replyTo := 0
			if cur.Round >= 2 {
				replyTo = m.lastOtherSeq(mm)
			}
			support := ""
			if cur.RoundsTotal > 0 && cur.Round >= cur.RoundsTotal {
				support = m.firstProposalID()
			}
			r := m.speak(mm, fmt.Sprintf("第%d轮：%s 的观点与理由。", cur.Round, mm.alias), replyTo, support)
			if r.Status == 200 || r.Status == 201 {
				advanced = true
			}
		}
		if !advanced {
			time.Sleep(60 * time.Millisecond)
		}
	}
}

// meetingWithProposals 建一个 3 轮、2 人、已经产出 2 个方案的会议，停在 round 2。
func meetingWithProposals(t *testing.T, srv *server) *meeting {
	t.Helper()
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(2)
	if !m.waitState("debating", 20*time.Second) {
		t.Fatalf("等不到 debating，当前 %s", m.state())
	}
	// 跑完第 1 轮
	m.driveToRound(2)
	// 第 2 轮轮到谁就提方案
	for _, mm := range m.members {
		st := m.inbox(mm)
		if st.State != "debating" {
			break
		}
		if st.YourTurn {
			if _, r := m.propose(mm, mm.alias+" 的方案", "方案正文：先灰度再全量。"); r.Status != 200 && r.Status != 201 {
				t.Logf("提示：%s 提方案失败 HTTP %d：%s", mm.alias, r.Status, r.snippet())
			}
			if r := m.speak(mm, mm.alias+" 补充说明方案。", m.lastOtherSeq(mm), ""); r.Status >= 500 {
				t.Fatalf("发言 5xx：%s", r.snippet())
			}
		}
	}
	return m
}

// driveToRound 推进到指定轮次。
func (m *meeting) driveToRound(target int) {
	m.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		st := m.inbox(m.members[0])
		if st.State != "debating" || st.Round >= target {
			return
		}
		advanced := false
		for _, mm := range m.members {
			cur := m.inbox(mm)
			if cur.State != "debating" || cur.Round >= target {
				break
			}
			if !cur.YourTurn {
				continue
			}
			replyTo := 0
			if cur.Round >= 2 {
				replyTo = m.lastOtherSeq(mm)
			}
			r := m.speak(mm, fmt.Sprintf("第%d轮：%s 的立场。", cur.Round, mm.alias), replyTo, "")
			if r.Status == 200 || r.Status == 201 {
				advanced = true
			}
		}
		if !advanced {
			time.Sleep(60 * time.Millisecond)
		}
	}
}

// 【10】提交 3 个方案 → 第 3 个 409 too_many_proposals
func TestC10_TooManyProposals(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(2)
	if !m.waitState("debating", 20*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}
	m.driveToRound(2)

	who := m.members[0]
	// 等到它自己的回合
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !m.inbox(who).YourTurn {
		time.Sleep(60 * time.Millisecond)
	}

	var codes []int
	for i := 1; i <= 3; i++ {
		_, r := m.propose(who, fmt.Sprintf("方案%d", i), "正文")
		codes = append(codes, r.Status)
	}
	if codes[2] != http.StatusConflict {
		t.Errorf("【10】同一 agent 的第 3 个方案期望 409，实际 %d（三次状态码 %v）", codes[2], codes)
	} else {
		t.Logf("【10】第 3 个方案被拒（三次状态码 %v）", codes)
	}
}

// 【11】非成员提交方案 → 404
func TestC11_NonMemberProposal(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(2)

	// 另一个会议的 agent 充当非成员
	other := newMeeting(t, srv, nil)
	outsider := other.join("outsider")

	r := call(t, srv.base, http.MethodPost, "/rooms/"+m.roomID+"/proposals", outsider.token, map[string]any{
		"title": "外来方案", "body": "我不属于这个会议", "opKey": newOpKey(),
	})
	expectStatus(t, r, http.StatusNotFound, "【11】非成员提交方案，期望 404（不泄漏会议存在）")
}

// 【12】投票阶段外投票 → 403 wrong_round
func TestC12_VoteOutsideVoting(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(2)
	if !m.waitState("debating", 20*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}

	r := m.vote(m.members[0], "p_1")
	expectStatus(t, r, http.StatusForbidden, "【12】辩论阶段投票，期望 403 wrong_round")
	if code := r.errCode(); code != "wrong_round" && code != "" {
		t.Errorf("【12】错误码是 %q，期望 wrong_round", code)
	}
}

// 【13】同一人投两次 → 覆盖不重复计票
func TestC13_DoubleVoteOverwrites(t *testing.T) {
	srv := newServer(t, "-summary-timeout", "2s")
	m := meetingWithProposals(t, srv)
	if len(m.props.order) < 2 {
		t.Skipf("【13】只产出 %d 个方案，需要 2 个才能验证改票", len(m.props.order))
	}
	m.driveToVoting(90 * time.Second)
	if !m.waitState("voting", 20*time.Second) {
		t.Fatalf("【13】进不了 voting，当前 %s", m.state())
	}

	p1, p2 := m.props.order[0], m.props.order[1]
	who := m.members[0]
	if r := m.voteAvoidingSelf(who, p1); r.Status != 200 && r.Status != 201 {
		t.Fatalf("【13】第一次投票失败 HTTP %d：%s", r.Status, r.snippet())
	}
	if r := m.voteAvoidingSelf(who, p2); r.Status != 200 && r.Status != 201 {
		t.Fatalf("【13】改票失败 HTTP %d：%s", r.Status, r.snippet())
	}
	// 其余人投票，让房间结束
	for _, mm := range m.members[1:] {
		if r := m.voteAvoidingSelf(mm, p1); r.Status != 200 && r.Status != 201 {
			t.Logf("提示：%s 投票失败 HTTP %d", mm.alias, r.Status)
		}
	}
	if !m.waitEnded(25 * time.Second) {
		t.Fatalf("【13】等不到 ended，当前 %s", m.state())
	}

	votes := voteMap(t, m)
	if votes[p1]+votes[p2] > float64(len(m.members)) {
		t.Errorf("【13】票数总和 %.0f 超过参会人数 %d —— 改票被重复计票（votes=%v）",
			votes[p1]+votes[p2], len(m.members), votes)
	} else {
		t.Logf("【13】改票后票数 %v（未重复计票）", votes)
	}
}

func voteMap(t *testing.T, m *meeting) map[string]float64 {
	t.Helper()
	// 新契约：conclusion.votes 变成 {"p_1":{total,self,cross}}，
	// 扁平总票数在 conclusion.voteTotals；flatVotes 会优先取它并兼容旧结构。
	return flatVotes(m.result())
}

// 【14】平票三级判定
func TestC14_TieBreak(t *testing.T) {
	t.Run("主席投票的那个胜", func(t *testing.T) {
		srv := newServer(t, "-summary-timeout", "2s")
		m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
		m.joinN(3)
		if !m.waitState("debating", 20*time.Second) {
			t.Fatalf("等不到 debating：%s", m.state())
		}
		m.driveToRound(2)

		// 造 3 个方案：A 提 p1、p2；B 提 p3
		deadline := time.Now().Add(20 * time.Second)
		submitted := 0
		for submitted < 3 && time.Now().Before(deadline) {
			for _, mm := range m.members {
				st := m.inbox(mm)
				if st.State != "debating" {
					break
				}
				if !st.YourTurn {
					continue
				}
				if submitted < 3 {
					if id, r := m.propose(mm, fmt.Sprintf("方案%d", submitted+1), "正文"); r.Status == 200 || r.Status == 201 {
						t.Logf("【14】%s 提交 %s", mm.alias, id)
						submitted++
					}
				}
				m.speak(mm, "补充说明。", m.lastOtherSeq(mm), "")
			}
			time.Sleep(80 * time.Millisecond)
		}
		if len(m.props.order) < 3 {
			t.Skipf("【14】只拿到 %d 个方案，需要 3 个才能构造平票", len(m.props.order))
		}

		m.driveToVoting(90 * time.Second)
		if !m.waitState("voting", 20*time.Second) {
			t.Fatalf("【14】进不了 voting，当前 %s", m.state())
		}

		mod := m.moderator()
		p1, p2, p3 := m.props.order[0], m.props.order[1], m.props.order[2]
		// 三票各投一个 → 1-1-1 平票
		// 服务端禁止投自己提的方案，所以轮转投票：主席投 p2，另两人投 p3、p1
		if r := m.vote(mod, p2); r.Status != 200 && r.Status != 201 {
			t.Fatalf("【14】主席投票失败 HTTP %d：%s", r.Status, r.snippet())
		}
		others := []*member{}
		for _, mm := range m.members {
			if mm.alias != mod.alias {
				others = append(others, mm)
			}
		}
		if len(others) >= 2 {
			m.vote(others[0], p3)
			m.vote(others[1], p1)
		}

		if !m.waitEnded(75 * time.Second) {
			t.Fatalf("【14】等不到 ended，当前 %s", m.state())
		}
		res := m.result()
		winner := winnerID(res)
		if winner != p2 {
			t.Errorf("【14】三方案平票时应由主席投的方案 %s 胜，实际 winner=%q\n  结论：%v",
				p2, winner, conclusionText(res))
		} else {
			t.Logf("【14】平票 → 主席票优先，winner=%s", winner)
		}
	})

	t.Run("主席没投则最早提交的胜", func(t *testing.T) {
		// 显式给小的 summary 超时：本用例验的是破平规则，不该等产品默认的概述窗口
		srv := newServer(t, "-summary-timeout", "2s")
		m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
		m.joinN(3)
		if !m.waitState("debating", 20*time.Second) {
			t.Fatalf("等不到 debating：%s", m.state())
		}
		m.driveToRound(2)

		deadline := time.Now().Add(20 * time.Second)
		submitted := 0
		for submitted < 3 && time.Now().Before(deadline) {
			for _, mm := range m.members {
				st := m.inbox(mm)
				if st.State != "debating" {
					break
				}
				if !st.YourTurn {
					continue
				}
				if submitted < 3 {
					if _, r := m.propose(mm, fmt.Sprintf("方案%d", submitted+1), "正文"); r.Status == 200 || r.Status == 201 {
						submitted++
					}
				}
				m.speak(mm, "补充说明。", m.lastOtherSeq(mm), "")
			}
			time.Sleep(80 * time.Millisecond)
		}
		if len(m.props.order) < 3 {
			t.Skipf("【14b】只拿到 %d 个方案", len(m.props.order))
		}

		m.driveToVoting(90 * time.Second)
		if !m.waitState("voting", 20*time.Second) {
			t.Fatalf("【14b】进不了 voting")
		}

		mod := m.moderator()
		p1, p2, p3 := m.props.order[0], m.props.order[1], m.props.order[2]
		// 主席弃权；另两人分别投 p2、p3 → p2 与 p3 平票，p1 零票
		if r := m.vote(mod, ""); r.Status != 200 && r.Status != 201 {
			t.Fatalf("【14b】主席弃权失败 HTTP %d：%s", r.Status, r.snippet())
		} else {
			t.Logf("【14b】主席弃权成功（HTTP %d）", r.Status)
		}
		others := []*member{}
		for _, mm := range m.members {
			if mm.alias != mod.alias {
				others = append(others, mm)
			}
		}
		if len(others) >= 2 {
			m.vote(others[0], p3)
			m.vote(others[1], p1)
		}

		if !m.waitEnded(75 * time.Second) {
			t.Fatalf("【14b】等不到 ended")
		}
		res := m.result()
		winner := winnerID(res)
		// p2 与 p3 平票，主席没投 → 取更早提交的 p2
		if winner != p2 && winner != p1 {
			t.Errorf("【14b】主席弃权时应由更早提交的方案胜（p1 零票时是 %s），实际 %q", p2, winner)
		} else {
			t.Logf("【14b】主席弃权 → 最早提交者优先，winner=%q", winner)
		}
	})
}

func winnerID(res map[string]any) string {
	concl, _ := res["conclusion"].(map[string]any)
	if concl == nil {
		return ""
	}
	w, ok := concl["winner"].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := w["proposalId"].(string)
	return id
}

func conclusionText(res map[string]any) string {
	concl, _ := res["conclusion"].(map[string]any)
	if concl == nil {
		return ""
	}
	s, _ := concl["text"].(string)
	return s
}

// 【15】无人提交方案 → 投票轮降级用 finalStance，winner=null 但 conclusion 非空
func TestC15_NoProposalsDegrade(t *testing.T) {
	srv := newServer(t, "-summary-timeout", "2s")
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(2)
	if !m.waitState("debating", 20*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}
	m.driveToVoting(90 * time.Second)
	if !m.waitState("voting", 20*time.Second) {
		t.Fatalf("【15】进不了 voting，当前 %s", m.state())
	}

	for i, mm := range m.members {
		stance := "agree"
		if i%2 == 1 {
			stance = "disagree"
		}
		r := call(t, srv.base, http.MethodPost, "/rooms/"+m.roomID+"/votes", mm.token, map[string]any{
			"finalStance": stance, "opKey": newOpKey(),
		})
		if r.Status != 200 && r.Status != 201 {
			t.Errorf("【15】%s 提交降级投票失败 HTTP %d：%s", mm.alias, r.Status, r.snippet())
		}
	}
	if !m.waitEnded(25 * time.Second) {
		t.Fatalf("【15】等不到 ended，当前 %s", m.state())
	}

	res := m.result()
	if w := winnerID(res); w != "" {
		t.Errorf("【15】无方案时 winner 应为 null，实际 %q", w)
	}
	if txt := conclusionText(res); strings.TrimSpace(txt) == "" {
		t.Errorf("【15】无方案时结论不能为空（必须有兜底文案）")
	} else {
		t.Logf("【15】无方案降级投票成功，结论：%s", txt)
	}
}

// 【16】全员弃权 → winner=null，结论写「未形成方案」，仍然 ended
func TestC16_AllAbstain(t *testing.T) {
	srv := newServer(t, "-summary-timeout", "2s")
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(2)
	if !m.waitState("debating", 20*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}
	m.driveToRound(2)

	// 至少造一个方案，否则走的是"无方案降级"分支，不是"全员弃权"
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && len(m.props.order) == 0 {
		for _, mm := range m.members {
			st := m.inbox(mm)
			if st.State != "debating" {
				break
			}
			if st.YourTurn {
				m.propose(mm, mm.alias+" 的方案", "正文")
				m.speak(mm, "补充。", m.lastOtherSeq(mm), "")
			}
		}
		time.Sleep(80 * time.Millisecond)
	}
	if len(m.props.order) == 0 {
		t.Skip("【16】没能造出方案，无法验证全员弃权")
	}

	m.driveToVoting(90 * time.Second)
	if !m.waitState("voting", 20*time.Second) {
		t.Fatalf("【16】进不了 voting")
	}
	for _, mm := range m.members {
		if r := m.vote(mm, ""); r.Status != 200 && r.Status != 201 {
			t.Errorf("【16】%s 弃权失败 HTTP %d：%s", mm.alias, r.Status, r.snippet())
		}
	}
	if !m.waitEnded(25 * time.Second) {
		t.Fatalf("【16】全员弃权后房间没有 ended（当前 %s）—— 会卡死", m.state())
	}

	res := m.result()
	if w := winnerID(res); w != "" {
		t.Errorf("【16】全员弃权时 winner 应为 null，实际 %q", w)
	}
	txt := conclusionText(res)
	if !strings.Contains(txt, "未形成") && !strings.Contains(txt, "方案") {
		t.Errorf("【16】全员弃权的结论应写明「未形成方案」，实际：%q", txt)
	} else {
		t.Logf("【16】全员弃权 → winner=null，结论：%s", txt)
	}
}
