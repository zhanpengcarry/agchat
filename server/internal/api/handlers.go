package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"agchat/internal/config"
	"agchat/internal/domain"
	"agchat/internal/room"
	"agchat/internal/store"
)

const maxBodyBytes = 64 << 10

// Server 聚合了处理请求所需的一切。
type Server struct {
	engine *room.Engine
	store  *store.Store
	cfg    config.Config
	docs   docs
	rl     *rateLimiter
	ip     *IPResolver
}

// New 造一个 Server。
func New(engine *room.Engine, st *store.Store, cfg config.Config, d docs) *Server {
	return &Server{
		engine: engine, store: st, cfg: cfg, docs: d,
		rl: newRateLimiter(),
		ip: NewIPResolver(cfg.TrustedProxies),
	}
}

// sourceIP 返回 (来源 IP, TCP 对端 IP)。限流键与审计日志都用解析后的来源。
func (s *Server) sourceIP(r *http.Request) (string, string) { return s.ip.Source(r) }

// =============================================================================
// 输出
// =============================================================================

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeErr 把业务错误写成统一形状：{"error":{"code":"...","message":"..."}}
//
// extra 里的内容会被摊进 error 对象（例如 409 new_messages 要带 messages）。
func writeErr(w http.ResponseWriter, err error) {
	var de *domain.Error
	if errors.As(err, &de) {
		body := map[string]any{"code": de.Code, "message": de.Message}
		for k, v := range de.Extra {
			body[k] = v
		}
		writeJSON(w, de.Status, map[string]any{"error": body})
		return
	}
	if errors.Is(err, domain.ErrNotFound) {
		writeJSON(w, 404, map[string]any{"error": map[string]string{
			"code": domain.ErrNotFound.Code, "message": domain.ErrNotFound.Message,
		}})
		return
	}
	writeJSON(w, 500, map[string]any{"error": map[string]string{
		"code": "internal_error", "message": err.Error(),
	}})
}

func decodeJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // 空体当 {} 处理
		}
		return domain.NewError(400, "invalid_json", "请求体不是合法 JSON")
	}
	return nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("authorization")
	const p = "bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

func hostOf(r *http.Request) string {
	if h := r.Host; h != "" {
		return h
	}
	return "127.0.0.1:8788"
}

// resolveRoom 把 roomId 翻译成 tenantId。
//
// ★ 存在性计时侧信道（本函数的核心）：
//
//	房间不存在 → 这里就返回；房间存在但凭证不属于它 → 后面还要开事务读
//	rooms/agents/messages 三张表。两条路径的 DB 往返次数不同（1 次 vs 5 次），
//	实测差 1.5–1.8 倍 —— 足够采样就能统计出"这个房间号到底存不存在"。
//	404 挡得住内容，挡不住"这里有没有东西"，而存在性本身就是要保护的数据。
//
//	所以查不到时**补做等价的哑查询**（store.DummyScopedLoad：一个事务 + 同样三条
//	索引探测），让两条路径做完全相同的功。这不是 time.Sleep —— 塞 sleep 只是
//	假装"在这台机器上差不多快"，换机器/换 SQLite 版本比例就飘了；真做同样的功，
//	比例天然是 1。
//
// resolveRoom 把 URL 上的 {key}（9 位会议号或 roomId）翻译成 (tenantId, roomId)。
//
// ★ 存在性计时侧信道（本函数的核心）：
//
//	会议号不存在 → 这里就返回；会议存在但凭证不属于它 → 后面还要开事务读
//	rooms/agents/messages 三张表。两条路径的 DB 往返次数不同（1 次 vs 5 次），
//	实测差 1.5–1.8 倍 —— 足够采样就能统计出"这个会议号到底存不存在"。
//	404 挡得住内容，挡不住"这里有没有东西"，而存在性本身就是要保护的数据。
//
//	所以查不到时**补做等价的哑查询**（store.DummyScopedLoad：一个事务 + 同样三条
//	索引探测），让两条路径做完全相同的功。这不是 time.Sleep —— 塞 sleep 只是
//	假装"在这台机器上差不多快"，换机器/换 SQLite 版本比例就飘了；真做同样的功，
//	比例天然是 1。
func (s *Server) resolveRoom(r *http.Request, key string) (string, string, error) {
	if key == "" {
		return "", "", domain.ErrNotFound
	}
	tenantID, roomID, err := s.store.ResolveRoomKey(r.Context(), key)
	if err != nil {
		// 补平数据库工作量，再做与"存在但无权"完全相同的审计查询
		_ = s.store.DummyScopedLoad(r.Context(), key)
		s.auditReject(r, key, "")
		return "", "", domain.ErrNotFound
	}
	return tenantID, roomID, nil
}

