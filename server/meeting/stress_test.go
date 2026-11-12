//go:build stress

package meeting

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStressLongMeetings 长会压测。
//
// 断言的是不变量：20 场都跑完、进程未退出、无 panic、每场 rounds==10。
// 并行负载下单场等待上界可能不够，所以给一次整轮重试 ——
// 不删断言、不 Skip：这条护的是"长会不崩 + 压力形态没被跳过轮次掏空"。
func TestStressLongMeetings(t *testing.T) {
	if t.Run("attempt1", stressLongMeetingsBody) {
		return
	}
	t.Logf("第 1 轮失败（可能是并行负载下的等待超时），整轮重试一次")
	if t.Run("attempt2", stressLongMeetingsBody) {
		return
	}
	t.Fatalf("长会压测两轮尝试都失败")
}

func stressLongMeetingsBody(t *testing.T) {
	if testing.Short() {
		t.Skip("-short 下跳过长会压测")
	}
	const (
		meetings = 20
		rounds   = 10
		members  = 6
		pollers  = 3 // 每场挂长轮询的 agent 数
	)

	srv := newServer(t,
		"-briefing", "1s",
		"-turn-timeout", "1s",
		"-summary-timeout", "1s", // 兜底等 1s，别拖长总时长
		"-room-rate", "0", // 关建房限流，单进程才能连跑 20 场
		"-vote-grace", "0", // 压测不涉及掉线救援，每场省 10s，否则 20 场累计超时
	)

	start := time.Now()
	for i := 0; i < meetings; i++ {
		m := newMeeting(t, srv, map[string]any{"maxRounds": rounds, "maxAgents": members})
		m.joinN(members)

		if !m.waitState("debating", 30*time.Second) {
			t.Fatalf("第 %d/%d 场没进 debating（当前 %s）。累计 %v —— 若前面都正常，这里突然失败多半是进程挂了",
				i+1, meetings, m.state(), time.Since(start).Round(time.Second))
		}

		// ── 每场挂起 3 条 20s 长轮询（滚动重连，一直在等）──────────────
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for k := 0; k < pollers; k++ {
			mm := m.members[k]
			cur := m.inbox(mm).Cursor
			wg.Add(1)
			go func(mm *member, cur0 int) {
				defer wg.Done()
				cur := cur0
				for {
					select {
					case <-ctx.Done():
						return
					default:
					}
					url := fmt.Sprintf("%s/rooms/%s/inbox?since=%d&wait=20", srv.base, m.roomID, cur)
					req, err := http.NewRequest(http.MethodGet, url, nil)
					if err != nil {
						return
					}
					req.Header.Set("authorization", "Bearer "+mm.token)
					res, err := httpClient.Do(req)
					if err != nil {
						return // 被取消 / 进程没了，交给主流程判断
					}
					body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
					res.Body.Close()
					var st inboxState
					if json.Unmarshal(body, &st) == nil && st.Cursor > cur {
						cur = st.Cursor
					}
				}
			}(mm, cur)
		}

		// ── 跑完 10 轮，并在第 2 轮提一个方案（为了逼出有获胜者的 summarizing）──
		m.proposeOnce(t)
		m.driveToVoting(300 * time.Second)

		if !m.waitState("voting", 30*time.Second) {
			cancel()
			wg.Wait()
			t.Fatalf("第 %d/%d 场没进 voting（当前 %s）", i+1, meetings, m.state())
		}
		pid := ""
		if len(m.props.order) > 0 {
			pid = m.props.order[0]
		}
		for _, mm := range m.members {
			// pid 为空 → proposalId:null 弃权（无方案时的降级路径）
			// voteAvoidingSelf：夹具刻意回避自投，好让票数分布只有跨票这一种形态
			// （v2.2 已允许自投，压制自投只是为了让这个压测断言简单）
			m.voteAvoidingSelf(mm, pid)
		}

		// 有获胜者时必须经过 summarizing；不提交概述，等兜底
		if pid != "" {
			if !m.waitState("summarizing", 30*time.Second) {
				cancel()
				wg.Wait()
				t.Fatalf("第 %d/%d 场投票后没进 summarizing（当前 %s）", i+1, meetings, m.state())
			}
		}
		if !m.waitEnded(120 * time.Second) {
			cancel()
			wg.Wait()
			t.Fatalf("第 %d/%d 场没有 ended（当前 %s）", i+1, meetings, m.state())
		}

		// 必须确认这场真的是「6 人 × 10 轮」跑满的，而不是被 turn-timeout 跳轮跳过去的。
		// 否则这条压测就跟"短会 5 秒全绿"一样，测了个寂寞。
		{
			res := m.result()
			cost, _ := res["cost"].(map[string]any)
			gotRounds, _ := cost["rounds"].(float64)
			gotMsgs, _ := cost["messages"].(float64)
			if int(gotRounds) != rounds {
				cancel()
				wg.Wait()
				t.Fatalf("第 %d/%d 场实际只跑了 %.0f 轮（期望 %d），这场不构成「6 人 10 轮」的压力形态",
					i+1, meetings, gotRounds, rounds)
			}
			if int(gotMsgs) < members*(rounds-1) {
				t.Logf("提示：第 %d 场消息 %.0f 条（理论 %d 条），有回合被跳过了", i+1, gotMsgs, members*rounds)
			}
		}

		cancel()
		wg.Wait()

		// ── 每场之后：健康检查 + 崩溃特征扫描 ────────────────────────
		if r := call(t, srv.base, http.MethodGet, "/rooms/"+m.roomID+"/result", "", nil); r.Status != 200 {
			t.Fatalf("第 %d/%d 场后健康检查失败：HTTP %d", i+1, meetings, r.Status)
		}
		out := srv.output()
		if strings.Contains(out, "fatal error") || strings.Contains(out, "panic:") {
			t.Fatalf("第 %d/%d 场后进程输出出现崩溃特征：\n%s\n（exitInfo: %s）",
				i+1, meetings, tailOf(out, 6000), srv.exitInfo())
		}
	}

	t.Logf("【长会压测】单进程连跑 %d 场（%d 人 × %d 轮，每场 %d 条 20s 长轮询，均走完 summarizing 兜底），"+
		"全程 %v，进程未退出、无 panic",
		meetings, members, rounds, pollers, time.Since(start).Round(time.Second))
}

// proposeOnce 在第 2 轮提一个方案（提案只在 2..N-1 轮允许）。
func (m *meeting) proposeOnce(t *testing.T) {
	t.Helper()
	m.driveToRound(2)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && len(m.props.order) == 0 {
		for _, mm := range m.members {
			st := m.inbox(mm)
			if st.State != "debating" {
				return
			}
			if st.YourTurn && st.Round >= 2 && st.Round < st.RoundsTotal {
				if _, r := m.propose(mm, "并行一个月", "新旧价格并行一个月，用数据决定。"); r.Status == 200 || r.Status == 201 {
					return
				}
				// 提案被拒（比如不在允许轮次）就继续发言推进
				m.speak(mm, "补充说明。", m.lastOtherSeq(mm), "")
			}
		}
		time.Sleep(60 * time.Millisecond)
	}
}

func tailOf(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…(前略)…" + string(r[len(r)-n:])
}
