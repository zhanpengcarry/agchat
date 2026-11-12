package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"agchat/internal/config"
	"agchat/internal/room"
	"agchat/internal/store"
)

func newServerWith(t *testing.T, cfg config.Config) *httptest.Server {
	ts, _ := newServerWithHandle(t, cfg)
	return ts
}

func newServerWithHandle(t *testing.T, cfg config.Config) (*httptest.Server, *Server) {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("打开内存库: %v", err)
	}
	eng := room.New(st, time.Hour, 30*time.Second, time.Minute)
	srv := New(eng, st, cfg, LoadDocs("../../nonexistent.md", "../../nonexistent.md"))
	ts := httptest.NewServer(NewRouter(srv))
	t.Cleanup(func() { ts.Close(); _ = st.Close() })
	return ts, srv
}

func silentLogs(t *testing.T) {
	t.Helper()
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
}

// setupRoom 建一个房间，让两个 agent 加入并各发一条（第 1 轮，不需要 replyTo），
// 返回第一个 agent 的 token —— 拿它去打别的房间就是"存在但无权"。
func setupRoom(t *testing.T, ts *httptest.Server, topic string) (roomID, password, token, tenantID string) {
	t.Helper()
	_, m := post(t, ts, "/api/rooms", "", map[string]any{
		"topic": topic, "tokenBudget": 200000, "maxRounds": 3, "maxAgents": 2, "minutesLimit": 60,
	})
	roomID = m["roomId"].(string)
	password = m["password"].(string)
	tenantID, _ = m["tenantId"].(string)

	joinOne := func(alias string) string {
		_, jm := post(t, ts, "/api/rooms/"+roomID+"/join", "", map[string]any{
			"password": password, "alias": alias, "model": "m",
		})
		tok, _ := jm["agentToken"].(string)
		if tok == "" {
			t.Fatalf("加入失败: %v", jm)
		}
		return tok
	}
	t1 := joinOne("小周 Claude")
	t2 := joinOne("小李 千问")

	// 第 1 轮：每人一条，不需要 replyTo
	if res, body := post(t, ts, "/api/rooms/"+roomID+"/messages", t1, map[string]any{
		"text": "我认为该改，年付门槛太高。", "opKey": "seed-1",
	}); res.StatusCode != 201 {
		t.Fatalf("预置发言1失败: %d %v", res.StatusCode, body)
	}
	if res, body := post(t, ts, "/api/rooms/"+roomID+"/messages", t2, map[string]any{
		"text": "补充：月付能拿到更多行为样本。", "opKey": "seed-2",
	}); res.StatusCode != 201 {
		t.Fatalf("预置发言2失败: %d %v", res.StatusCode, body)
	}
	return roomID, password, t1, tenantID
}

// sampleInterleaved 交替测量两组（A,B,A,B…）。
//
// 为什么必须交替：如果先把 A 全测完再测 B，任何环境漂移（GC、后台进程、CPU 降频）
// 都会整段压到其中一组身上，制造出"两条路径有差异"的假象。
// 交替采样让漂移平均分配到两边，比较的才是路径本身。
func sampleInterleaved(t *testing.T, client *http.Client, urlA, urlB, token string, n int) (a, b []time.Duration) {
	t.Helper()
	one := func(url string) time.Duration {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("authorization", "Bearer "+token)
		start := time.Now()
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != 404 {
			t.Fatalf("期望 404，得到 %d", res.StatusCode)
		}
		return time.Since(start)
	}
	// 预热连接与语句缓存
	for i := 0; i < 5; i++ {
		one(urlA)
		one(urlB)
	}
	for i := 0; i < n; i++ {
		a = append(a, one(urlA))
		b = append(b, one(urlB))
	}
	sort.Slice(a, func(i, j int) bool { return a[i] < a[j] })
	sort.Slice(b, func(i, j int) bool { return b[i] < b[j] })
	return a, b
}

func median(d []time.Duration) float64 {
	if len(d) == 0 {
		return 0
	}
	return float64(d[len(d)/2].Microseconds())
}

