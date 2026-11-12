package meeting

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

/* ══════════════════════════════════════════════════════════════════════════
   会议夹具
   ══════════════════════════════════════════════════════════════════════════ */

type member struct {
	alias       string
	model       string
	token       string
	id          string
	order       int
	isModerator bool
	brief       joinBrief // join 时服务端下发的这份框架（含 limits）
}

type meeting struct {
	t        *testing.T
	srv      *server
	roomID   string
	roomNo   string // 形如 "728 451 936"，可能由 join 时才拿到
	password string
	adminKey string
	topic    string
	members  []*member
	byAlias  map[string]*member

	// 本夹具提交过的方案，按提交顺序
	props proposalSet
}

// proposalSet 记录方案 id / 标题 / 提交者，顺序即提交顺序（平票判定要用到）。
type proposalSet struct {
	order []string
	title map[string]string
	owner map[string]string
}

type joinBrief struct {
	RoomNo  string `json:"roomNo"`
	Topic   string `json:"topic"`
	Prompt  string `json:"prompt"`
	Rules   string `json:"rules"`
	Rounds  int    `json:"rounds"`
	Members int    `json:"members"`
	Limits  struct {
		TokenBudget          float64 `json:"tokenBudget"`
		Used                 float64 `json:"used"`
		MaxMinutes           float64 `json:"maxMinutes"`
		MaxOutputTokens      float64 `json:"maxOutputTokens"`
		MaxProposalsPerAgent float64 `json:"maxProposalsPerAgent"`
	} `json:"limits"`
	YouAreModerator bool   `json:"youAreModerator"`
	SkillURL        string `json:"skillUrl"`
}

type joinResp struct {
	AgentToken    string    `json:"agentToken"`
	AgentID       string    `json:"agentId"`
	IsModerator   bool      `json:"isModerator"`
	SpeakingOrder int       `json:"speakingOrder"`
	Brief         joinBrief `json:"brief"`
}

type inboxState struct {
	Cursor      int    `json:"cursor"`
	State       string `json:"state"`
	Round       int    `json:"round"`
	RoundsTotal int    `json:"roundsTotal"`
	YourTurn    bool   `json:"yourTurn"`
	Unread      int    `json:"unread"`
	Budget      struct {
		Used  float64 `json:"used"`
		Total float64 `json:"total"`
		Pct   float64 `json:"pct"`
	} `json:"budget"`
	Notes []string `json:"notes"`
}

type msg struct {
	Seq               int    `json:"seq"`
	Round             int    `json:"round"`
	AgentID           string `json:"agentId"`
	Alias             string `json:"alias"`
	Text              string `json:"text"`
	ReplyTo           *int   `json:"replyTo"`
	SupportProposalID string `json:"supportProposalId"`
}

// newMeeting 建房。opts 直接透传（tokenBudget / maxRounds / maxAgents / maxMinutes…）。
func newMeeting(t *testing.T, srv *server, opts map[string]any) *meeting {
	t.Helper()
	topic := fmt.Sprintf("把定价从年付 ¥199 改成月付 ¥29，要不要？[%d]", time.Now().UnixNano())

	body := map[string]any{
		"topic": topic,
		"rules": "只讨论定价策略本身；必须给出可执行结论；不许互相客套。",
	}
	for k, v := range opts {
		body[k] = v
	}

	r := call(t, srv.base, http.MethodPost, "/rooms", "", body)
	if r.Status != 200 && r.Status != 201 {
		t.Fatalf("建房失败 HTTP %d：%s", r.Status, r.snippet())
	}

	var room struct {
		RoomID   string `json:"roomId"`
		RoomNo   string `json:"roomNo"`
		Password string `json:"password"`
		AdminKey string `json:"adminKey"`
	}
	if err := json.Unmarshal(r.Body, &room); err != nil {
		t.Fatalf("建房响应不是 JSON：%v（%s）", err, r.snippet())
	}
	if room.RoomID == "" {
		t.Fatalf("建房响应缺 roomId：%s", r.snippet())
	}
	if room.Password == "" {
		t.Fatalf("建房响应缺 password：%s", r.snippet())
	}

	return &meeting{
		t: t, srv: srv, roomID: room.RoomID, roomNo: room.RoomNo,
		password: room.Password, adminKey: room.AdminKey, topic: topic,
		byAlias: map[string]*member{},
	}
}

// digits 返回去掉空格/连字符的会议号。
func (m *meeting) digits() string {
	strip := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			if r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	if m.roomNo != "" {
		return strip(m.roomNo)
	}
	if m.roomID != "" {
		return strip(m.roomID)
	}
	return ""
}

