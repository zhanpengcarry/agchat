package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestKeyPointsPrefersDeclaredFinalStanceOverHTTP 端到端复现：收尾轮带 supportProposalId 的
// 表态必须进入立场矩阵，而不是被"最长那条"（第 1 轮）顶掉。
//
// 这条是从一次真实演练里抠出来的：收尾轮明明带了 supportProposalId，结果 positions 里
// 展示的还是第 1 轮那句话。引擎级单测能过、走 HTTP 就不过，所以这里用真实 HTTP 路径锁一遍。
func TestKeyPointsPrefersDeclaredFinalStanceOverHTTP(t *testing.T) {
	ts := newTestServer(t)

	_, room := post(t, ts, "/api/rooms", "", map[string]any{
		"topic": "立场矩阵取样", "maxRounds": 3, "maxAgents": 2, "tokenBudget": 500000,
	})
	key := fmt.Sprintf("%v", room["roomNo"])
	key = strings.ReplaceAll(key, " ", "")
	password := fmt.Sprintf("%v", room["password"])

	join := func(alias string) (string, string) {
		_, j := post(t, ts, "/api/rooms/"+key+"/join", "", map[string]any{
			"password": password, "alias": alias, "model": "m",
		})
		return fmt.Sprintf("%v", j["agentToken"]), fmt.Sprintf("%v", j["agentId"])
	}
	tokA, _ := join("甲")
	tokB, _ := join("乙")

	post(t, ts, "/api/rooms/"+key+"/messages", tokA, map[string]any{
		"text": "我的立场：先做结构化结论，因为它让分歧可测；唤醒依赖别人开放接口，不是我们能决定的事。", "opKey": "h1",
	})
	post(t, ts, "/api/rooms/"+key+"/messages", tokB, map[string]any{"text": "乙的立场。", "opKey": "h2"})

	_, p := post(t, ts, "/api/rooms/"+key+"/proposals", tokA, map[string]any{
		"title": "甲方案", "body": "可执行内容。", "opKey": "h3",
	})
	pid := fmt.Sprintf("%v", p["proposalId"])
	post(t, ts, "/api/rooms/"+key+"/messages", tokA, map[string]any{"text": "回应乙。", "replyTo": 2, "opKey": "h4"})
	post(t, ts, "/api/rooms/"+key+"/messages", tokB, map[string]any{"text": "回应甲。", "replyTo": 1, "opKey": "h5"})

	// 收尾轮：甲明确表态（带 supportProposalId）
	post(t, ts, "/api/rooms/"+key+"/messages", tokA, map[string]any{
		"text": "收尾表态：我支持甲方案，理由是它可证伪。", "supportProposalId": pid, "opKey": "h6",
	})
	post(t, ts, "/api/rooms/"+key+"/messages", tokB, map[string]any{
		"text": "我也支持甲方案。", "supportProposalId": pid, "opKey": "h7",
	})
	post(t, ts, "/api/rooms/"+key+"/votes", tokA, map[string]any{"proposalId": pid, "opKey": "h8"})
	post(t, ts, "/api/rooms/"+key+"/votes", tokB, map[string]any{"proposalId": pid, "opKey": "h9"})

	res, body := get(t, ts, "/api/rooms/"+key+"/result", "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("result 应 200，得到 %d", res.StatusCode)
	}
	var out struct {
		Positions []struct {
			Alias     string   `json:"alias"`
			KeyPoints []string `json:"keyPoints"`
			Summary   string   `json:"summary"`
		} `json:"positions"`
		Timeline []struct {
			Seq   int    `json:"seq"`
			Round int    `json:"round"`
			Alias string `json:"alias"`
		} `json:"timeline"`
		Cost map[string]any `json:"cost"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析 result 失败: %v", err)
	}
	for _, tl := range out.Timeline {
		t.Logf("seq%d round%d %s", tl.Seq, tl.Round, tl.Alias)
	}
	t.Logf("cost=%v", out.Cost)
	for _, p := range out.Positions {
		if p.Alias != "甲" {
			continue
		}
		if len(p.KeyPoints) == 0 || !strings.Contains(p.KeyPoints[0], "收尾表态") {
			t.Fatalf("立场矩阵应取收尾轮的表态，实际 %q（summary=%q）", p.KeyPoints, p.Summary)
		}
		return
	}
	t.Fatal("result 里找不到甲的立场")
}
