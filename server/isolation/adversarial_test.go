// Package isolation —— 第二轮：对抗性攻击用例。
//
// ⚠️ 先记一条口径，避免以后有人拿错靶子当标准：
//
//	mvp/server.mjs 是**旧的 Node 参考实现，不是生产路径**。它在本套件的
//	跨租户用例上返回 401（而不是 404），这本身就是它的问题：401 等于告诉攻击者
//	"这个房间存在，只是你没权限"，而 401 与 404 的差异足以把全平台房间枚举出来。
//	Go 生产版已经按 404 修好并通过。以后不要把 Node 版的绿灯当验收标准。
//
// 本轮假设攻击者已经持有一个合法租户（房间密码 / agent token / adminKey 至少其一），
// 目标是越界到别人的数据、撑爆/拖垮服务、或者绕过计费与投票规则。
//
// 判定原则：
//   - 任何输入都不能产生 5xx（5xx 意味着未处理的分支，通常连带信息泄漏）。
//   - 跨租户一律 404，且不同不存在的 roomId 之间响应不可区分。
//   - 计费与投票这类"业务不变量"被绕过，等同于数据隔离失效（同一房间内的公平性也是隔离）。
package isolation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

/* ══════════════════════════════════════════════════════════════════════════
   本轮新增的小工具
   ══════════════════════════════════════════════════════════════════════════ */

// callH 支持自定义请求头，用来做"头部走私"类攻击。
func callH(t *testing.T, method, path, token string, hdr map[string]string, body any) resp {
	t.Helper()
	r, err := doH(method, path, token, hdr, body, httpTimeout)
	if err != nil {
		t.Fatalf("%s %s 请求失败（服务可能挂了）：%v", method, path, err)
	}
	return r
}