// writeEngineErr 统一处理引擎错误；404 时先写审计日志再回响应。
//
// 这里的 404 有两种来源，必须走同一段代码，否则又会出现"哪条路径多干了一件事"的偏差：
//   - 房间存在，但 token 不属于这个 (租户, 房间, agent) 三元组
//   - close 时 adminKey 与房间不匹配
func (s *Server) writeEngineErr(w http.ResponseWriter, r *http.Request, roomID, tenantID string, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		s.auditReject(r, roomID, tenantID)
	}
	writeErr(w, err)
}

// enumGuard 对"无需凭证就能问出房间存不存在"的接口做按 IP 限流。
//
// ★ 只装在真正的枚举口上：GET /api/rooms/{id}、/api/rooms/{id}/result、GET /room/{id}。
//
//	这三个不需要任何 token，房间存在回 200、不存在回 404 —— 是干净的 oracle。
//	/inbox 与 /messages 必须带 token，两种失败都是 404，本来就不泄漏存在性，
//	给它们限流只会误伤同一出口 IP 后面的多个 agent 心跳。
//
// ★ 必须在任何存在性判断**之前**执行。若放在查库之后，429 本身就成了
//
//	"这个房间存在"的信号，等于换个方式泄漏同一件事。
//	实测 600 次探测 0.15 秒就能跑完 —— 404 挡内容，但挡不住扫描。
func (s *Server) enumGuard(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.EnumRate <= 0 {
		return true
	}
	src, _ := s.sourceIP(r)
	if s.rl.allow("enum|"+src, s.cfg.EnumRate, time.Minute) {
		return true
	}
	w.Header().Set("retry-after", "60")
	writeErr(w, domain.NewError(429, "rate_limited", "请求太频繁：房间查询按 IP 限流"))
	return false
}

// auditReject 记录一次被拒的凭证尝试。
//
// roomTenant 为空表示"房间本身不存在"；非空表示"房间存在但这次凭证不匹配"。
// 两种情况的日志字段一致、DB 查询次数一致，所以不会重新引入计时差。
//
// ★ 绝不记录 token 原文，只记它的 sha256 前 8 位。
func (s *Server) auditReject(r *http.Request, roomID, roomTenant string) {
	tok := bearerToken(r)
	if tok == "" {
		return
	}
	hash := domain.HashToken(tok)
	prefix := hash
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	ip, peer := s.sourceIP(r)
	// 与"存在但无权"路径相同的一次索引查询：只读 token_directory 的归属映射
	ref, found, err := s.store.LookupTokenTenant(r.Context(), hash)
	if err != nil {
		slog.Warn("agent_auth_rejected", "verdict", "lookup_failed", "ip", ip, "peer_ip", peer,
			"path", r.URL.Path, "room_id", roomID, "token_hash_prefix", prefix, "err", err.Error())
		return
	}

	verdict := "unknown_token"
	if found {
		switch {
		case roomTenant == "":
			verdict = "unknown_room_token_known" // 房间不存在，但这枚 token 是真的
		case ref.TenantID != roomTenant:
			verdict = "cross_tenant_attempt" // ← 重点：拿别的租户的 token 打这个房间
		case ref.RoomID != roomID:
			verdict = "cross_room_attempt"
		default:
			verdict = "scope_inconsistent" // 同租户同房间却不匹配，属异常，需要人工看
		}
	}

	attrs := []any{
		"verdict", verdict,
		"ip", ip,
		"peer_ip", peer,
		"method", r.Method,
		"path", r.URL.Path,
		"room_id", roomID,
		"room_exists", roomTenant != "",
		"token_hash_prefix", prefix,
	}
	if roomTenant == "" {
		attrs = append(attrs, "room_tenant", nil)
	} else {
		attrs = append(attrs, "room_tenant", roomTenant)
	}
	if found {
		attrs = append(attrs, "token_tenant", ref.TenantID, "token_room", ref.RoomID)
	} else {
		attrs = append(attrs, "token_tenant", nil, "token_room", nil)
	}
	slog.Warn("agent_auth_rejected", attrs...)
}

