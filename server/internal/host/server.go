package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"agchat/internal/domain"
	"agchat/internal/invite"
	"agchat/internal/oplog"
	"agchat/internal/room"
)

// routes 是宿主的全部路由（docs/09 §3）+ 本地控制面补的三条。
//
// 安全基线（arch A-24 / G13）：
//   - 只绑 127.0.0.1（--expose-lan 才 0.0.0.0）；
//   - 校验 Host 头（防 DNS rebinding）；
//   - **不写 access-control-allow-origin**（一个字节都不写）；
//   - 无凭证端点一律只读且不 tick；
//   - /v1/status 需要控制面 token；/healthz 开放但不含任何会议数据。
func (h *Host) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/room", h.hCtl(h.create))
	mux.HandleFunc("POST /v1/invite", h.hCtl(h.issue))
	mux.HandleFunc("GET /v1/card/{ref}", h.hCtl(h.card))
	mux.HandleFunc("POST /v1/close", h.hCtl(h.closeRoom))
	mux.HandleFunc("POST /v1/resume", h.hCtl(h.resume))
	mux.HandleFunc("GET /v1/status", h.hCtl(h.status))
	mux.HandleFunc("GET /v1/rooms", h.hCtl(h.roomsList))
	mux.HandleFunc("GET /v1/export", h.hCtl(h.export))
	mux.HandleFunc("POST /v1/join", h.join)
	mux.HandleFunc("GET /v1/head", h.head)
	mux.HandleFunc("GET /v1/next", h.next)
	mux.HandleFunc("POST /v1/post", h.agentOp(oplog.OpPost))
	mux.HandleFunc("POST /v1/propose", h.agentOp(oplog.OpPropose))
	mux.HandleFunc("POST /v1/vote", h.agentOp(oplog.OpVote))
	mux.HandleFunc("POST /v1/summary", h.agentOp(oplog.OpSummary))
	mux.HandleFunc("GET /v1/result", h.result)
	mux.HandleFunc("GET /healthz", h.healthz)
	return h.checkHost(mux)
}

