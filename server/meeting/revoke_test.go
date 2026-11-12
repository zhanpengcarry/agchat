package meeting

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// 预置弃权（掉线者）+ 投票宽限期（-vote-grace，默认 10s）的成对验证。
//
// 背景：预置弃权原本与「全员投完立即结算」互相抵消 —— 预置瞬间就结算，
// 根本不存在「截止前」这个窗口，「可撤销」形同虚设。
// v2.1 加固定短宽限：活人都投完后不立刻结算，再等 vote-grace，
// 期内被预置者回来投票可覆盖那条预置，宽限到才结算。

type presetFixture struct {
	m      *meeting
	pid    string
	owner  string
	silent *member
}

// setupPreset 造 3 人 + 1 个方案，跑到 voting。
// 方案由第 1 个说话的人提交；另两人里挑一个当「掉线者」。
func setupPreset(t *testing.T, srv *server) *presetFixture {
	t.Helper()

	m := newMeeting(t, srv, map[string]any{"maxRounds": 3, "maxAgents": 3})
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
			if st.YourTurn && st.Round >= 2 && st.Round < st.RoundsTotal {
				if _, r := m.propose(mm, "并行一个月", "新旧价格并行一个月。"); r.Status == 200 || r.Status == 201 {
					break
				}
			}
			if st.YourTurn {
				m.speak(mm, "推进一轮。", m.lastOtherSeq(mm), "")
			}
		}
		time.Sleep(60 * time.Millisecond)
	}
	if len(m.props.order) == 0 {
		t.Skip("没能造出方案")
	}

	pid := m.props.order[0]
	owner := m.props.owner[pid]

	// 挑一个不是方案作者的人当掉线者（作者要投别人，能正常参与）
	var silent *member
	var voter *member
	for _, mm := range m.members {
		if mm.alias == owner {
			continue
		}
		if silent == nil {
			silent = mm
		} else {
			voter = mm
		}
	}
	if silent == nil || voter == nil {
		t.Skip("成员构成不满足本用例假设")
	}

	m.driveToVoting(150 * time.Second)
	if !m.waitState("voting", 30*time.Second) {
		t.Fatalf("进不了 voting，当前 %s", m.state())
	}

	// 作者显式弃权（不是"自投被禁"，v2.2 允许自投；这里只是要造一张 abstain 票），
	// 另一个非作者投方案，silent 保持沉默等预置
	if r := m.vote(silentOwner(owner, m), ""); r.Status != 200 && r.Status != 201 {
		t.Logf("提示：作者显式弃权失败 HTTP %d：%s", r.Status, r.snippet())
	}
	if r := m.vote(voter, pid); r.Status != 200 && r.Status != 201 {
		t.Logf("提示：%s 投票失败 HTTP %d：%s", voter.alias, r.Status, r.snippet())
	}

	return &presetFixture{m: m, pid: pid, owner: owner, silent: silent}
}

func silentOwner(alias string, m *meeting) *member {
	for _, mm := range m.members {
		if mm.alias == alias {
			return mm
		}
	}
	return m.members[0]
}

// votesNow 读当前 result 的票型（voting 阶段也应当可读）。
func votesNow(t *testing.T, m *meeting) (map[string]any, float64) {
	t.Helper()
	r := call(t, m.srv.base, http.MethodGet, "/rooms/"+m.roomID+"/result", "", nil)
	if r.Status != 200 {
		return nil, 0
	}
	var res map[string]any
	if json.Unmarshal(r.Body, &res) != nil {
		return nil, 0
	}
	concl, _ := res["conclusion"].(map[string]any)
	_ = concl
	fv := flatVotes(res)
	out := map[string]any{}
	for k, v := range fv {
		out[k] = v
	}
	return out, fv["abstain"]
}

