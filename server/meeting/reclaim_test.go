package meeting

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// TestReclaimInFullRoom 验证 reclaim（认回原身份）在满员房间里的语义。
//
// 这是「不占新座位」的证明：一个已满 6 人的房间里，原成员认回自己拿新 token，
// 之后第 7 个**新面孔**仍然必须是 room_full —— 如果 reclaim 偷偷开了一个新座位，
// 这条就会红。
//
// 同时验证旧 token 在认回后立即失效（否则等于一个座位两把钥匙）。
func TestReclaimInFullRoom(t *testing.T) {
	srv := newServer(t, "-join-rate", "100") // 本用例一次要打 9 次 join，避开默认 8 次/分钟的限流
	m := newMeeting(t, srv, map[string]any{"maxAgents": 6})

	const n = 6
	m.joinN(n)
	if !m.waitState("debating", 30*time.Second) {
		t.Fatalf("等不到 debating：%s", m.state())
	}
	orig := m.members[0]
	origID := orig.id
	origToken := orig.token

	type reclaimResp struct {
		AgentToken    string `json:"agentToken"`
		AgentID       string `json:"agentId"`
		Reclaimed     bool   `json:"reclaimed"`
		SpeakingOrder int    `json:"speakingOrder"`
	}

	doReclaim := func() (int, reclaimResp, string) {
		r := call(t, srv.base, http.MethodPost, "/rooms/"+m.roomID+"/join", "", map[string]any{
			"password": m.password,
			"alias":    orig.alias,
			"model":    "test-model",
			"reclaim":  true,
		})
		var jr reclaimResp
		_ = json.Unmarshal(r.Body, &jr)
		return r.Status, jr, r.snippet()
	}

	// ① 刚活动过（30s 内）→ 应当拒绝，避免把还活着的 agent 顶掉
	if st, _, body := doReclaim(); st != http.StatusConflict {
		t.Logf("【reclaim】活跃期内认回返回 %d（期望 409 member_active）；响应：%s", st, body)
	} else {
		t.Logf("【reclaim】活跃期内认回被拒（409 member_active），符合预期")
	}

	// ② 等过活跃窗口后再认回
	time.Sleep(32 * time.Second)
	st, jr, body := doReclaim()
	if st != 200 && st != 201 {
		t.Fatalf("【reclaim】静默 32s 后认回失败 HTTP %d：%s", st, body)
	}
	if !jr.Reclaimed {
		t.Errorf("【reclaim】响应里 reclaimed 不是 true：%s", body)
	}
	if jr.AgentID != origID {
		t.Errorf("【reclaim】agentId 变了（%q → %q）—— 认回变成了复制身份，群里的历史会断成两个人",
			origID, jr.AgentID)
	} else {
		t.Logf("【reclaim】认回成功，agentId 不变（%s），座位号 %d", jr.AgentID, jr.SpeakingOrder)
	}

	// ③ 旧 token 立即失效
	if r := call(t, srv.base, http.MethodGet, "/rooms/"+m.roomID+"/inbox?wait=0", origToken, nil); r.Status == 200 {
		t.Errorf("【reclaim】旧 token 认回后仍然可用（HTTP 200）—— 等于一个座位两把钥匙")
	} else {
		t.Logf("【reclaim】旧 token 已失效（HTTP %d）", r.Status)
	}

	// ④ 新 token 可用
	if r := call(t, srv.base, http.MethodGet, "/rooms/"+m.roomID+"/inbox?wait=0", jr.AgentToken, nil); r.Status != 200 {
		t.Errorf("【reclaim】新 token 不可用（HTTP %d）", r.Status)
	}

	// ⑤ 关键：满员房间里第 7 个**新面孔**仍然 room_full（证明 reclaim 没占新座位）
	r7 := m.joinHandle("brand-new-face", m.roomID)
	if r7.errCode() == "join_rate_limited" || r7.Status == http.StatusTooManyRequests {
		t.Fatalf("【reclaim】第 7 次 join 被 join 限流（429）挡住，没测到 room_full —— 这是用例的限流配置问题")
	}
	if r7.Status != http.StatusConflict {
		t.Errorf("【reclaim】认回之后新面孔进场返回 %d（期望 409 room_full）—— "+
			"说明 reclaim 偷偷开了新座位，6 人上限被突破\n  响应 %s", r7.Status, r7.snippet())
	} else {
		t.Logf("【reclaim】认回后新面孔仍被挡（409 room_full），座位数未被突破")
	}
}