// checkHost 校验 Host 头（只接受 loopback / --expose-lan 时的本机地址）。
func (h *Host) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		name, port, err := splitHostPort(host)
		if err != nil {
			writeErr(w, newOpErr(403, "bad_host", "Host 头不合法（本地宿主只接受 loopback 地址）"))
			return
		}
		allowed := map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}
		if h.cfg.ExposeLAN {
			for _, ip := range lanIPs() {
				allowed[ip] = true
			}
		}
		if !allowed[name] {
			writeErr(w, newOpErr(403, "bad_host", "Host 头不是本机地址，已拒绝（防 DNS rebinding）"))
			return
		}
		if want := portOf(h.Addr()); want != "" && port != "" && port != want {
			writeErr(w, newOpErr(403, "bad_host", "Host 头的端口与监听端口不一致，已拒绝"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func splitHostPort(host string) (name, port string, err error) {
	if i := strings.LastIndex(host, ":"); i >= 0 {
		name, port = host[:i], host[i+1:]
		name = strings.Trim(name, "[]")
		if name == "" || port == "" {
			return "", "", fmt.Errorf("bad host %q", host)
		}
		if _, e := strconv.Atoi(port); e != nil {
			return "", "", fmt.Errorf("bad port %q", port)
		}
		return name, port, nil
	}
	if host == "" {
		return "", "", fmt.Errorf("empty host")
	}
	return strings.Trim(host, "[]"), "", nil
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

var bufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// ---- 统一响应 --------------------------------------------------------------

func writeJSONResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.Header().Set("x-content-type-options", "nosniff")
	// ★ 故意不写 access-control-allow-origin（arch A-24）。
	w.WriteHeader(status)
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err == nil {
		_, _ = w.Write(buf.Bytes())
	}
	bufPool.Put(buf)
}

func writeErr(w http.ResponseWriter, err error) {
	oe, ok := err.(*opError)
	if !ok {
		oe = &opError{Status: 500, Code: "internal", Msg: err.Error()}
	}
	if oe.Status == 0 {
		oe.Status = 500
	}
	body := map[string]any{
		"ok":    false,
		"error": map[string]any{"code": oe.Code, "message": oe.Msg},
	}
	if oe.Detail != nil {
		body["detail"] = oe.Detail
		if human, ok := oe.Detail["human"].(string); ok && human != "" {
			body["human"] = human
		}
	}
	writeJSONResp(w, oe.Status, body)
}

// hCtl 包一层控制面 token 校验（Authorization: Bearer <control.token>）。
func (h *Host) hCtl(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.isCtl(r) {
			writeErr(w, newOpErr(401, "ctl_token_required", "控制面端点需要 ~/.agchat/control.token（本机文件，0600）"))
			return
		}
		if err := fn(w, r); err != nil {
			writeErr(w, err)
		}
	}
}

func (h *Host) isCtl(r *http.Request) bool {
	tok := bearer(r)
	return tok != "" && subtleEqual(tok, h.ctlToken)
}

func bearer(r *http.Request) string {
	v := strings.TrimSpace(r.Header.Get("authorization"))
	if len(v) >= 7 && strings.EqualFold(v[:7], "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// ---- 控制面 handler --------------------------------------------------------

func (h *Host) create(w http.ResponseWriter, r *http.Request) error {
	var req CreateReq
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	res, err := h.Create(r.Context(), req)
	if err != nil {
		return toOpErr(err)
	}
	writeJSONResp(w, 200, res)
	return nil
}

func (h *Host) issue(w http.ResponseWriter, r *http.Request) error {
	var req IssueReq
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.Ref) == "" {
		return newOpErr(400, "ref_required", "缺 ref")
	}
	res, err := h.Issue(r.Context(), req)
	if err != nil {
		return toOpErr(err)
	}
	writeJSONResp(w, 200, res)
	return nil
}

func (h *Host) card(w http.ResponseWriter, r *http.Request) error {
	ref := r.PathValue("ref")
	card, err := h.Card(ref)
	if err != nil {
		return err
	}
	writeJSONResp(w, 200, map[string]any{"ref": ref, "card": card})
	return nil
}

func (h *Host) closeRoom(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Ref string `json:"ref"`
	}
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	res, err := h.CloseRoom(r.Context(), req.Ref)
	if err != nil {
		return toOpErr(err)
	}
	writeJSONResp(w, 200, res)
	return nil
}

func (h *Host) resume(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Ref string `json:"ref"`
	}
	if err := decodeBody(r, &req); err != nil {
		return err
	}
	if h.roomByRef(req.Ref) == nil {
		if err := h.openRoom(r.Context(), req.Ref); err != nil {
			return &opError{Status: 404, Code: "room_not_found", Msg: "这个 home 里没有这场会: " + err.Error()}
		}
		_ = writeRendezvous(h.cfg, req.Ref, h.Addr(), h.startedAt)
	}
	st, err := h.Status(r.Context(), req.Ref)
	if err != nil {
		return toOpErr(err)
	}
	writeJSONResp(w, 200, map[string]any{
		"ref": st.Ref, "pid": pid(), "addr": h.Addr(), "hostFile": h.cfg.roomJSONPath(req.Ref),
		"state": st.State, "resumed": true,
	})
	return nil
}

func (h *Host) status(w http.ResponseWriter, r *http.Request) error {
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		return newOpErr(400, "ref_required", "缺 ref（多房间时 status 必须指定房间）")
	}
	res, err := h.Status(r.Context(), ref)
	if err != nil {
		return toOpErr(err)
	}
	writeJSONResp(w, 200, res)
	return nil
}

func (h *Host) roomsList(w http.ResponseWriter, r *http.Request) error {
	writeJSONResp(w, 200, map[string]any{"rooms": h.ListRooms(r.Context()), "pid": pid(), "addr": h.Addr()})
	return nil
}

