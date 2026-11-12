package meeting

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNew1_PromptHeadcountConsistent 锁住「prompt 人数」这次修复。
//
// 修复前：prompt 里写的是 join 那一刻的实际人数，6 人会议里先到的 agent
// 会看到「共 1 位参会者」，等于给每个 agent 一个错误的世界观。
// 修复后：prompt 用 maxAgents（容量），真实名单改由开场后的 inbox.roster 下发。
func TestNew1_PromptHeadcountConsistent(t *testing.T) {
	srv := newServer(t)
	m := newMeeting(t, srv, map[string]any{"maxAgents": 6})

	const n = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	briefs := []joinBrief{}
	members := []*member{}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r := call(t, srv.base, http.MethodPost, "/rooms/"+m.roomID+"/join", "", map[string]any{
				"password": m.password,
				"alias":    "pea-" + string(rune('a'+idx)),
				"model":    "test-model",
			})
			mu.Lock()
			defer mu.Unlock()
			if r.Status != 200 && r.Status != 201 {
				t.Errorf("【新1】并发进场失败 HTTP %d：%s", r.Status, r.snippet())
				return
			}
			var jr joinResp
			if json.Unmarshal(r.Body, &jr) != nil {
				return
			}
			briefs = append(briefs, jr.Brief)
			members = append(members, &member{
				alias: jr.Brief.Topic, token: jr.AgentToken, id: jr.AgentID,
				order: jr.SpeakingOrder, isModerator: jr.IsModerator, brief: jr.Brief,
			})
		}(i)
	}
	wg.Wait()

	if len(briefs) != n {
		t.Fatalf("【新1】只有 %d/%d 人进场成功", len(briefs), n)
	}

	// ① 6 份 prompt 里的「人数容量」措辞必须完全一致。
	// 注意不能整段比对：prompt 里还有「你是第 N 位进入的 agent」，
	// 那句本来就因人而异，整段比必然不等（我第一版就写错过）。
	headcountRe := regexp.MustCompile("最多\\s*\\d+\\s*位参会者")
	first := headcountRe.FindString(briefs[0].Prompt)
	if first == "" {
		t.Fatalf("【新1】prompt 里找不到「最多 N 位参会者」这类容量描述：\n  %s", firstN(briefs[0].Prompt, 300))
	}
	for i, b := range briefs {
		if got := headcountRe.FindString(b.Prompt); got != first {
			t.Errorf("【新1】第 %d 份 prompt 的人数措辞是 %q，与第一份 %q 不同 —— 人数随入场先后变了",
				i+1, got, first)
		}
	}
	t.Logf("【新1】6 份 prompt 的人数措辞一致：%q", first)
	// ② 开场后 roster 长度一致且等于实际参会数
	if !m.waitState("debating", 30*time.Second) {
		t.Fatalf("【新1】等不到 debating：%s", m.state())
	}
	var lengths []int
	for _, mm := range members {
		st := m.rosterOf(mm)
		lengths = append(lengths, len(st))
	}
	for i, l := range lengths {
		if l != n {
			t.Errorf("【新1】第 %d 位 agent 的 roster 长度 = %d，期望 %d", i+1, l, n)
		}
	}
	t.Logf("【新1】开场后 %d 人的 roster 长度均为 %d", n, lengths[0])
}

// rosterOf 读某人的 inbox.roster。
func (m *meeting) rosterOf(mm *member) []map[string]any {
	r := call(m.t, m.srv.base, http.MethodGet, "/rooms/"+m.roomID+"/inbox?wait=0", mm.token, nil)
	if r.Status != 200 {
		return nil
	}
	var out struct {
		Roster []map[string]any `json:"roster"`
	}
	_ = json.Unmarshal(r.Body, &out)
	return out.Roster
}