func mean(d []time.Duration) float64 {
	var s float64
	for _, v := range d {
		s += float64(v.Microseconds())
	}
	return s / float64(len(d))
}

// trimmedMean 去掉最慢/最快的 10% 再求均值。
//
// 为什么不用裸均值：本机时钟量化和 GC/调度抖动会偶发个别几十毫秒的离群点，
// 一个离群点就能把 30 个样本的均值拉高 30%+，让这条测试随机变红。
// 离群点是环境噪声，不是"两条路径干了不同的活"—— 用修剪均值仍然能抓到系统性的差异。
func trimmedMean(sorted []time.Duration) float64 {
	if len(sorted) < 10 {
		return mean(sorted)
	}
	cut := len(sorted) / 10
	body := sorted[cut : len(sorted)-cut]
	return mean(body)
}

// TestNoExistenceTimingSideChannel 存在性计时侧信道回归。
//
// 同一个 token，分别打：
//
//	A. 不存在的房间          → 只剩一次索引探测（旧实现）
//	B. 存在但不属于它的房间  → 还要开事务读 rooms/agents/messages
//
// 旧实现两者差 1.5–1.8 倍，足够采样就能判断房间是否存在。
// 现在 A 会补做等价的哑查询（store.DummyScopedLoad），两条路径做同样的功。
func TestNoExistenceTimingSideChannel(t *testing.T) {
	silentLogs(t)
	// 限流开着但阈值很高：让两条路径都付同样的限流开销，同时不会被 429 打断
	ts := newServerWith(t, config.Config{JoinRate: 1000, EnumRate: 100000, TurnTimeout: time.Hour, Briefing: 30 * time.Second})

	_, _, tokenA, _ := setupRoom(t, ts, "房间 A：有内容")
	roomB, _, _, _ := setupRoom(t, ts, "房间 B：另一个租户")

	const missingRoom = "r_zzzz" // 一定不存在
	urlMissing := ts.URL + "/api/rooms/" + missingRoom + "/inbox"
	urlExists := ts.URL + "/api/rooms/" + roomB + "/inbox"

	client := ts.Client()
	const n = 40

	// 重试最多 3 轮：`go test ./...` 会并行跑多个包，本机偶发的调度抖动
	// 足以把某一轮推到阈值之上。真正的回归（旧实现是 1.5–1.8×）在每一轮都会出现，
	// 所以"三轮里有一轮干净"足以说明两条路径等价；三轮都不干净才算回归。
	best := 0.0
	for attempt := 1; attempt <= 3; attempt++ {
		dMissing, dExists := sampleInterleaved(t, client, urlMissing, urlExists, tokenA, n)
		ratioMed := ratio(median(dMissing), median(dExists))
		ratioTrim := ratio(trimmedMean(dMissing), trimmedMean(dExists))
		t.Logf("第 %d 轮: 不存在 median=%.0fµs / 存在无权 median=%.0fµs；比值 median=%.2f trimmed=%.2f",
			attempt, median(dMissing), median(dExists), ratioMed, ratioTrim)

		if best == 0 || ratioMed < best {
			best = ratioMed
		}
		if ratioMed < 1.5 {
			return
		}
	}
	t.Fatalf("三轮中位数比值最小 %.2f，仍 ≥ 1.5 —— 存在性计时侧信道可测", best)
}

func ratio(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		return 1
	}
	if a > b {
		return a / b
	}
	return b / a
}