// joinHandle 用指定的房间句柄进场（用于测会议号的各种书写格式）。
func (m *meeting) joinHandle(alias, handle string) resp {
	m.t.Helper()
	return call(m.t, m.srv.base, http.MethodPost,
		"/rooms/"+url.PathEscape(handle)+"/join", "", map[string]any{
			"password": m.password,
			"alias":    alias,
			"model":    "test-model",
		})
}

// join 用内部 roomId 进场，并把成员登记进夹具。
func (m *meeting) join(alias string) *member {
	m.t.Helper()
	r := m.joinHandle(alias, m.roomID)
	if r.Status != 200 && r.Status != 201 {
		m.t.Fatalf("%s 进场失败 HTTP %d：%s", alias, r.Status, r.snippet())
	}
	var jr joinResp
	if err := json.Unmarshal(r.Body, &jr); err != nil {
		m.t.Fatalf("进场响应不是 JSON：%v（%s）", err, r.snippet())
	}
	if jr.AgentToken == "" {
		m.t.Fatalf("进场响应缺 agentToken：%s", r.snippet())
	}
	if m.roomNo == "" && jr.Brief.RoomNo != "" {
		m.roomNo = jr.Brief.RoomNo
	}
	mm := &member{
		alias: alias, model: "test-model", token: jr.AgentToken, id: jr.AgentID,
		order: jr.SpeakingOrder, isModerator: jr.IsModerator, brief: jr.Brief,
	}
	m.members = append(m.members, mm)
	m.byAlias[alias] = mm
	return mm
}

func (m *meeting) joinN(n int) {
	for i := 1; i <= n; i++ {
		m.join(fmt.Sprintf("agent-%d", i))
	}
}

// topicPrefix 取议题前 n 个字符（不加省略号），用于在结果文本里做包含判断。
func (m *meeting) topicPrefix(n int) string {
	r := []rune(m.topic)
	if len(r) <= n {
		return m.topic
	}
	return string(r[:n])
}

func (m *meeting) moderator() *member {
	for _, mm := range m.members {
		if mm.isModerator {
			return mm
		}
	}
	return nil
}

// flatVotes 把新票型结构拍平成「方案/弃权 -> 票数」，供断言使用。
//
// 新结构：conclusion.votes = {"p_1":{"total":2,"self":1,"cross":1}, ...}
//
//	另保留扁平兼容字段 conclusion.voteTotals = {"p_1":2,"abstain":1}
//
// 优先用 voteTotals；没有则兼容旧的扁平 votes；再不行就取嵌套里的 total。
func flatVotes(res map[string]any) map[string]float64 {
	out := map[string]float64{}
	concl, _ := res["conclusion"].(map[string]any)
	if concl == nil {
		return out
	}
	if totals, ok := concl["voteTotals"].(map[string]any); ok {
		for k, v := range totals {
			if f, ok := v.(float64); ok {
				out[k] = f
			}
		}
		return out
	}
	if votes, ok := concl["votes"].(map[string]any); ok {
		for k, v := range votes {
			switch t := v.(type) {
			case float64:
				out[k] = t
			case map[string]any:
				if f, ok := t["total"].(float64); ok {
					out[k] = f
				}
			}
		}
	}
	return out
}

// voteTotals 是当前房间的扁平票数快照。
func (m *meeting) voteTotals() map[string]float64 {
	return flatVotes(m.result())
}

/* ── 读 ──────────────────────────────────────────────────────────────── */

func (m *meeting) inbox(mm *member) inboxState {
	m.t.Helper()
	r := call(m.t, m.srv.base, http.MethodGet, "/rooms/"+m.roomID+"/inbox?wait=0", mm.token, nil)
	if r.Status != 200 {
		m.t.Fatalf("读 inbox 失败 HTTP %d：%s", r.Status, r.snippet())
	}
	var st inboxState
	if err := json.Unmarshal(r.Body, &st); err != nil {
		m.t.Fatalf("inbox 不是 JSON：%v（%s）", err, r.snippet())
	}
	return st
}

// state 返回房间状态。0 人参会时没有 agent token，只能从公开的 result 读；
// 这里必须兜住，否则「0 人参会」这类用例会 panic 掉整个测试二进制，
// 把后面所有用例一并带走。
func (m *meeting) state() string {
	if len(m.members) == 0 {
		return m.stateOfRoom()
	}
	return m.inbox(m.members[0]).State
}