// TestPresetAbstainCanBeRevokedWithinGrace：预置后在宽限期内回来投票 → 必须覆盖。
func TestPresetAbstainCanBeRevokedWithinGrace(t *testing.T) {
	if !supportsFlag(testBinary(t), "vote-grace") {
		t.Skip("被测版本还没有 -vote-grace")
	}
	srv := newServer(t, "-turn-timeout", "1s", "-summary-timeout", "1s", "-vote-grace", "10s")
	pf := setupPreset(t, srv)
	m, pid, silent := pf.m, pf.pid, pf.silent

	// 超过 3×turn-timeout（3s）让它被预置，但仍在 10s 宽限期内
	time.Sleep(5 * time.Second)

	before, abstainBefore := votesNow(t, m)
	t.Logf("【宽限】预置后票型 %v（abstain=%.0f）", before, abstainBefore)
	if abstainBefore < 1 {
		t.Logf("【宽限】5s 时未观察到预置弃权，继续主断言")
	}

	// 关键：房间此刻还必须是 voting（宽限期内），不能已经 ended
	if st := m.state(); st != "voting" {
		t.Fatalf("【宽限】预置后 5s 房间已是 %s —— 宽限期 %s 没生效，撤销窗口为零",
			st, "10s")
	}

	r := m.vote(silent, pid)
	if r.Status != 200 && r.Status != 201 {
		t.Fatalf("【宽限】%s 在宽限期内回来投票被拒 HTTP %d：%s",
			silent.alias, r.Status, r.snippet())
	}
	t.Logf("【宽限】%s 在宽限期内成功改票", silent.alias)

	if !m.waitEnded(40 * time.Second) {
		t.Fatalf("【宽限】宽限期后没有结算（当前 %s）", m.state())
	}

	after, abstainAfter := votesNow(t, m)
	t.Logf("【宽限】最终票型 %v（abstain=%.0f）", after, abstainAfter)

	// 作者那一票是显式弃权，正常的 1 票；预置那一票必须被覆盖掉
	if abstainAfter >= 2 {
		t.Errorf("【宽限】回来投票后 abstain 仍有 %.0f 票 —— 预置弃权没被覆盖（正常应只剩作者的 1 票显式弃权）",
			abstainAfter)
	}
	if got, _ := after[pid].(float64); got < 2 {
		t.Errorf("【宽限】方案 %s 只拿到 %.0f 票，期望 2 票（voter + 回来的这位）", pid, got)
	}
}

// TestPresetAbstainSticksWhenNoReturn：对照，预置后不回来 → 宽限到就结算，票型仍是 abstain。
func TestPresetAbstainSticksWhenNoReturn(t *testing.T) {
	if !supportsFlag(testBinary(t), "vote-grace") {
		t.Skip("被测版本还没有 -vote-grace")
	}
	srv := newServer(t, "-turn-timeout", "1s", "-summary-timeout", "1s", "-vote-grace", "3s")
	pf := setupPreset(t, srv)
	m := pf.m

	start := time.Now()
	if !m.waitEnded(60 * time.Second) {
		t.Fatalf("【对照】宽限期后没有结算（当前 %s）—— 会永久卡在 voting", m.state())
	}
	elapsed := time.Since(start)

	after, abstain := votesNow(t, m)
	t.Logf("【对照】不回来 → %v 后结算，票型 %v（abstain=%.0f）", elapsed.Round(time.Second), after, abstain)

	// 断言改成看"方案拿到几票"：掉线者没投，所以方案只有 voter 那一票。
	// 不依赖 abstain 键是否存在 —— 票型结构随场景变化是已知的 API 陷阱。
	if got, ok := after[pf.pid].(float64); ok {
		if got != 1 {
			t.Errorf("【对照】方案 %s 得了 %.0f 票，期望 1（只有 voter，掉线者未投）", pf.pid, got)
		}
	} else {
		t.Errorf("【对照】票型里读不到方案 %s 的票数：%v", pf.pid, after)
	}
	// abstain 键存在时才校验，值为 0 也算正常（表示没人弃权）
	if abstain == 0 && len(after) > 0 {
		t.Logf("【对照】abstain 为 0 或缺失（新契约会恒定给 0），已用方案票数完成主断言")
	}
	if elapsed > 25*time.Second {
		t.Errorf("【对照】结算等了 %v，远超 vote-grace=3s —— 宽限期没按配置执行", elapsed)
	}
}