// TestNew2_DisagreementExcludesWinner 锁住「分歧文案」这次修复。
//
// 修复前：分歧段会把获胜方案自己说成「另一个方案」，票数也取错。
// 期望票型 {p_1:2, p_2:1} 时，分歧段只能提落选方案 + 它的票数。
func TestNew2_DisagreementExcludesWinner(t *testing.T) {
	srv := newServer(t, "-summary-timeout", "2s")
	m := newMeeting(t, srv, map[string]any{"maxRounds": 3})
	m.joinN(3)
	if !m.waitState("debating", 25*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}
	m.driveToRound(2)

	// 造两个方案：agent-1 提 p_1，agent-2 提 p_2
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) && len(m.props.order) < 2 {
		for _, mm := range m.members {
			st := m.inbox(mm)
			if st.State != "debating" {
				break
			}
			if !st.YourTurn {
				continue
			}
			if len(m.props.order) < 2 {
				m.propose(mm, mm.alias+" 的方案", "方案正文。")
			}
			m.speak(mm, "补充说明。", m.lastOtherSeq(mm), "")
		}
		time.Sleep(60 * time.Millisecond)
	}
	if len(m.props.order) < 2 {
		t.Skipf("只造出 %d 个方案，需要 2 个", len(m.props.order))
	}
	p1, p2 := m.props.order[0], m.props.order[1]
	t1, t2 := m.props.title[p1], m.props.title[p2]

	m.driveToVoting(150 * time.Second)
	if !m.waitState("voting", 30*time.Second) {
		t.Fatalf("进不了 voting，当前 %s", m.state())
	}
	// 票型 p1:2, p2:1
	// 这里刻意让 p1 的作者回避自投（改投 p2），好让「主要分歧」只在 p2 上出现：
	// v2.2 允许自投，但本用例要验的是分歧文案，不是自投形态（自投见 vote_rules_test）。
	owner1 := m.props.owner[p1]
	voted := 0
	for _, mm := range m.members {
		if mm.alias == owner1 {
			m.vote(mm, p2)
		} else if voted < 2 {
			m.vote(mm, p1)
			voted++
		} else {
			m.vote(mm, p2)
		}
	}

	if !m.waitEnded(60 * time.Second) {
		t.Fatalf("等不到 ended，当前 %s", m.state())
	}

	res := m.result()
	if w := winnerID(res); w != p1 {
		t.Fatalf("前置不成立：winner=%q，期望 %q", w, p1)
	}

	// ① ownerReport 的「主要分歧」段不能出现获胜方案标题
	report, _ := res["ownerReport"].(string)
	disagree := extractAfter(report, "主要分歧")
	if strings.TrimSpace(disagree) == "" {
		t.Fatalf("【新2】ownerReport 里找不到「主要分歧」段：%s", firstN(report, 300))
	}
	if strings.Contains(disagree, t1) {
		t.Errorf("【新2】分歧段里出现了获胜方案标题 %q —— 把赢家自己说成了「另一个方案」\n  分歧段：%s",
			t1, firstN(disagree, 200))
	}
	if !strings.Contains(disagree, t2) {
		t.Errorf("【新2】分歧段里没有落选方案标题 %q\n  分歧段：%s", t2, firstN(disagree, 200))
	}
	if !strings.Contains(disagree, "1") {
		t.Errorf("【新2】分歧段里没有落选方案的票数\n  分歧段：%s", firstN(disagree, 200))
	}
	t.Logf("【新2】分歧段：%s", firstN(strings.TrimSpace(disagree), 160))

	// ② summary.sections[h=="分歧"] 同源，也要满足
	summary, _ := res["summary"].(map[string]any)
	if sections, ok := summary["sections"].([]any); ok {
		for _, s := range sections {
			obj, ok := s.(map[string]any)
			if !ok {
				continue
			}
			h, _ := obj["h"].(string)
			if !strings.Contains(h, "分歧") {
				continue
			}
			body, _ := obj["p"].(string)
			if strings.Contains(body, t1) {
				t.Errorf("【新2】summary.sections[分歧] 里出现了获胜方案标题 %q：%s", t1, firstN(body, 200))
			}
		}
	}
}

// extractAfter 取出文本里某个标签之后、同一行剩余的部分。
func extractAfter(text, label string) string {
	i := strings.Index(text, label)
	if i < 0 {
		return ""
	}
	rest := text[i+len(label):]
	rest = strings.TrimLeft(rest, "：: \t")
	if j := strings.IndexAny(rest, "\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}