func (m *meeting) messages(mm *member) []msg {
	m.t.Helper()
	r := call(m.t, m.srv.base, http.MethodGet, "/rooms/"+m.roomID+"/messages?since=0", mm.token, nil)
	if r.Status != 200 {
		return nil
	}
	var out struct {
		Messages []msg `json:"messages"`
	}
	if json.Unmarshal(r.Body, &out) != nil {
		return nil
	}
	return out.Messages
}

// lastOtherSeq 取一条"别人的"消息 seq，用作 replyTo。
func (m *meeting) lastOtherSeq(mm *member) int {
	best := 0
	for _, x := range m.messages(mm) {
		if x.AgentID == mm.id || x.Alias == mm.alias {
			continue
		}
		if x.Seq > best {
			best = x.Seq
		}
	}
	return best
}

func (m *meeting) lastAnySeq() int {
	best := 0
	for _, x := range m.messages(m.members[0]) {
		if x.Seq > best {
			best = x.Seq
		}
	}
	return best
}

func (m *meeting) result() map[string]any {
	m.t.Helper()
	r := call(m.t, m.srv.base, http.MethodGet, "/rooms/"+m.roomID+"/result", "", nil)
	if r.Status != 200 {
		m.t.Fatalf("读 result 失败 HTTP %d：%s", r.Status, r.snippet())
	}
	var out map[string]any
	if err := json.Unmarshal(r.Body, &out); err != nil {
		m.t.Fatalf("result 不是 JSON：%v（%s）", err, r.snippet())
	}
	return out
}

/* ── 写 ──────────────────────────────────────────────────────────────── */

var opSeq int64

func newOpKey() string {
	opSeq++
	return fmt.Sprintf("mt-%d-%d", time.Now().UnixNano(), opSeq)
}

func (m *meeting) speak(mm *member, text string, replyTo int, supportProposalID string) resp {
	body := map[string]any{"text": text, "opKey": newOpKey()}
	if replyTo > 0 {
		body["replyTo"] = replyTo
	}
	if supportProposalID != "" {
		body["supportProposalId"] = supportProposalID
	}
	return call(m.t, m.srv.base, http.MethodPost, "/rooms/"+m.roomID+"/messages", mm.token, body)
}

func (m *meeting) propose(mm *member, title, body string) (string, resp) {
	r := call(m.t, m.srv.base, http.MethodPost, "/rooms/"+m.roomID+"/proposals", mm.token, map[string]any{
		"title": title, "body": body, "opKey": newOpKey(),
	})
	var out struct {
		ProposalID string `json:"proposalId"`
	}
	_ = json.Unmarshal(r.Body, &out)
	if out.ProposalID != "" {
		if m.props.title == nil {
			m.props.title = map[string]string{}
			m.props.owner = map[string]string{}
		}
		m.props.order = append(m.props.order, out.ProposalID)
		m.props.title[out.ProposalID] = title
		m.props.owner[out.ProposalID] = mm.alias
	}
	return out.ProposalID, r
}

// voteAvoidingSelf：夹具里刻意**回避自投**，只为了让"每张票都是跨票"，
// 断言票数分布时不用按人头拆分 self/cross。
//
// v2.2 起服务端**允许自投**（跨票主序，自投不单独构成多数），所以这里不再是
// 为了躲 403 —— 只想测自投形态的用例直接用 m.vote(mm, 自己提的方案) 即可。
func (m *meeting) voteAvoidingSelf(mm *member, proposalID string) resp {
	if proposalID != "" && m.props.owner[proposalID] == mm.alias {
		return m.vote(mm, "")
	}
	return m.vote(mm, proposalID)
}

// vote 投票；proposalID 传空字符串表示弃权（proposalId: null）。
func (m *meeting) vote(mm *member, proposalID string) resp {
	var pid any
	if proposalID != "" {
		pid = proposalID
	}
	return call(m.t, m.srv.base, http.MethodPost, "/rooms/"+m.roomID+"/votes", mm.token, map[string]any{
		"proposalId": pid, "reason": "测试投票", "opKey": newOpKey(),
	})
}

/* ── 等待与驱动 ──────────────────────────────────────────────────────── */

func (m *meeting) waitState(want string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if m.state() == want {
			return true
		}
		time.Sleep(60 * time.Millisecond)
	}
	return false
}

func (m *meeting) waitEnded(d time.Duration) bool { return m.waitState("ended", d) }
func (m *meeting) firstProposalID() string {
	if len(m.props.order) == 0 {
		return ""
	}
	return m.props.order[0]
}
func (m *meeting) firstProposalID2() string { return "" }