func (h *Host) export(w http.ResponseWriter, r *http.Request) error {
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		return newOpErr(400, "ref_required", "缺 ref")
	}
	data, err := h.Export(r.Context(), ref)
	if err != nil {
		return toOpErr(err)
	}
	w.Header().Set("content-type", "application/zip")
	w.Header().Set("cache-control", "no-store")
	w.Header().Set("content-length", strconv.Itoa(len(data)))
	// 同样不写 access-control-allow-origin。
	w.WriteHeader(200)
	_, _ = w.Write(data)
	return nil
}

// ---- 参与者 handler --------------------------------------------------------

func (h *Host) join(w http.ResponseWriter, r *http.Request) {
	var req JoinReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	res, err := h.Join(r.Context(), req)
	if err != nil {
		writeErr(w, toOpErr(err))
		return
	}
	writeJSONResp(w, 200, res)
}

func (h *Host) head(w http.ResponseWriter, r *http.Request) {
	rc, s, err := h.authByToken(bearer(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	if ok, reason := admit(s.Caps, invite.CapRead); !ok {
		writeErr(w, newOpErr(403, "capability_denied", reason))
		return
	}
	// 客户端要拿 state/round/seq 一起签名（post 载荷要带 round 与 seq）。
	// 这些都在内存快照里：head 是每次发 op 前的必经调用，不该为它读一次整库。
	rc.mu.Lock()
	snap := rc.snap
	head := HeadView{I: rc.log.count(), H: rc.log.head()}
	rc.mu.Unlock()
	writeJSONResp(w, 200, map[string]any{
		"ref": rc.ref, "head": head, "agentId": s.AgentID,
		"state": snap.State, "round": snap.Round, "seq": snap.Seq,
	})
}

func (h *Host) next(w http.ResponseWriter, r *http.Request) {
	waitSec := 20
	if v := r.URL.Query().Get("wait"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			waitSec = n
		}
	}
	if waitSec > int(room.MaxWaitSeconds) {
		waitSec = int(room.MaxWaitSeconds)
	}
	if waitSec < 0 {
		waitSec = 0
	}
	res, err := h.Next(r.Context(), bearer(r), time.Duration(waitSec)*time.Second)
	if err != nil {
		writeErr(w, toOpErr(err))
		return
	}
	writeJSONResp(w, 200, res)
}

func (h *Host) agentOp(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req AgentOpReq
		if err := decodeBody(r, &req); err != nil {
			writeErr(w, err)
			return
		}
		res, ack, err := h.applyAgentOp(r.Context(), kind, bearer(r), req)
		if err != nil {
			writeErr(w, toOpErr(err))
			return
		}
		out := map[string]any{"ok": true, "result": res, "head": HeadView{I: req.Op.I, H: req.Op.H}}
		if ack != nil {
			out["ack"] = ack
			out["duplicate"] = false
		} else {
			out["duplicate"] = true
		}
		writeJSONResp(w, 200, out)
	}
}

func (h *Host) result(w http.ResponseWriter, r *http.Request) {
	res, err := h.Result(r.Context(), bearer(r))
	if err != nil {
		writeErr(w, toOpErr(err))
		return
	}
	writeJSONResp(w, 200, res)
}

func (h *Host) healthz(w http.ResponseWriter, r *http.Request) {
	// 不含任何会议数据（G13）：只报存活与版本。
	writeJSONResp(w, 200, map[string]any{"ok": true, "version": h.cfg.Version})
}

// ---- 工具 ------------------------------------------------------------------

func decodeBody(r *http.Request, v any) error {
	defer func() { _, _ = r.Body.Close(), 0 }()
	const maxBody = 4 << 20
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	if err := dec.Decode(v); err != nil {
		// 超长和"不是 JSON"要分开：前者删几个字段就行，后者是写错了，
		// 混成一个 400 会让人往错的方向查。
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return newOpErr(413, "body_too_large", fmt.Sprintf("请求体超过 %d 字节上限", maxBody))
		}
		return newOpErr(400, "bad_json", "请求体不是合法 JSON: "+err.Error())
	}
	return nil
}