func doH(method, path, token string, hdr map[string]string, body any, timeout time.Duration) (resp, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return resp{}, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, r)
	if err != nil {
		return resp{}, err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		if strings.EqualFold(k, "host") {
			req.Host = v // Host 要单独设，Header.Set("Host") 不生效
			continue
		}
		req.Header.Set(k, v)
	}

	c := &http.Client{Timeout: timeout}
	res, err := c.Do(req)
	if err != nil {
		return resp{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return resp{Status: res.StatusCode, Body: raw, Method: method, Path: path}, nil
}

// doRaw 直接发原始字节，用来构造非法 UTF-8、超大包这类 JSON 编码会"修好"的输入。
func doRaw(method, path string, hdr map[string]string, body []byte, timeout time.Duration) (resp, error) {
	req, err := http.NewRequest(method, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return resp{}, err
	}
	req.Header.Set("content-type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{Timeout: timeout}
	res, err := c.Do(req)
	if err != nil {
		return resp{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return resp{Status: res.StatusCode, Body: raw, Method: method, Path: path}, nil
}

func expectNo5xx(t *testing.T, r resp, what string) {
	t.Helper()
	if r.Status >= 500 {
		t.Errorf("%s\n  出现 5xx（HTTP %d）——未处理的输入分支，通常连带信息泄漏\n  请求 %s %s\n  响应 %s",
			what, r.Status, r.Method, r.Path, r.snippet())
	}
}

type inboxWithBudget struct {
	Cursor   int      `json:"cursor"`
	State    string   `json:"state"`
	Round    int      `json:"round"`
	YourTurn bool     `json:"yourTurn"`
	Unread   int      `json:"unread"`
	Notes    []string `json:"notes"`
	Budget   struct {
		Used  float64 `json:"used"`
		Total float64 `json:"total"`
		Left  float64 `json:"left"`
		Pct   float64 `json:"pct"`
	} `json:"budget"`
}

func inboxB(t *testing.T, tn *tenant) inboxWithBudget {
	t.Helper()
	r := call(t, http.MethodGet, tn.inboxPath()+"?wait=0", tn.agentToken, nil)
	if r.Status != 200 {
		t.Fatalf("读 inbox 失败 HTTP %d：%s", r.Status, r.snippet())
	}
	var in inboxWithBudget
	if err := json.Unmarshal(r.Body, &in); err != nil {
		t.Fatalf("inbox 不是 JSON：%v（%s）", err, r.snippet())
	}
	return in
}

// createRoomOpts 允许自定义预算与轮数（预算类用例需要很小的预算）。
func createRoomOpts(t *testing.T, topic string, maxAgents, maxRounds, tokenBudget int) roomInfo {
	t.Helper()
	info, err := tryCreateRoomOpts(topic, maxAgents, maxRounds, tokenBudget)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return info
}

func tryCreateRoomOpts(topic string, maxAgents, maxRounds, tokenBudget int) (roomInfo, error) {
	var info roomInfo
	r, err := do(http.MethodPost, "/rooms", "", map[string]any{
		"topic":        topic,
		"rules":        "adversarial isolation test",
		"tokenBudget":  tokenBudget,
		"maxRounds":    maxRounds,
		"maxAgents":    maxAgents,
		"minutesLimit": 60,
	})
	if err != nil {
		return info, err
	}
	if r.Status != 200 && r.Status != 201 {
		if r.Status == http.StatusTooManyRequests {
			return info, fmt.Errorf("建房被限流（429 room_rate_limited）：本次测试进程内建的房间已到上限。" +
				"这是**用例数量**问题，不是隔离缺陷")
		}
		return info, fmt.Errorf("建房返回 HTTP %d：%s", r.Status, r.snippet())
	}
	if err := json.Unmarshal(r.Body, &info); err != nil {
		return info, fmt.Errorf("建房响应不是 JSON：%v（%s）", err, r.snippet())
	}
	if info.RoomID == "" || info.Password == "" {
		return info, fmt.Errorf("建房响应缺字段：%s", r.snippet())
	}
	return info, nil
}

// 沙箱租户：注入用例和大包用例共用。
//
// 服务端对建房有速率限制（实测 10 个/进程），所以用例之间要复用房间。
// 这里把 maxRounds 开大，让注入用例连发十几条之后房间还活着，
// 后面的长轮询用例才挂得上去。
var (
	sandboxOnce sync.Once
	sandbox     *tenant
	sandboxErr  error
)

func sandboxTenant(t *testing.T) *tenant {
	t.Helper()
	sandboxOnce.Do(func() {
		topic := newUniqueTopic("sandbox")
		room, err := tryCreateRoomOpts(topic, 1, 60, 200000)
		if err != nil {
			sandboxErr = err
			return
		}
		j, err := tryJoin(room.RoomID, room.Password, "sandbox-agent")
		if err != nil {
			sandboxErr = err
			return
		}
		sandbox = &tenant{
			name: "sandbox", topic: topic, roomID: room.RoomID, password: room.Password,
			adminKey: room.AdminKey, agentToken: j.AgentToken, agentID: j.AgentID, alias: "sandbox-agent",
		}
	})
	if sandboxErr != nil {
		t.Fatalf("准备沙箱租户失败：%v", sandboxErr)
	}
	return sandbox
}

// pairRoom 是一个"一个租户、多个 agent"的房间，用来测同房间内的不变量
// （opKey 幂等、投票计票），这类用例需要两个 agent 轮流发言才构造得出来。
type pairRoom struct {
	topic    string
	roomID   string
	password string
	adminKey string
	agents   []*tenant
}

func newPairRoom(t *testing.T, n, maxRounds, budget int) *pairRoom {
	t.Helper()
	topic := newUniqueTopic("pair-topic")
	room := createRoomOpts(t, topic, n, maxRounds, budget)
	pr := &pairRoom{topic: topic, roomID: room.RoomID, password: room.Password, adminKey: room.AdminKey}
	for i := 0; i < n; i++ {
		alias := fmt.Sprintf("pair-agent-%d", i+1)
		j, err := tryJoin(room.RoomID, room.Password, alias)
		if err != nil {
			t.Fatalf("pair 房第 %d 个 agent 进场失败：%v", i+1, err)
		}
		pr.agents = append(pr.agents, &tenant{
			name: alias, topic: topic, roomID: room.RoomID, password: room.Password,
			adminKey: room.AdminKey, agentToken: j.AgentToken, agentID: j.AgentID, alias: alias,
		})
	}
	return pr
}

// waitForState 轮询直到房间进入指定状态（用于等投票轮）。
func waitForState(t *testing.T, tn *tenant, want string) inboxWithBudget {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last inboxWithBudget
	for time.Now().Before(deadline) {
		last = inboxB(t, tn)
		if last.State == want {
			return last
		}
		// 房间提前结束就别再等了，让断言去报真实状态
		if last.State == "ended" && want != "ended" {
			t.Fatalf("等 %s 状态时房间已经 ended（round=%d）", want, last.Round)
		}
		time.Sleep(80 * time.Millisecond)
	}
	t.Fatalf("等 %s 状态超时，当前 state=%s round=%d", want, last.State, last.Round)
	return last
}

// votesPath 是 v2 的投票端点。
// v2 规定「房间里没有任何方案时的降级投票」也走这个端点，请求体用 {finalStance, opKey}。
// （规格 §九：converging 已并入 voting，finalStance 从 /messages 挪到了 /votes。）
func (tn *tenant) votesPath() string { return "/rooms/" + tn.roomID + "/votes" }

// postStance 提交一张降级投票（finalStance 形态）。
func postStance(t *testing.T, tn *tenant, stance string) resp {
	t.Helper()
	return call(t, http.MethodPost, tn.votesPath(), tn.agentToken, map[string]any{
		"finalStance": stance,
		"opKey":       newOpKey(),
	})
}

// lastOtherSeqOf 取一条"别人的"消息 seq，用作 replyTo（第 2 轮起必填）。
func lastOtherSeqOf(t *testing.T, tn *tenant) int {
	t.Helper()
	r := call(t, http.MethodGet, tn.messagesPath()+"?since=0", tn.agentToken, nil)
	if r.Status != 200 {
		return 0
	}
	var out struct {
		Messages []message `json:"messages"`
	}
	if json.Unmarshal(r.Body, &out) != nil {
		return 0
	}
	best := 0
	for _, m := range out.Messages {
		if m.AgentID == tn.agentID {
			continue
		}
		if m.Seq > best {
			best = m.Seq
		}
	}
	return best
}

// driveToVoting 把讨论轮次跑完，直到房间进入 voting（或 ended）。
//
// v2 的状态机是 briefing → debating → voting → summarizing → ended，
// 没有 converging。maxRounds=2 时要跑完「第 1 轮立场轮 + 第 2 轮收尾轮」才会投票。
func driveToVoting(t *testing.T, agents []*tenant, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st := inboxB(t, agents[0]); st.State != "debating" {
			return
		}
		advanced := false
		for _, tn := range agents {
			cur := inboxB(t, tn)
			if cur.State != "debating" {
				return
			}
			if !cur.YourTurn {
				continue
			}
			body := map[string]any{
				"text":  fmt.Sprintf("第%d轮：%s 的立场与理由。", cur.Round, tn.alias),
				"opKey": newOpKey(),
			}
			if cur.Round >= 2 {
				if seq := lastOtherSeqOf(t, tn); seq > 0 {
					body["replyTo"] = seq
				}
			}
			r := call(t, http.MethodPost, tn.messagesPath(), tn.agentToken, body)
			if r.Status == 200 || r.Status == 201 {
				advanced = true
			} else if r.Status >= 500 {
				t.Fatalf("驱动发言时服务端 5xx：%s", r.snippet())
			}
		}
		if !advanced {
			time.Sleep(60 * time.Millisecond)
		}
	}
	t.Fatalf("驱动 %v 后仍未进入 voting", timeout)
}

// ownTenant 建一个只属于本用例的租户。
//
// 提醒：Go 按文件名顺序收集测试函数，adversarial_test.go 排在 isolation_test.go 前面。
// 破坏性的用例必须用自己的房间，否则会把共享租户的轮次提前跑完，
// 让后面原套件里的"轮到我发言"类断言莫名其妙失败。
// 但服务端建房有速率限制（10 个/进程），所以能共用沙箱的尽量共用。
func ownTenant(t *testing.T, name string) *tenant {
	t.Helper()
	tn, err := tryNewTenant(name)
	if err != nil {
		t.Fatalf("建专属租户 %s 失败：%v", name, err)
	}
	return tn
}

/* ══════════════════════════════════════════════════════════════════════════
   攻击 1：注入
   ──────────────────────────────────────────────────────────────────────────
   服务端底层是 SQLite，注入一旦成立后果是"读别人的库"。
   这里不假设它一定拼 SQL，只断言：不 5xx、不越权、不改语义。
   ══════════════════════════════════════════════════════════════════════════ */

func TestInjectionPayloads(t *testing.T) {
	// 注入会往房间里灌很多消息，用沙箱租户，别污染共享夹具（也别多建房间）
	a := sandboxTenant(t)
	_, b, _ := sharedTenants(t)

	payloads := []struct {
		name string
		val  string
	}{
		{"SQLi-OR", "' OR 1=1--"},
		{"SQLi-DROP", "'; DROP TABLE rooms;--"},
		{"SQLi-UNION", "' UNION SELECT 1,2,3--"},
		{"NUL", "a\x00b"},
		{"percent-NUL", "%00"},
		{"path-traversal", "../../etc/passwd"},
		{"encoded-traversal", "..%2F..%2Fapi%2Frooms"},
		{"long-cjk", strings.Repeat("啊", 4000)},
		{"emoji", "😀👨‍👩‍👧‍👦🏳️‍🌈"},
		{"fullwidth", "ＡＢＣ０１２"},
		{"quote", `{"$ne":null}`},
	}

	// 1) roomId 注入：不能 5xx，不能 2xx（别人的房间不存在这种 id）
	for _, p := range payloads {
		id := url.PathEscape(p.val)
		r := call(t, http.MethodGet, "/rooms/"+id+"/inbox?wait=0", a.agentToken, nil)
		expectNo5xx(t, r, "攻击1 roomId注入（"+p.name+"）到 inbox")
		if r.Status == 200 {
			t.Errorf("攻击1：roomId 注入「%s」竟然返回 200", p.name)
		}
		r2 := call(t, http.MethodGet, "/rooms/"+id+"/result", "", nil)
		expectNo5xx(t, r2, "攻击1 roomId注入（"+p.name+"）到 result")
		if r2.Status == 200 {
			t.Errorf("攻击1：roomId 注入「%s」打 result 竟然 200", p.name)
		}
	}

	// 2) 文本注入：要么收下当普通文本，要么拒绝；绝不能 5xx 或影响别的房间
	for _, p := range payloads {
		body := map[string]any{"text": p.val, "opKey": newOpKey()}
		r := call(t, http.MethodPost, a.messagesPath(), a.agentToken, body)
		expectNo5xx(t, r, "攻击1 text注入（"+p.name+"）")
	}

	// 注入之后，B 的房间必须毫发无损
	if r := call(t, http.MethodGet, "/rooms/"+b.roomID+"/result", "", nil); r.Status != 200 {
		t.Errorf("攻击1：一轮注入之后 B 的结果页不可读了（HTTP %d）——可能被注入破坏了", r.Status)
	} else {
		for _, p := range payloads {
			if p.name == "SQLi-DROP" && strings.Contains(string(r.Body), "DROP TABLE") {
				t.Errorf("攻击1：B 的结果页里出现了注入载荷，说明数据被串了")
			}
		}
	}

	// 3) 非法 UTF-8（截断的 emoji）：JSON 编码器会把它"修好"，所以走原始字节
	broken := []byte(`{"text":"截断的😀` + "\xe4\xb8" + `","opKey":"` + newOpKey() + `"}`)
	if r, err := doRaw(http.MethodPost, a.messagesPath(), map[string]string{
		"authorization": "Bearer " + a.agentToken,
	}, broken, 10*time.Second); err != nil {
		t.Errorf("攻击1：非法 UTF-8 请求直接失败（连接被断？）：%v", err)
	} else {
		expectNo5xx(t, r, "攻击1 非法 UTF-8 消息体")
	}

	// 4) 超长 alias / model 进场：不许把服务打挂（复用 open 房间，省一个建房配额）
	room := sharedOpenRoom(t)
	jr := call(t, http.MethodPost, "/rooms/"+room.RoomID+"/join", "", map[string]any{
		"password": room.Password,
		"alias":    strings.Repeat("名", 2000),
		"model":    "' OR 1=1--",
	})
	expectNo5xx(t, jr, "攻击1 超长 alias + 注入 model 进场")
}

/* ══════════════════════════════════════════════════════════════════════════
   攻击 2：枚举 —— 200 个 roomId × 3 个端点
   ──────────────────────────────────────────────────────────────────────────
   不仅要全 404，还要求响应"互相不可区分"：如果存在的房间比不存在的快、
   或者 body 长度有系统性差异，攻击者依然能筛出真房间。
   ══════════════════════════════════════════════════════════════════════════ */

func TestEnumerationUniformAndFast(t *testing.T) {
	a, b, _ := sharedTenants(t)

	const N = 200
	ids := make([]string, 0, N)
	// 一半纯随机，一半"近似真实 id"（改一位），后者最容易暴露实现差异
	rnd := uint64(time.Now().UnixNano())
	for i := 0; i < N/2; i++ {
		rnd = rnd*6364136223846793005 + 1442695040888963407
		ids = append(ids, fmt.Sprintf("r_%c%c%c%c",
			'a'+byte(rnd%26), 'a'+byte((rnd>>8)%26), 'a'+byte((rnd>>16)%26), 'a'+byte((rnd>>24)%26)))
	}
	baseID := a.roomID
	for i := 0; len(ids) < N; i++ {
		if len(baseID) > 1 {
			mut := baseID[:len(baseID)-1]
			ids = append(ids, mut+string(rune('a'+i%26)))
		} else {
			ids = append(ids, fmt.Sprintf("r_%04d", i))
		}
	}

	type ep struct {
		name string
		path func(id string) string
		tok  string
	}
	eps := []ep{
		{"meta", func(id string) string { return "/rooms/" + id }, ""},
		{"result", func(id string) string { return "/rooms/" + id + "/result" }, ""},
		{"inbox", func(id string) string { return "/rooms/" + id + "/inbox?wait=0" }, a.agentToken},
	}

	for _, e := range eps {
		// 按状态码分别累积。
		//
		// 429 是枚举限流，它对"房间存在"和"房间不存在"一视同仁，所以不算泄漏。
		// 但断言不能只放宽了事：同一状态码内部必须仍只有一种响应形态，
		// 否则等于把 404 的泄漏换成了 429 的泄漏。
		bodiesByStatus := map[int]map[string]int{}
		statusCount := map[int]int{}
		var durs []time.Duration
		unexpected := 0

		for _, id := range ids {
			if id == a.roomID || id == b.roomID {
				continue
			}
			start := time.Now()
			r := call(t, http.MethodGet, e.path(id), e.tok, nil)
			durs = append(durs, time.Since(start))
			statusCount[r.Status]++

			switch r.Status {
			case http.StatusNotFound, http.StatusTooManyRequests:
				// 合法结果
			default:
				unexpected++
				if unexpected <= 3 {
					t.Errorf("攻击2：枚举 %s 端点，id=%q 期望 404 或 429，实际 %d\n  响应 %s",
						e.name, id, r.Status, r.snippet())
				}
				continue
			}
			// 把回显的 id 抹掉再比较，否则"body 里带 id"会被误判成差异
			norm := strings.ReplaceAll(string(r.Body), id, "{ID}")
			if bodiesByStatus[r.Status] == nil {
				bodiesByStatus[r.Status] = map[string]int{}
			}
			bodiesByStatus[r.Status][norm]++
		}

		if unexpected > 3 {
			t.Errorf("攻击2：%s 端点共有 %d 个用例返回了 404/429 之外的状态", e.name, unexpected)
		}
		if statusCount[http.StatusNotFound] == 0 {
			t.Errorf("攻击2：%s 端点一次 404 都没有（状态分布 %v）—— 无法验证 404 响应之间不可区分",
				e.name, statusCount)
		}

		for status, variants := range bodiesByStatus {
			if len(variants) <= 1 {
				continue
			}
			list := make([]string, 0, len(variants))
			for k, n := range variants {
				list = append(list, fmt.Sprintf("(出现%d次) %s", n, firstN(k, 140)))
			}
			sort.Strings(list)
			t.Errorf("攻击2：%s 端点的 %d 响应体有 %d 种形态，存在可区分性\n  %s",
				e.name, status, len(variants), strings.Join(list, "\n  "))
		}
		t.Logf("攻击2：%s 端点状态分布 %v", e.name, statusCount)

		// 耗时不可区分（放得很宽，只抓量级差异；本地抖动不该误报）
		if len(durs) > 10 {
			zeros := 0
			for _, d := range durs {
				if d == 0 {
					zeros++
				}
			}
			sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
			p50 := durs[len(durs)/2].Nanoseconds()
			p95 := durs[len(durs)*95/100].Nanoseconds()
			maxd := durs[len(durs)-1].Nanoseconds()
			sum := int64(0)
			for _, d := range durs {
				sum += d.Nanoseconds()
			}
			avg := sum / int64(len(durs))
			t.Logf("攻击2：%s 端点 n=%d zeros=%d p50=%dns p95=%dns max=%dns avg=%dns",
				e.name, len(durs), zeros, p50, p95, maxd, avg)

			// 绝对闸门：查一个不存在的房间不该超过 100ms。
			// 全量并行跑时这条会被负载抖动打中，所以**有界重试 3 次**：
			// 每次都重新采样，3 轮都超阈值才判负 —— 不删断言、不 Skip。
			if maxd > int64(100*time.Millisecond) {
				persistent := true
				for try := 0; try < 3; try++ {
					var mx int64
					for i := 0; i < 30; i++ {
						s := time.Now()
						call(t, http.MethodGet, "/rooms/r_recheck"+strconv.Itoa(i)+"/result", "", nil)
						if d := time.Since(s).Nanoseconds(); d > mx {
							mx = d
						}
					}
					if mx <= int64(100*time.Millisecond) {
						t.Logf("攻击2：%s 端点首轮 max=%v 超闸门，重采样第 %d 轮恢复（max=%v），判为负载抖动",
							e.name, time.Duration(maxd), try+1, time.Duration(mx))
						persistent = false
						break
					}
				}
				if persistent {
					t.Errorf("攻击2：%s 端点有请求耗时 %v，且 3 轮重采样仍然超 100ms 闸门 —— 远超正常 404 的开销",
						e.name, time.Duration(maxd))
				}
			}

			// 存在性的计时侧信道：同一个 token 打「不存在的房间」和「存在但无权访问的房间」，
			// 两者都应该是 404，且平均耗时要在一个量级内。
			//
			// 这里用平均值而不是 p50/p95：本机时钟量化很粗（实测一半以上的 time.Since 恰好为 0），
			// 分位数不可靠；平均值能把量化误差摊平，仍能抓到"多查了一次库 / 多返回了几百字节"这类系统性差异。
			// 只对 inbox 做，因为 meta 和 result 对存在的房间会返回 200，状态码本来就不同，比耗时没有意义。
			if e.name == "inbox" {
				const M = 100
				var sum int64
				wrong := 0
				for i := 0; i < M; i++ {
					s := time.Now()
					r := call(t, http.MethodGet, "/rooms/"+b.roomID+"/inbox?wait=0", a.agentToken, nil)
					sum += time.Since(s).Nanoseconds()
					if r.Status != http.StatusNotFound {
						wrong++
					}
				}
				avgExisting := sum / M
				t.Logf("攻击2：inbox 平均耗时——不存在 %dns / 存在但无权 %dns（各 %d 次）", avg, avgExisting, M)
				if wrong > 0 {
					t.Errorf("攻击2：存在但无权的房间有 %d/%d 次没返回 404", wrong, M)
				}
				if avg > 0 && avgExisting > 0 {
					ratio := float64(avgExisting) / float64(avg)
					t.Logf("攻击2：inbox 计时比值 = %.2f×（阈值 2.00×）", ratio)
					if ratio > 2.0 || ratio < 0.5 {
						// 有界重试：并行负载下比值会抖，3 轮都超才判负（不删断言、不 Skip）
						persistent := true
						for try := 0; try < 3; try++ {
							var s1, s2 int64
							for k := 0; k < 50; k++ {
								st := time.Now()
								call(t, http.MethodGet, "/rooms/r_nope"+strconv.Itoa(k)+"/inbox?wait=0", a.agentToken, nil)
								s1 += time.Since(st).Nanoseconds()
								st = time.Now()
								call(t, http.MethodGet, "/rooms/"+b.roomID+"/inbox?wait=0", a.agentToken, nil)
								s2 += time.Since(st).Nanoseconds()
							}
							if s1 == 0 {
								continue
							}
							r2 := float64(s2) / float64(s1)
							if r2 <= 2.0 && r2 >= 0.5 {
								t.Logf("攻击2：inbox 比值首轮 %.2f× 超阈值，重采样第 %d 轮恢复（%.2f×），判为负载抖动",
									ratio, try+1, r2)
								persistent = false
								break
							}
						}
						if persistent {
							t.Errorf("攻击2：存在但无权的房间耗时是不存在房间的 %.2f 倍（阈值 2.0），且 3 轮重采样仍未恢复 —— 存在性可被计时侧信道区分", ratio)
						}
					}
				}
			}
		} // 收 if len(durs) > 10
	}
}

/* ══════════════════════════════════════════════════════════════════════════
   攻击 3：凭证混淆
   ══════════════════════════════════════════════════════════════════════════ */

func TestCredentialConfusion(t *testing.T) {
	a, b, c := sharedTenants(t)

	// 3.1 A 的 token + B 的 roomId，打遍所有需要鉴权的端点
	endpoints := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/rooms/" + b.roomID + "/inbox?wait=0", nil},
		{http.MethodGet, "/rooms/" + b.roomID + "/messages?since=0", nil},
		{http.MethodPost, "/rooms/" + b.roomID + "/messages", map[string]any{"text": "x", "opKey": newOpKey()}},
	}
	for _, e := range endpoints {
		r := call(t, e.method, e.path, a.agentToken, e.body)
		if r.Status >= 200 && r.Status < 300 {
			t.Errorf("攻击3：A 的 token 打 B 的 %s %s 竟然成功（%d）", e.method, e.path, r.Status)
		}
		expectNo5xx(t, r, "攻击3 A-token/B-room "+e.path)
	}

	// 3.2 adminKey 当 Bearer 打只读端点
	for _, p := range []string{a.inboxPath() + "?wait=0", a.messagesPath() + "?since=0"} {
		r := call(t, http.MethodGet, p, a.adminKey, nil)
		if r.Status >= 200 && r.Status < 300 {
			t.Errorf("攻击3：adminKey 当 Bearer 读 %s 竟然成功（%d）—— 管理凭证混进了 agent 身份空间", p, r.Status)
		}
	}

	// 3.3 用别人的 agentId 当 token
	for _, tok := range []string{b.agentID, c.agentID, a.agentID} {
		r := call(t, http.MethodGet, a.inboxPath()+"?wait=0", tok, nil)
		if r.Status >= 200 && r.Status < 300 {
			t.Errorf("攻击3：用 agentId %q 当 token 竟然通过鉴权", tok)
		}
	}

	// 3.4 token 内部结构手术：把 agent 段替换成别人的
	surgery := []string{
		strings.ReplaceAll(a.agentToken, a.agentID, b.agentID),
		strings.ReplaceAll(a.agentToken, "m_1", "m_2"),
		strings.ReplaceAll(a.agentToken, a.roomID, b.roomID),
	}
	for i, tok := range surgery {
		if tok == a.agentToken {
			continue // 这个替换没命中，跳过
		}
		r := call(t, http.MethodGet, "/rooms/"+b.roomID+"/inbox?wait=0", tok, nil)
		if r.Status >= 200 && r.Status < 300 {
			t.Errorf("攻击3：token 结构手术 #%d 后能读 B 的 inbox（%d）—— token 不是不可构造的", i, r.Status)
		}
	}

	// 3.5 查询参数冒充：?agentId= / ?tenant= / ?as=
	for _, q := range []string{"agentId=" + b.agentID, "tenant=tenantB", "as=" + b.agentID} {
		r := call(t, http.MethodGet, "/rooms/"+a.roomID+"/inbox?wait=0&"+q, a.agentToken, nil)
		if r.Status == 200 && strings.Contains(string(r.Body), b.topic) {
			t.Errorf("攻击3：查询参数 %q 改变了租户判定，看到了 B 的数据", q)
		}
		expectNo5xx(t, r, "攻击3 查询参数冒充 "+q)
	}

	// 3.6 正向对照：A 自己的 token + 自己的房间必须能读，否则上面的否定断言没意义
	if r := call(t, http.MethodGet, a.inboxPath()+"?wait=0", a.agentToken, nil); r.Status != 200 {
		t.Fatalf("攻击3 对照组：A 读自己的房间都失败（HTTP %d），前面的断言不可信", r.Status)
	}
}

/* ══════════════════════════════════════════════════════════════════════════
   攻击 4：头部走私
   ══════════════════════════════════════════════════════════════════════════ */

func TestHeaderSmuggling(t *testing.T) {
	a, b, _ := sharedTenants(t)

	headers := []map[string]string{
		{"X-Tenant": "tenantB"},
		{"X-Tenant-Id": b.roomID},
		{"X-Forwarded-Host": "evil.example"},
		{"X-Forwarded-For": "10.0.0.1"},
		{"X-Original-URL": "/rooms/" + b.roomID + "/inbox"},
		{"X-Rewrite-URL": "/rooms/" + b.roomID + "/messages"},
		{"X-HTTP-Method-Override": "DELETE"},
		{"Host": "evil.example"},
	}

	for _, h := range headers {
		name := firstKey(h)

		// 4.1 拿 A 的 token 去读 B，希望靠头部"变成"B —— 必须失败
		r := callH(t, http.MethodGet, "/rooms/"+b.roomID+"/inbox?wait=0", a.agentToken, h, nil)
		if r.Status >= 200 && r.Status < 300 {
			t.Errorf("攻击4：加头部 %s 之后，A 的 token 读到了 B 的 inbox", name)
		}
		expectNo5xx(t, r, "攻击4 "+name+" → B inbox")

		// 4.2 读自己的房间时加"冒充别人"的头，返回内容不能变成 B 的
		r2 := callH(t, http.MethodGet, a.inboxPath()+"?wait=0", a.agentToken, h, nil)
		expectNo5xx(t, r2, "攻击4 "+name+" → A inbox")
		if r2.Status == 200 && strings.Contains(string(r2.Body), b.topic) {
			t.Errorf("攻击4：加头部 %s 之后，A 的 inbox 里出现了 B 的议题", name)
		}

		// 4.3 方法覆盖头不能让 GET 变成破坏性操作
		r3 := callH(t, http.MethodGet, "/rooms/"+a.roomID+"/result", "", h, nil)
		expectNo5xx(t, r3, "攻击4 "+name+" → A result")
	}

	// 4.4 头部乱放一通之后，房间必须还在
	if r := call(t, http.MethodGet, "/rooms/"+a.roomID+"/result", "", nil); r.Status != 200 {
		t.Errorf("攻击4：一轮头部走私之后 A 的房间不可读了（%d）—— 可能被方法覆盖头破坏", r.Status)
	}
}

func firstKey(m map[string]string) string {
	for k := range m {
		return k
	}
	return ""
}

/* ══════════════════════════════════════════════════════════════════════════
   攻击 5：并发 join 不能超卖名额
   ══════════════════════════════════════════════════════════════════════════ */

func TestConcurrentJoinDoesNotOversell(t *testing.T) {
	const capacity = 3
	const attempts = 8

	room := createRoomOpts(t, newUniqueTopic("oversell"), capacity, 2, 200000)

	var wg sync.WaitGroup
	var mu sync.Mutex
	statuses := make([]int, 0, attempts)
	errs := make([]error, 0, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r, err := do(http.MethodPost, "/rooms/"+room.RoomID+"/join", "", map[string]any{
				"password": room.Password,
				"alias":    fmt.Sprintf("race-%d", idx),
				"model":    "race",
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			statuses = append(statuses, r.Status)
		}(i)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Errorf("攻击5：%d 个并发 join 出现连接错误：%v", len(errs), errs[0])
	}

	ok, full, other := 0, 0, []int{}
	for _, s := range statuses {
		switch {
		case s == 200 || s == 201:
			ok++
		case s == http.StatusConflict || s == http.StatusGone || s == http.StatusTooManyRequests:
			// 409 room_full 是期望；429 是 join 限流（默认 8/分钟，8 个并发刚好踩线），
			// 410 是房间已开辩。三种都算"没让人进来"。
			full++
		default:
			other = append(other, s)
			if s >= 500 {
				t.Errorf("攻击5：join 返回 5xx（%d），并发下未处理的状态", s)
			}
		}
	}

	if ok != capacity {
		t.Errorf("攻击5：名额超卖/少卖。容量 %d，实际成功 %d（状态码分布：%v）", capacity, ok, statuses)
	}
	if ok+full+len(other) != attempts {
		t.Errorf("攻击5：状态码统计不平，attempts=%d ok=%d full/limited=%d other=%d",
			attempts, ok, full, len(other))
	}
	t.Logf("攻击5：并发 %d 次 join，成功 %d，被拒 %d，其它 %v", attempts, ok, full, other)
}

/* ══════════════════════════════════════════════════════════════════════════
   攻击 6：opKey 幂等不能被绕过，也不能误合并
   ══════════════════════════════════════════════════════════════════════════ */

func TestOpKeyIdempotency(t *testing.T) {
	pr := newPairRoom(t, 2, 3, 200000)
	first, second := pr.agents[0], pr.agents[1]

	// 等 first 拿到发言权
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if inboxB(t, first).State == "debating" {
			break
		}
		time.Sleep(80 * time.Millisecond)
	}

	const text = "同一条逻辑消息"
	opKey := newOpKey()

	// 6.1 同一个 opKey 并发发两次 → 只落一条
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := []int{}
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := do(http.MethodPost, first.messagesPath(), first.agentToken, map[string]any{
				"text": text, "opKey": opKey,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				codes = append(codes, r.Status)
			}
		}()
	}
	wg.Wait()

	for _, s := range codes {
		if s >= 500 {
			t.Errorf("攻击6：同 opKey 并发发送返回 5xx（%d）", s)
		}
	}

	msgs := listMessages(t, first)
	if n := countText(msgs, text); n != 1 {
		t.Errorf("攻击6：同一个 opKey 发两次落了 %d 条（期望 1 条）—— 幂等被绕过", n)
	}

	// 6.2 不同 opKey、相同内容 → 必须是两条，不能被"内容去重"误合并
	r := call(t, http.MethodPost, second.messagesPath(), second.agentToken, map[string]any{
		"text": text, "opKey": newOpKey(),
	})
	if r.Status != 200 && r.Status != 201 {
		t.Fatalf("攻击6：第二个 agent 发言失败 HTTP %d：%s", r.Status, r.snippet())
	}
	msgs = listMessages(t, first)
	if n := countText(msgs, text); n != 2 {
		t.Errorf("攻击6：两个不同 opKey 的相同内容只落了 %d 条（期望 2 条）—— 内容被误合并", n)
	}
}

func countText(msgs []message, want string) int {
	n := 0
	for _, m := range msgs {
		if strings.TrimSpace(m.Text) == want {
			n++
		}
	}
	return n
}

/* ══════════════════════════════════════════════════════════════════════════
   攻击 7：收敛轮投票越权 / 重复计票
   ══════════════════════════════════════════════════════════════════════════ */

func TestConvergingVoteIntegrity(t *testing.T) {
	// v2 状态机：converging 已并入 voting。
	// maxRounds=2 → 第 1 轮立场轮 + 第 2 轮收尾轮，两轮都说完才进 voting。
	pr := newPairRoom(t, 2, 2, 200000)
	r1, r2 := pr.agents[0], pr.agents[1]

	waitForState(t, r1, "debating")
	driveToVoting(t, []*tenant{r1, r2}, 60*time.Second)
	waitForState(t, r1, "voting")

	// 7.1 非成员提交投票 → 必须拒绝
	_, _, c := sharedTenants(t)
	nr := call(t, http.MethodPost, r1.votesPath(), c.agentToken, map[string]any{
		"finalStance": "agree", "opKey": newOpKey(),
	})
	if nr.Status >= 200 && nr.Status < 300 {
		t.Errorf("攻击7：非房间成员竟然能投票（HTTP %d）", nr.Status)
	}
	expectNo5xx(t, nr, "攻击7 非成员投票")

	// 7.2 同一个 agent 投两次 → 最终只计一票
	if r := postStance(t, r1, "agree"); r.Status != 200 && r.Status != 201 {
		t.Fatalf("攻击7：r1 第一次投票失败 HTTP %d：%s", r.Status, r.snippet())
	}
	dup := postStance(t, r1, "disagree")
	if dup.Status >= 500 {
		t.Errorf("攻击7：重复投票返回 5xx（%d）", dup.Status)
	}
	t.Logf("攻击7：重复投票的状态码 = %d（2xx 表示允许改票，4xx 表示拒绝重复，两者都可接受，只要不重复计票）",
		dup.Status)

	if r := postStance(t, r2, "disagree"); r.Status != 200 && r.Status != 201 {
		t.Fatalf("攻击7：r2 投票失败 HTTP %d：%s", r.Status, r.snippet())
	}

	// 7.3 等房间结束，看票数
	var result map[string]any
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status, body := getResult(t, pr.roomID)
		if status == 200 {
			var m map[string]any
			if json.Unmarshal(body, &m) == nil && (m["state"] == "ended" || m["state"] == "summarizing") {
				result = m
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if result == nil {
		t.Fatalf("攻击7：等 15s 房间还没结束，无法验证票数")
	}

	concl, _ := result["conclusion"].(map[string]any)
	votes, _ := concl["voteTotals"].(map[string]any)
	agree := numOf(votes["agree"])
	disagree := numOf(votes["disagree"])
	abstain := numOf(votes["abstain"])
	total := agree + disagree + abstain

	t.Logf("攻击7：最终票数 agree=%.0f disagree=%.0f abstain=%.0f", agree, disagree, abstain)
	if total != 2 {
		t.Errorf("攻击7：总票数 %.0f，但房间里只有 2 个 agent —— 有人被重复计票（或外人混进来了）", total)
	}
	if agree > 1 {
		t.Errorf("攻击7：同一个 agent 投了两次都被计入（agree=%.0f）", agree)
	}
}

func numOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case nil:
		return 0
	}
	return 0
}

/* ══════════════════════════════════════════════════════════════════════════
   攻击 8：预算计费不能被绕过；预算打穿后仍须能交 finalStance
   ══════════════════════════════════════════════════════════════════════════ */

func TestReadIsChargedAndFinalStanceSurvivesBudget(t *testing.T) {
	// 一个房间同时验证两件事，省建房配额（服务端建房限流 10 个/进程）：
	//   A. 反复读同一段历史，预算必须累加 —— 读是花钱的，否则 O(N²) 成本无从约束
	//   B. 预算被打穿之后，仍必须能交 finalStance —— 产品承诺"必须有结论"优先于省钱
	const tinyBudget = 1000
	// v2：单人入会会跳过辩论直接出结论（规格 §八 #2），构造不出 debating→voting，
	// 所以这里必须 2 人。用例意图不变：读计费 + 预算耗尽后仍能投票。
	pr := newPairRoom(t, 2, 2, tinyBudget)
	tn := pr.agents[0]

	waitForState(t, tn, "debating")
	driveToVoting(t, pr.agents, 60*time.Second)
	waitForState(t, tn, "voting")

	// ── A. 读必须计费 ─────────────────────────────────────────────
	before := inboxB(t, tn).Budget.Used
	reads := 0
	for i := 0; i < 5; i++ {
		r := call(t, http.MethodGet, tn.messagesPath()+"?since=0", tn.agentToken, nil)
		if r.Status == 402 {
			break
		}
		if r.Status != 200 {
			t.Fatalf("攻击8A：第 %d 次重读历史失败 HTTP %d：%s", i+1, r.Status, r.snippet())
		}
		reads++
	}
	after := inboxB(t, tn).Budget.Used
	t.Logf("攻击8A：重读 %d 次，预算 used %.0f → %.0f", reads, before, after)
	if reads > 0 && after <= before {
		t.Errorf("攻击8A：反复读同一段历史预算没有增长（%.0f → %.0f）—— 读未计费，成本闸门失效", before, after)
	}

	// ── B. 把预算读穿，再交 finalStance ───────────────────────────
	pct := after / float64(tinyBudget) * 100
	for i := 0; i < 400; i++ {
		r := call(t, http.MethodGet, tn.messagesPath()+"?since=0", tn.agentToken, nil)
		if r.Status == 402 {
			pct = 100
			break
		}
		if r.Status != 200 {
			t.Fatalf("攻击8B：耗尽预算过程中返回意外状态 %d：%s", r.Status, r.snippet())
		}
		pct = inboxB(t, tn).Budget.Pct
		if pct >= 96 {
			break
		}
	}
	t.Logf("攻击8B：预算已用 %.1f%%", pct)

	r := postStance(t, tn, "agree")
	if r.Status < 200 || r.Status >= 300 {
		t.Errorf("攻击8B：预算打穿后交 finalStance 被拒（HTTP %d）—— 违反'必须有结论'的产品承诺\n  响应 %s",
			r.Status, r.snippet())
	}
}

/* ══════════════════════════════════════════════════════════════════════════
   攻击 9：超大包 / 长轮询占用
   ══════════════════════════════════════════════════════════════════════════ */

func TestOversizedBodyAndLongPollHeadroom(t *testing.T) {
	// 长轮询要挂在一个"还活着"的房间上才有意义；和注入用例共用沙箱，省一个建房配额
	a := sandboxTenant(t)

	// 9.1 5MB body：必须被快速拒绝（4xx），不能 5xx、不能挂住
	big := make([]byte, 5<<20)
	for i := range big {
		big[i] = 'a'
	}
	start := time.Now()
	r, err := doRaw(http.MethodPost, a.messagesPath(),
		map[string]string{"authorization": "Bearer " + a.agentToken}, big, 30*time.Second)
	took := time.Since(start)
	if err != nil {
		t.Errorf("攻击9：5MB 包直接导致连接错误（%v）—— 应返回 4xx 而不是断连", err)
	} else {
		t.Logf("攻击9：5MB body 返回 %d，耗时 %v", r.Status, took)
		expectNo5xx(t, r, "攻击9 5MB body")
		if r.Status >= 200 && r.Status < 300 {
			t.Errorf("攻击9：5MB body 竟然被接受（%d）—— 没有体积上限", r.Status)
		}
		if took > 20*time.Second {
			t.Errorf("攻击9：5MB body 处理耗时 %v，可能是全量读进内存后才拒绝", took)
		}
	}

	// 9.2 50 个并发长轮询，其余请求必须仍然有响应
	cur := inboxB(t, a).Cursor
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var completed int64
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := fmt.Sprintf("%s/rooms/%s/inbox?since=%d&wait=8", baseURL, a.roomID, cur)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				atomic.AddInt64(&completed, 1)
				return
			}
			req.Header.Set("authorization", "Bearer "+a.agentToken)
			res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
			if err == nil {
				_, _ = io.ReadAll(io.LimitReader(res.Body, 1<<16))
				res.Body.Close()
			}
			atomic.AddInt64(&completed, 1)
		}()
	}
	time.Sleep(500 * time.Millisecond) // 让它们都挂到长轮询上

	// 关键校验：这 50 个请求此刻必须真的还挂着，否则下面的"普通请求仍然快"
	// 就是空的（它们要是立刻返回了，压根没占住连接）。
	if n := atomic.LoadInt64(&completed); n > 0 {
		t.Errorf("攻击9：50 个长轮询里有 %d 个在 500ms 内就返回了，没有真正挂住，"+
			"这条用例失去意义（检查 since 游标是否已落后于事件计数）", n)
	}

	worst := time.Duration(0)
	for i := 0; i < 5; i++ {
		s := time.Now()
		rr := call(t, http.MethodGet, "/rooms/"+a.roomID+"/result", "", nil)
		d := time.Since(s)
		if d > worst {
			worst = d
		}
		if rr.Status != 200 {
			t.Errorf("攻击9：50 个并发长轮询期间，普通请求失败 HTTP %d", rr.Status)
		}
	}
	t.Logf("攻击9：50 个并发长轮询下，普通请求最慢 %v", worst)
	if worst > 3*time.Second {
		t.Errorf("攻击9：长轮询占满了连接池/工作协程，普通请求被拖到 %v（应 < 3s）", worst)
	}

	cancel()
	wg.Wait()

	// 收尾：服务必须还活着
	if rr := call(t, http.MethodGet, "/rooms/"+a.roomID+"/result", "", nil); rr.Status != 200 {
		t.Errorf("攻击9：50 个长轮询取消之后服务不健康了（HTTP %d）", rr.Status)
	}
}