// agentTokenHash 取 Bearer token 的哈希。
//
// 注意：这里只校验"有没有 token"，token 属不属于这个租户/房间/agent
// 由引擎在租户事务内用三元组确认；不匹配一律 404。
func agentTokenHash(r *http.Request) (string, error) {
	tok := bearerToken(r)
	if tok == "" {
		return "", domain.NewError(401, "unauthorized", "缺少 Authorization: Bearer <agentToken>")
	}
	return domain.HashToken(tok), nil
}

// =============================================================================
// 限流（进程内滑动窗口，够 MVP 用）
// =============================================================================

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string][]int64
}

func newRateLimiter() *rateLimiter { return &rateLimiter{buckets: map[string][]int64{}} }

func (r *rateLimiter) allow(key string, max int, window time.Duration) bool {
	if max <= 0 {
		return true
	}
	now := time.Now().UnixMilli()
	cutoff := now - window.Milliseconds()
	r.mu.Lock()
	defer r.mu.Unlock()
	arr := r.buckets[key]
	kept := arr[:0]
	for _, t := range arr {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		r.buckets[key] = kept
		return false
	}
	r.buckets[key] = append(kept, now)
	return true
}

// =============================================================================
// 端点
// =============================================================================

// HandleCreateRoom POST /api/rooms —— 人类建房间。无鉴权，但限流。
//
// 创建者拿到的 adminKey 只能用来 close，**不能发言**：
// 这个产品里人类没有任何发言通道，这是定义不是遗漏。
func (s *Server) HandleCreateRoom(w http.ResponseWriter, r *http.Request) {
	src, _ := s.sourceIP(r)
	if !s.rl.allow("create|"+src, s.cfg.RoomRate, 10*time.Minute) {
		writeErr(w, domain.NewError(429, "room_rate_limited", "建房间太频繁，请稍后再试"))
		return
	}
	var in room.CreateInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}

	tenantID, err := s.store.NewTenant(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.engine.Create(r.Context(), tenantID, in)
	if err != nil {
		writeErr(w, err)
		return
	}
	host := hostOf(r)
	writeJSON(w, 201, map[string]any{
		"roomId":   res.Room.ID,
		"roomNo":   room.FormatRoomNo(res.Room.RoomNo),
		"roomUrl":  "http://" + host + "/room/" + res.Room.RoomNo,
		"password": res.Password, // 只出现这一次
		"adminKey": res.AdminKey, // 只出现这一次
		"tenantId": tenantID,
		"estimate": res.Estimate,
	})
}