func pid() int { return osGetpid() }

// NextResp 是 GET /v1/next 的响应（字段与 CLI --json 契约一致）。
type NextResp struct {
	State                 domain.State        `json:"state"`
	Round                 int                 `json:"round"`
	RoundsTotal           int                 `json:"roundsTotal"`
	YourTurn              bool                `json:"yourTurn"`
	YourTurnDeadlineInSec *int                `json:"yourTurnDeadlineInSec"`
	Budget                map[string]int      `json:"budget"`
	Unread                int                 `json:"unread"`
	MentionsYou           int                 `json:"mentionsYou"`
	Proposals             []room.ProposalView `json:"proposals"`
	MyVote                *string             `json:"myVote"`
	YouAreWinner          bool                `json:"youAreWinner"`
	Roster                []room.RosterItem   `json:"roster"`
	Notes                 []string            `json:"notes"`
	TimedOut              bool                `json:"timedOut"`
	IsModerator           bool                `json:"isModerator"`
	ReadCursor            int                 `json:"readCursor"`
	AgentID               string              `json:"agentId"`
	Ref                   string              `json:"ref"`
	Head                  HeadView            `json:"head"`
}

// Next 是长轮询：引擎现成的 Inbox 负责等待（不自旋、不持数据库连接）。
func (h *Host) Next(ctx context.Context, token string, wait time.Duration) (*NextResp, error) {
	rc, s, err := h.authByToken(token)
	if err != nil {
		return nil, err
	}
	if ok, reason := admit(s.Caps, invite.CapRead); !ok {
		return nil, newOpErr(403, "capability_denied", reason)
	}
	before, err := h.peek(ctx, rc)
	if err != nil {
		return nil, err
	}
	// 长轮询的游标必须是**上次给出去的那个事件序号**。传 nil 时引擎会立刻返回
	// （ops.go:410），wait 等于白给，agent 只能自己空转重试。
	rc.mu.Lock()
	var cur int64
	if sp := rc.seats[s.AgentID]; sp != nil {
		cur = sp.Cursor
	}
	rc.mu.Unlock()
	snap, err := h.eng.Inbox(ctx, TenantID, rc.roomID, tokenHashOf(s), &cur, wait)
	if err != nil {
		return nil, err
	}
	rc.mu.Lock()
	if sp := rc.seats[s.AgentID]; sp != nil && snap.Cursor > sp.Cursor {
		sp.Cursor = snap.Cursor
		rc.dirty = true
	}
	err = h.syncLocked(ctx, rc, before)
	head := HeadView{I: rc.log.count(), H: rc.log.head()}
	if rc.dirty {
		h.persist(rc)
	}
	rc.mu.Unlock()
	if err != nil {
		return nil, err
	}
	led := snap.Budget
	proposals := snap.Proposals
	if proposals == nil {
		proposals = []room.ProposalView{}
	}
	roster := snap.Roster
	notes := snap.Notes
	if notes == nil {
		notes = []string{}
	}
	return &NextResp{
		State: snap.State, Round: snap.Round, RoundsTotal: snap.RoundsTotal,
		YourTurn: snap.YourTurn, YourTurnDeadlineInSec: snap.YourTurnDeadlineInSec,
		Budget: map[string]int{"tokenBudget": led.Total, "used": led.Used, "remaining": led.Left, "pct": led.Pct},
		Unread: snap.Unread, MentionsYou: snap.MentionsYou,
		Proposals: proposals, MyVote: snap.MyVote, YouAreWinner: snap.YouAreWinner,
		Roster: roster, Notes: notes, TimedOut: snap.TimedOut,
		IsModerator: snap.IsModerator, ReadCursor: snap.ReadCursor,
		AgentID: s.AgentID, Ref: rc.ref, Head: head,
	}, nil
}

var _ = json.Marshal
