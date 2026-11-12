package meeting

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// 沉默形态要分开测：全沉默只是极端情况，真实场景是「一个人沉默、其他人愿意说话」。
// 这条用例存在的理由：如果实现把"某个 agent 超时"误判成"整轮无人发言"，
// 那么整轮会原地重开，而重开后又是第一个人先 —— 其他人永远轮不到。

// TestMixedSilence 混合沉默：3 人会议，主席全程沉默，另两人正常轮询。
func TestMixedSilence(t *testing.T) {
	srv := newServer(t, "-turn-timeout", "1s")
	m := newMeeting(t, srv, map[string]any{"maxRounds": 10, "maxAgents": 3})
	m.joinN(3)
	if !m.waitState("debating", 30*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}

	// 主席（第一个进场）全程沉默；另外两人是"愿意说话的人"
	mod := m.moderator()
	active := []*member{}
	for _, mm := range m.members {
		if mm.alias != mod.alias {
			active = append(active, mm)
		}
	}
	if len(active) != 2 {
		t.Fatalf("期望 2 个活跃成员，实际 %d", len(active))
	}

	// 记录每个活跃成员在每一轮是否拿到过发言权并发言成功
	spoke := map[string]map[int]bool{}
	for _, mm := range active {
		spoke[mm.alias] = map[int]bool{}
	}

	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		st := m.inbox(active[0])
		if st.State != "debating" {
			break
		}
		for _, mm := range active {
			cur := m.inbox(mm)
			if cur.State != "debating" {
				break
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
			r := m.speak(mm, fmt.Sprintf("第%d轮：%s 的观点。", cur.Round, mm.alias), replyTo, support)
			if r.Status == 200 || r.Status == 201 {
				spoke[mm.alias][cur.Round] = true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	final := m.state()
	// 死亡信号：还没走满轮次就进了 voting
	if final != "voting" && final != "summarizing" && final != "ended" {
		t.Fatalf("【混合沉默】会议卡住了，当前 state=%s", final)
	}

	// ① 两个愿意说话的人，必须在每一轮都拿到过发言权
	for _, mm := range active {
		missing := []int{}
		for r := 1; r <= 10; r++ {
			if !spoke[mm.alias][r] {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			t.Errorf("【混合沉默】%s 在这些轮次没拿到发言权：%v —— "+
				"一个 agent 沉默把整轮判成「无人发言」、原地重开，导致其他人永远轮不到",
				mm.alias, missing)
		}
	}

	// ② 会议必须走满 maxRounds，而不是被提前打进 voting
	res := m.result()
	cost, _ := res["cost"].(map[string]any)
	rounds, _ := cost["rounds"].(float64)
	msgs, _ := cost["messages"].(float64)
	t.Logf("【混合沉默】最终 state=%s，rounds=%.0f，messages=%.0f（2 人 × 10 轮理论值 20）", final, rounds, msgs)

	if int(rounds) < 10 {
		t.Errorf("【混合沉默】只跑到 %.0f 轮就结束了（期望 10 轮）—— 被提前打进 voting", rounds)
	}
	if int(msgs) < 2*9 {
		t.Errorf("【混合沉默】消息数只有 %.0f 条（2 人 × 10 轮应接近 20）—— 有人被系统性饿死", msgs)
	}
}

// TestAllSilentStillEntersVoting 对照：3 人全沉默，仍应在 3 个空轮后进 voting。
// 修混合沉默时不能把这个原有行为改坏。
func TestAllSilentStillEntersVoting(t *testing.T) {
	srv := newServer(t, "-turn-timeout", "1s")
	m := newMeeting(t, srv, map[string]any{"maxRounds": 10, "maxAgents": 3})
	m.joinN(3)
	if !m.waitState("debating", 30*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}
	if !m.waitState("voting", 40*time.Second) {
		t.Fatalf("【全沉默】3 个空轮后没有进 voting（当前 %s）—— 整场会卡死", m.state())
	}
	t.Logf("【全沉默】3 人全沉默 → 如期进入 voting")
}

// TestHalfwayDropout 半程掉线：3 人跑 3 轮后第 1 人不再响应，后 7 轮仍要能推进并投票出结果。
func TestHalfwayDropout(t *testing.T) {
	srv := newServer(t, "-turn-timeout", "1s", "-summary-timeout", "1s")
	m := newMeeting(t, srv, map[string]any{"maxRounds": 10, "maxAgents": 3})
	m.joinN(3)
	if !m.waitState("debating", 30*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}

	drop := m.members[0]
	alive := m.members[1:]

	// 前 3 轮：全员正常说话
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		st := m.inbox(alive[0])
		if st.State != "debating" || st.Round > 3 {
			break
		}
		for _, mm := range m.members {
			cur := m.inbox(mm)
			if cur.State != "debating" || cur.Round > 3 {
				break
			}
			if !cur.YourTurn {
				continue
			}
			replyTo := 0
			if cur.Round >= 2 {
				replyTo = m.lastOtherSeq(mm)
			}
			m.speak(mm, fmt.Sprintf("第%d轮：%s。", cur.Round, mm.alias), replyTo, "")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 之后第 1 人彻底不响应（模拟掉线），只有后两人继续
	deadline = time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		st := m.inbox(alive[0])
		if st.State != "debating" {
			break
		}
		for _, mm := range alive {
			cur := m.inbox(mm)
			if cur.State != "debating" {
				break
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
			m.speak(mm, fmt.Sprintf("第%d轮：%s 继续。", cur.Round, mm.alias), replyTo, support)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !m.waitState("voting", 30*time.Second) {
		t.Fatalf("【半程掉线】%s 掉线后会议停在 %s，没能进入 voting", drop.alias, m.state())
	}
	// 投票（降级路径，无方案）
	for _, mm := range alive {
		call(t, srv.base, http.MethodPost, "/rooms/"+m.roomID+"/votes", mm.token,
			map[string]any{"finalStance": "agree", "opKey": newOpKey()})
	}
	if !m.waitEnded(80 * time.Second) {
		t.Fatalf("【半程掉线】投票后 80s 仍没 ended（当前 %s）—— 掉线成员的票既没算弃权，也没被投票锁定结算", m.state())
	}
	res := m.result()
	if txt := conclusionText(res); txt == "" {
		t.Errorf("【半程掉线】有人掉线后没能产出结论")
	} else {
		t.Logf("【半程掉线】%s 掉线后仍走完并出结论：%s", drop.alias, firstN(txt, 60))
	}
}

// TestInboxProposalsArrayAndTurnDeadline 校验 inbox 的两个新字段契约。
func TestInboxProposalsArrayAndTurnDeadline(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3, "maxAgents": 2})
	m.joinN(2)
	if !m.waitState("debating", 25*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}

	type inboxNew struct {
		State     string `json:"state"`
		Round     int    `json:"round"`
		YourTurn  bool   `json:"yourTurn"`
		Proposals []struct {
			ProposalID string  `json:"proposalId"`
			Title      string  `json:"title"`
			Body       string  `json:"body"`
			By         string  `json:"by"`
			Votes      float64 `json:"votes"`
		} `json:"proposals"`
		YourTurnDeadlineInSec *float64 `json:"yourTurnDeadlineInSec"`
	}
	read := func(mm *member) inboxNew {
		r := call(t, srv.base, http.MethodGet, "/rooms/"+m.roomID+"/inbox?wait=0", mm.token, nil)
		if r.Status != 200 {
			t.Fatalf("读 inbox 失败 HTTP %d", r.Status)
		}
		var out inboxNew
		if err := json.Unmarshal(r.Body, &out); err != nil {
			t.Fatalf("inbox 不是 JSON：%v", err)
		}
		return out
	}

	// 空房间：proposals 必须是 []，不能是 0
	st := read(m.members[0])
	if st.Proposals == nil {
		t.Errorf("【契约】inbox.proposals 在空房间应为 []，实际是 null（类型契约被破坏）")
	}
	t.Logf("【契约】空房间 proposals 长度 = %d（是数组）", len(st.Proposals))

	// 回合倒计时：轮到我时是正数，不是我时是 null
	var holder *member
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && holder == nil {
		for _, mm := range m.members {
			if read(mm).YourTurn {
				holder = mm
				break
			}
		}
		time.Sleep(60 * time.Millisecond)
	}
	if holder == nil {
		t.Fatalf("【契约】等不到任何人拿到发言权")
	}
	hs := read(holder)
	if hs.YourTurnDeadlineInSec == nil {
		t.Errorf("【契约】轮到我时 yourTurnDeadlineInSec 是 null，期望正数")
	} else if *hs.YourTurnDeadlineInSec <= 0 {
		t.Errorf("【契约】轮到我时 yourTurnDeadlineInSec = %.1f，期望正数", *hs.YourTurnDeadlineInSec)
	} else {
		t.Logf("【契约】轮到我时 yourTurnDeadlineInSec = %.1fs", *hs.YourTurnDeadlineInSec)
	}
	for _, mm := range m.members {
		if mm.alias == holder.alias {
			continue
		}
		if other := read(mm); other.YourTurnDeadlineInSec != nil {
			t.Errorf("【契约】不是我时 yourTurnDeadlineInSec 应为 null，实际 %.1f", *other.YourTurnDeadlineInSec)
		}
	}

	// 提交方案后，proposals 必须是带字段的数组
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && len(m.props.order) == 0 {
		for _, mm := range m.members {
			cur := read(mm)
			if cur.State != "debating" {
				break
			}
			if cur.YourTurn && cur.Round >= 2 && cur.Round < 10 {
				if _, r := m.propose(mm, "并行一个月", "新旧并行一个月。"); r.Status == 200 || r.Status == 201 {
					break
				}
			}
			if cur.YourTurn {
				m.speak(mm, "推进一轮。", m.lastOtherSeq(mm), "")
			}
		}
		time.Sleep(60 * time.Millisecond)
	}
	if len(m.props.order) == 0 {
		t.Skip("【契约】没能造出方案，跳过 proposals 数组内容校验")
	}
	st = read(m.members[0])
	if len(st.Proposals) == 0 {
		t.Fatalf("【契约】有方案后 inbox.proposals 仍为空")
	}
	p := st.Proposals[0]
	if p.ProposalID == "" || p.Title == "" || p.By == "" {
		t.Errorf("【契约】proposals 元素缺字段：%+v", p)
	}
	t.Logf("【契约】proposals[0] = {id=%s title=%q by=%q votes=%.0f}", p.ProposalID, p.Title, p.By, p.Votes)
}
