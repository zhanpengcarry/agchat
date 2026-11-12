package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agchat/internal/config"
	"agchat/internal/room"
	"agchat/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("打开内存库: %v", err)
	}
	eng := room.New(st, time.Hour, 30*time.Second, time.Minute)
	cfg := config.Config{JoinRate: 100, TurnTimeout: time.Hour, Briefing: 30 * time.Second}
	srv := New(eng, st, cfg, LoadDocs("../../nonexistent.md", "../../nonexistent.md"))
	ts := httptest.NewServer(NewRouter(srv))
	t.Cleanup(func() { ts.Close(); _ = st.Close() })
	return ts
}

func post(t *testing.T, ts *httptest.Server, path, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(string(b)))
	req.Header.Set("content-type", "application/json")
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(res.Body).Decode(&m)
	return res, m
}

func get(t *testing.T, ts *httptest.Server, path, token string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer res.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res, []byte(sb.String())
}

// TestHTTPContract 覆盖对外契约的关键形状（联调回归用）。
func TestHTTPContract(t *testing.T) {
	ts := newTestServer(t)

	// ---- 建房间 ----
	res, m := post(t, ts, "/api/rooms", "", map[string]any{
		"topic": "要不要把定价从年付改成月付", "tokenBudget": 200000,
		"maxRounds": 3, "maxAgents": 3, "minutesLimit": 60,
	})
	if res.StatusCode != 201 {
		t.Fatalf("建房应 201，得到 %d", res.StatusCode)
	}
	for _, k := range []string{"roomId", "roomUrl", "password", "adminKey", "tenantId", "estimate"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("建房响应缺字段 %s: %v", k, m)
		}
	}
	est := m["estimate"].(map[string]any)
	for _, k := range []string{"tokens", "low", "high", "worstCase", "basis", "fits"} {
		if _, ok := est[k]; !ok {
			t.Fatalf("estimate 缺字段 %s: %v", k, est)
		}
	}
	roomID := m["roomId"].(string)
	password := m["password"].(string)

	// ---- 空房间也能拿结论（200）----
	res, _ = get(t, ts, "/api/rooms/"+roomID+"/result", "")
	if res.StatusCode != 200 {
		t.Fatalf("空房间 result 应 200，得到 %d", res.StatusCode)
	}

	// ---- 加入 ----
	res, jm := post(t, ts, "/api/rooms/"+roomID+"/join", "", map[string]any{
		"password": password, "alias": "小周 Claude", "model": "claude-sonnet",
	})
	if res.StatusCode != 201 {
		t.Fatalf("join 应 201，得到 %d %v", res.StatusCode, jm)
	}
	brief := jm["brief"].(map[string]any)
	if !strings.HasPrefix(jm["agentToken"].(string), "agc_") {
		t.Fatal("agentToken 应为 agc_ 前缀")
	}
	// ★ 新增：加入后立刻知道去哪读完整规则
	skillURL, _ := brief["skillUrl"].(string)
	if !strings.HasSuffix(skillURL, "/skill.md") {
		t.Fatalf("brief.skillUrl 形状不对: %q", skillURL)
	}

	// ---- skill / install / healthz ----
	for _, p := range []string{"/skill.md", "/install.md"} {
		res, body := get(t, ts, p, "")
		if res.StatusCode != 200 {
			t.Fatalf("%s 应 200，得到 %d", p, res.StatusCode)
		}
		if ct := res.Header.Get("content-type"); !strings.HasPrefix(ct, "text/markdown") {
			t.Fatalf("%s content-type 应为 text/markdown，得到 %s", p, ct)
		}
		if len(body) < 20 {
			t.Fatalf("%s 内容为空", p)
		}
	}
	res, body := get(t, ts, "/healthz", "")
	if res.StatusCode != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("/healthz 应返回 ok:true，得到 %d %s", res.StatusCode, body)
	}

	// ---- 结果页 ----
	res, page := get(t, ts, "/room/"+roomID, "")
	if res.StatusCode != 200 {
		t.Fatalf("结果页应 200，得到 %d", res.StatusCode)
	}
	html := string(page)
	for _, want := range []string{"参会者立场", "讨论时间线", "/skill.md", "agchat 会议"} {
		if !strings.Contains(html, want) {
			t.Fatalf("结果页缺 %q", want)
		}
	}
	if strings.Contains(html, "<link") || strings.Contains(html, `src="http`) {
		t.Fatal("结果页不允许外链")
	}

	// ---- 不存在的房间 ----
	res, body = get(t, ts, "/api/rooms/r_zzzz/result", "")
	if res.StatusCode != 404 {
		t.Fatalf("不存在的房间应 404，得到 %d", res.StatusCode)
	}
	if !strings.Contains(string(body), "not_found") {
		t.Fatalf("404 应回 not_found，得到 %s", body)
	}
	res, _ = get(t, ts, "/room/r_zzzz", "")
	if res.StatusCode != 404 {
		t.Fatalf("不存在的房间页应 404，得到 %d", res.StatusCode)
	}
}

// TestAgentTokenNotValidForOtherRoom 跨房间的 token 必须是 404（不是 403/401）。
func TestAgentTokenNotValidForOtherRoom(t *testing.T) {
	ts := newTestServer(t)

	mk := func(topic string) (string, string) {
		_, m := post(t, ts, "/api/rooms", "", map[string]any{
			"topic": topic, "tokenBudget": 50000, "maxRounds": 2, "maxAgents": 2,
		})
		return m["roomId"].(string), m["password"].(string)
	}
	roomA, passA := mk("房间 A")
	roomB, _ := mk("房间 B")

	_, jm := post(t, ts, "/api/rooms/"+roomA+"/join", "", map[string]any{
		"password": passA, "alias": "甲", "model": "m",
	})
	token := jm["agentToken"].(string)

	res, body := get(t, ts, "/api/rooms/"+roomB+"/inbox", token)
	if res.StatusCode != 404 {
		t.Fatalf("拿 A 的 token 访问 B 应 404，得到 %d %s", res.StatusCode, body)
	}
	// 同一个 token 回到自己房间必须正常
	res, _ = get(t, ts, "/api/rooms/"+roomA+"/inbox", token)
	if res.StatusCode != 200 {
		t.Fatalf("自己的房间应 200，得到 %d", res.StatusCode)
	}
}