// TestEnumRateLimit 枚举限流：超过阈值一律 429 rate_limited，
// 而且对"存在的房间"和"不存在的房间"表现一致（否则 429 本身又成了一个 oracle）。
func TestEnumRateLimit(t *testing.T) {
	silentLogs(t)
	// 先用高阈值把场景搭好，再把阈值压到 3 并清零计数 —— 否则建场景自己就会撞上限流
	ts, srv := newServerWithHandle(t, config.Config{JoinRate: 1000, EnumRate: 100000, TurnTimeout: time.Hour, Briefing: 30 * time.Second})
	roomID, _, token, _ := setupRoom(t, ts, "限流房间")
	srv.cfg.EnumRate = 3
	srv.rl = newRateLimiter()

	client := ts.Client()
	hit := func(path string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		if token != "" {
			req.Header.Set("authorization", "Bearer "+token)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	// 前 3 次放行
	for i := 0; i < 3; i++ {
		if code, _ := hit("/api/rooms/" + roomID); code != 200 {
			t.Fatalf("第 %d 次应放行，得到 %d", i+1, code)
		}
	}
	// 第 4 次起 429
	code, body := hit("/api/rooms/" + roomID)
	if code != 429 || !strings.Contains(body, "rate_limited") {
		t.Fatalf("超限应返回 429 rate_limited，得到 %d %s", code, body)
	}
	// ★ /result 不计入枚举限流：参会者轮询进度、管理者取结论都靠它，
	//   同一出口 IP 后面多个 agent 或反复刷新不该被 429 挡住。
	for i := 0; i < 100; i++ {
		if code, body := hit("/api/rooms/" + roomID + "/result"); code != 200 {
			t.Fatalf("/result 第 %d 次被限流了（%d %s）—— 它是已知会议号的读取口，不是枚举探针", i+1, code, body)
		}
	}

	// 对不存在的房间同样 429（不泄漏存在性）
	code2, body2 := hit("/api/rooms/r_zzzz")
	if code2 != 429 || !strings.Contains(body2, "rate_limited") {
		t.Fatalf("不存在的房间也应 429，得到 %d %s", code2, body2)
	}
}

// TestCrossTenantAttemptIsAudited 跨租户尝试必须留下结构化审计日志，
// 且绝不写 token 原文。
func TestCrossTenantAttemptIsAudited(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(old)

	ts := newServerWith(t, config.Config{JoinRate: 1000, EnumRate: 1000, TurnTimeout: time.Hour, Briefing: 30 * time.Second})

	_, _, tokenA, tenantA := setupRoom(t, ts, "租户 A 的房间")
	roomB, _, _, tenantB := setupRoom(t, ts, "租户 B 的房间")
	if tenantA == tenantB {
		t.Fatal("两个房间应该属于不同租户")
	}

	// 拿 A 的 token 打 B 的房间
	res, _ := get(t, ts, "/api/rooms/"+roomB+"/inbox", tokenA)
	if res.StatusCode != 404 {
		t.Fatalf("跨租户应 404，得到 %d", res.StatusCode)
	}

	logs := buf.String()
	if !strings.Contains(logs, "agent_auth_rejected") {
		t.Fatalf("没有写出审计日志: %s", logs)
	}
	if !strings.Contains(logs, "cross_tenant_attempt") {
		t.Fatalf("verdict 应为 cross_tenant_attempt: %s", logs)
	}
	if !strings.Contains(logs, tenantB) {
		t.Fatalf("日志里应有被访问房间的租户 %s", tenantB)
	}
	if !strings.Contains(logs, tenantA) {
		t.Fatalf("日志里应有 token 所属租户 %s", tenantA)
	}
	// 绝不记 token 原文
	if strings.Contains(logs, tokenA) {
		t.Fatalf("审计日志泄漏了 token 原文！")
	}

	// 逐字段校验一行日志
	var rec map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "agent_auth_rejected" {
			rec = m
			break
		}
	}
	if rec == nil {
		t.Fatalf("找不到 agent_auth_rejected 记录: %s", logs)
	}
	for _, k := range []string{"ip", "path", "room_id", "token_hash_prefix", "token_tenant", "room_tenant", "verdict", "time"} {
		if _, ok := rec[k]; !ok {
			t.Fatalf("审计日志缺字段 %q: %v", k, rec)
		}
	}
	prefix, _ := rec["token_hash_prefix"].(string)
	if len(prefix) != 8 {
		t.Fatalf("token_hash_prefix 应为 8 位，得到 %q", prefix)
	}
	if rec["room_exists"] != true {
		t.Fatalf("room_exists 应为 true，得到 %v", rec["room_exists"])
	}
}