// HandleJoin POST /api/rooms/{roomId}/join
func (s *Server) HandleJoin(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	tenantID, roomID, err := s.resolveRoom(r, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	src, _ := s.sourceIP(r)
	if !s.rl.allow("join|"+roomID+"|"+src, s.cfg.JoinRate, time.Minute) {
		writeErr(w, domain.NewError(429, "join_rate_limited", "加入请求太频繁"))
		return
	}
	var in room.JoinInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.engine.Join(r.Context(), tenantID, roomID, in, hostOf(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"agentToken":    res.AgentToken,
		"agentId":       res.AgentID,
		"isModerator":   res.IsModerator,
		"speakingOrder": res.SpeakingOrder,
		"reclaimed":     res.Reclaimed, // true = 这是"认回"，agentId 不变、不占新座位
		"brief":         res.Brief,
	})
}

// HandlePropose POST /api/rooms/{key}/proposals —— 提交方案（每人最多 2 个）。
func (s *Server) HandlePropose(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	tokenHash, err := agentTokenHash(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	tenantID, roomID, err := s.resolveRoom(r, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in room.ProposeInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.engine.Propose(r.Context(), tenantID, roomID, tokenHash, in)
	if err != nil {
		s.writeEngineErr(w, r, roomID, tenantID, err)
		return
	}
	writeJSON(w, 201, res)
}

// HandleVote POST /api/rooms/{key}/votes —— 投票（每人 1 票，可改）。
func (s *Server) HandleVote(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	tokenHash, err := agentTokenHash(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	tenantID, roomID, err := s.resolveRoom(r, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in room.VoteInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.engine.Vote(r.Context(), tenantID, roomID, tokenHash, in)
	if err != nil {
		s.writeEngineErr(w, r, roomID, tenantID, err)
		return
	}
	writeJSON(w, 201, res)
}

// HandleSummary POST /api/rooms/{key}/summary —— 获胜者撰写概述。
func (s *Server) HandleSummary(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	tokenHash, err := agentTokenHash(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	tenantID, roomID, err := s.resolveRoom(r, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in room.SummaryInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.engine.Summary(r.Context(), tenantID, roomID, tokenHash, in)
	if err != nil {
		s.writeEngineErr(w, r, roomID, tenantID, err)
		return
	}
	writeJSON(w, 200, res)
}

// HandleInbox GET /api/rooms/{roomId}/inbox
//
// 注意这里**不做枚举限流**：inbox 不是存在性 oracle ——
// 没带 token 时"房间存在/不存在"都是 401；带了 token 时两种失败都是 404。
// 反过来给它限流会误伤同一出口 IP 后面的多个 agent 心跳。
func (s *Server) HandleInbox(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	// 先判"有没有带 token"，再判会议在不在：
	// 反过来的话，无 token 时"存在→401 / 不存在→404"本身就是个枚举口子。
	tokenHash, err := agentTokenHash(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	tenantID, roomID, err := s.resolveRoom(r, key)
	if err != nil {
		writeErr(w, err)
		return
	}

	q := r.URL.Query()
	var since *int64
	if q.Has("since") {
		v, _ := strconv.ParseInt(q.Get("since"), 10, 64)
		since = &v
	}
	waitSec, _ := strconv.Atoi(q.Get("wait"))
	if waitSec < 0 {
		waitSec = 0
	}
	if waitSec > room.MaxWaitSeconds {
		waitSec = room.MaxWaitSeconds
	}

	snap, err := s.engine.Inbox(r.Context(), tenantID, roomID, tokenHash, since, time.Duration(waitSec)*time.Second)
	if err != nil {
		s.writeEngineErr(w, r, roomID, tenantID, err)
		return
	}
	writeJSON(w, 200, snap)
}

// HandleGetMessages GET /api/rooms/{roomId}/messages —— 读正文，计费。
func (s *Server) HandleGetMessages(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	tokenHash, err := agentTokenHash(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	tenantID, roomID, err := s.resolveRoom(r, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	// 显式开关：只回别人的消息（省 agent 宿主的上下文）。默认关，保持 /messages 是完整转写稿。
	excludeSelf := r.URL.Query().Get("excludeSelf") == "1" || r.URL.Query().Get("excludeSelf") == "true"

	res, err := s.engine.Read(r.Context(), tenantID, roomID, tokenHash, since, excludeSelf)
	if err != nil {
		s.writeEngineErr(w, r, roomID, tenantID, err)
		return
	}
	writeJSON(w, 200, res)
}

// HandlePostMessage POST /api/rooms/{roomId}/messages —— 发言 / 收敛轮表态。
func (s *Server) HandlePostMessage(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	tokenHash, err := agentTokenHash(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	tenantID, roomID, err := s.resolveRoom(r, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	var in room.PostInput
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	res, err := s.engine.Post(r.Context(), tenantID, roomID, tokenHash, in)
	if err != nil {
		s.writeEngineErr(w, r, roomID, tenantID, err)
		return
	}
	writeJSON(w, 201, res)
}

// HandleClose POST /api/rooms/{roomId}/close —— 人类唯一的写操作。
func (s *Server) HandleClose(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	tenantID, roomID, err := s.resolveRoom(r, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		AdminKey string `json:"adminKey"`
	}
	_ = decodeJSON(r, &body)
	adminKey := bearerToken(r)
	if adminKey == "" {
		adminKey = body.AdminKey
	}
	hash := ""
	if adminKey != "" {
		hash = domain.HashToken(adminKey)
	}
	rm, err := s.engine.Close(r.Context(), tenantID, roomID, hash)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// adminKey 对不上：同样是"存在但无权"，走同一段审计
			ip, peer := s.sourceIP(r)
			slog.Warn("admin_close_rejected",
				"ip", ip, "peer_ip", peer, "method", r.Method, "path", r.URL.Path,
				"room_id", roomID, "room_tenant", tenantID, "verdict", "admin_key_mismatch")
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "state": rm.State, "roomId": rm.ID})
}

// HandleRoomMeta GET /api/rooms/{roomId} —— 公开元信息（无 token）。
func (s *Server) HandleRoomMeta(w http.ResponseWriter, r *http.Request) {
	if !s.enumGuard(w, r) {
		return
	}
	key := r.PathValue("key")
	tenantID, roomID, err := s.resolveRoom(r, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	var payload map[string]any
	err = s.engine.View(r.Context(), tenantID, roomID, func(agg *room.Agg) error {
		rm := agg.Room
		agents := make([]map[string]any, 0, len(agg.Agents))
		for _, a := range agg.Agents {
			agents = append(agents, map[string]any{
				"agentId": a.ID, "alias": a.Alias, "model": a.Model,
				"hasFinalStance": a.FinalStance != "",
			})
		}
		payload = map[string]any{
			"roomId":       rm.ID,
			"topic":        rm.Topic,
			"state":        rm.State,
			"round":        rm.Round,
			"roundsTotal":  rm.MaxRounds,
			"createdAt":    isoUTC(rm.CreatedAt),
			"startedAt":    isoUTCorNil(rm.StartedAt),
			"endedAt":      isoUTCorNil(rm.EndedAt),
			"agents":       agents,
			"budget":       budgetOf(rm),
			"messageCount": len(agg.Messages),
		}
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, payload)
}

// HandleResult GET /api/rooms/{roomId}/result —— 结论 JSON（公开）。
func (s *Server) HandleResult(w http.ResponseWriter, r *http.Request) {
	// ★ /result 不计入枚举限流。
	//
	// 它不是"猜房间"的探针，而是**已知会议号才能调**的读取口：
	// 参会 agent 拿它看进度、管理者拿它取结论，skill 也让人读它。
	// 把它和 /api/rooms/{id} 一起按 IP 计数，会让同一出口 IP 后面的多个 agent、
	// 或一个人反复刷新，直接被 429 挡在门外 —— 挡住的正是正常用法。
	// 枚举风险由 /api/rooms/{id} 和结果页承担，那里仍然限流。
	key := r.PathValue("key")
	tenantID, roomID, err := s.resolveRoom(r, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	var res *room.Result
	err = s.engine.View(r.Context(), tenantID, roomID, func(agg *room.Agg) error {
		res = s.engine.ResultFor(agg)
		return nil
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	// ownerReport 需要对外域名，只能在 HTTP 层拼
	res.OwnerReport = room.OwnerReportOf(res, "http://"+hostOf(r)+"/room/"+key)
	writeJSON(w, 200, res)
}

// HandlePage GET /room/{roomId} —— 只读结果页（给人看）。
//
// 这个页面同样是存在性 oracle（房间在→200，不在→404），所以和 API 一起限流：
// 只堵 API 不堵 HTML 页，等于留了一扇没锁的侧门。
func (s *Server) HandlePage(w http.ResponseWriter, r *http.Request) {
	if !s.enumGuard(w, r) {
		return
	}
	key := r.PathValue("key")
	tenantID, roomID, err := s.resolveRoom(r, key)
	if err != nil {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.WriteHeader(404)
		_, _ = w.Write(renderMissing(roomID))
		return
	}
	var html []byte
	err = s.engine.View(r.Context(), tenantID, roomID, func(agg *room.Agg) error {
		res := s.engine.ResultFor(agg)
		var e error
		html, e = renderPage(res, agg.Room.ID, agg.Room.MaxRounds)
		return e
	})
	if err != nil {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		w.WriteHeader(404)
		_, _ = w.Write(renderMissing(roomID))
		return
	}
	w.Header().Set("content-type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write(html)
}

// HandleSkillMD GET /skill.md —— 把 skill 原文吐给 agent。
//
// 内容来自 -skill 指定的文件，**不在二进制里复制一份**：
// 那份文本是给 AI 的规范，只能有一个来源，复制就会分叉。
func (s *Server) HandleSkillMD(w http.ResponseWriter, r *http.Request) {
	writeMarkdown(w, s.docs.skill)
}

// HandleInstallMD GET /install.md —— 一页式接入说明。
func (s *Server) HandleInstallMD(w http.ResponseWriter, r *http.Request) {
	writeMarkdown(w, s.docs.join)
}

func writeMarkdown(w http.ResponseWriter, body []byte) {
	w.Header().Set("content-type", "text/markdown; charset=utf-8")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

// HandleHealthz GET /healthz
func (s *Server) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeJSON(w, 503, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// =============================================================================
// 小工具
// =============================================================================

func isoUTC(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

func isoUTCorNil(ms int64) any {
	if ms <= 0 {
		return nil
	}
	return isoUTC(ms)
}

func budgetOf(rm *domain.Room) map[string]int {
	left := rm.TokenBudget - rm.BudgetUsed
	if left < 0 {
		left = 0
	}
	pct := 0
	if rm.TokenBudget > 0 {
		pct = int(float64(rm.BudgetUsed)/float64(rm.TokenBudget)*100 + 0.5)
	}
	return map[string]int{"used": rm.BudgetUsed, "total": rm.TokenBudget, "left": left, "pct": pct}
}
