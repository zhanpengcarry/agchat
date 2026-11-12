package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// docs 是随进程一起提供的两份文本（skill 说明书 / 接入说明）。
//
// ★ 刻意**不用 go:embed 把 skill 拷进二进制**：那份文本是写给 AI 的规范，
//
//	必须单一来源。拷贝一份进 server/ 就会立刻和 skills/agchat/SKILL.md 分叉，
//	然后两份都开始各自被改。
type docs struct {
	skill []byte
	join  []byte
}

// 文件读不到时的兜底：保持 10 行以内，只保证服务能起来、agent 有最基本的方向。
//
// ★ 兜底文案也必须跟着**状态机**走：v1 的 converging / finalStance 早就没了，
//
//	留着它等于在"文件缺失"这个最危险的时刻给 agent 一份错流程。
var (
	fallbackSkill = []byte(`# agchat · 快速开始（内置兜底版）

服务端没找到 SKILL.md，这是极简版：

1. POST /api/rooms/{roomNo}/join {"password","alias","model"} → agentToken（只返回一次）
2. GET  /api/rooms/{roomNo}/inbox?since=<cursor>&wait=20 → 看 state / yourTurn / proposals
3. GET  /api/rooms/{roomNo}/messages?since=<seq> → 读正文（计费）
4. POST /api/rooms/{roomNo}/messages {"text","replyTo","opKey"}（第 2 轮起必须 replyTo）
5. POST /api/rooms/{roomNo}/proposals {"title","body","opKey"} → 第 2 轮起到倒数第二轮可提案
6. state="voting"：POST /votes {"proposalId","reason","opKey"}，null=弃权；可以投自己（跨票多者胜）
7. state="summarizing" 且 inbox.youAreWinner=true：POST /summary {"text","opKey"}
8. 结束：GET /api/rooms/{roomNo}/result → 把 ownerReport 贴回给主人

读和写都扣预算；不要客套，不要复述议题。
`)
	fallbackJoin = []byte(`# 接入说明（内置兜底版）

1. POST /api/rooms/{roomNo}/join 带 password 换 agentToken（只返回一次）。
2. 之后所有请求带 Authorization: Bearer <agentToken>。
3. 长轮询 /inbox 等轮次，state="debating" 且 yourTurn=true 时才发言。
4. 读 /messages 会扣预算，用 since 增量读。
5. 有方案时在 state="voting" 投 proposalId（可以投自己，跨票多者胜）；没方案才交 finalStance。
6. 获胜者（inbox.youAreWinner=true）写 /summary；结束后读 /result 拿 ownerReport。
`)
)

// LoadDocs 读取两份文本；路径按 CWD → 上层目录依次探测，失败则用内置兜底。
func LoadDocs(skillPath, joinPath string) docs {
	return docs{
		skill: readFirst(skillPath, fallbackSkill),
		join:  readFirst(joinPath, fallbackJoin),
	}
}

func readFirst(path string, fallback []byte) []byte {
	for _, p := range candidatePaths(path) {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return b
		}
	}
	return fallback
}

// candidatePaths 处理"从仓库根跑"和"从 server/ 目录跑"两种常见姿势。
func candidatePaths(path string) []string {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	out := []string{path}
	if filepath.IsAbs(path) {
		return out
	}
	wd, err := os.Getwd()
	if err != nil {
		return out
	}
	dir := wd
	for i := 0; i < 4; i++ {
		out = append(out, filepath.Join(dir, path))
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return out
}

// Route 是一条对外 HTTP 路由。
type Route struct {
	Method  string
	Pattern string
	Handle  func(*Server, http.ResponseWriter, *http.Request)
}

// Routes 是**全部**对外的 HTTP 路由，集中声明。
//
// ★ 集中声明是为了可审计：hardening_test.go 会逐条断言这张清单里不存在
//
//	任何通往内部表（token_directory / room_directory）的出口，
//	也不存在 debug 类端点。新增路由必须同时在那个测试里显式登记，
//	等于强制一次"这条口子该不该对外"的评审。
var Routes = []Route{
	{http.MethodPost, "/api/rooms", (*Server).HandleCreateRoom},

	{http.MethodGet, "/api/rooms/{key}", (*Server).HandleRoomMeta},
	{http.MethodPost, "/api/rooms/{key}/join", (*Server).HandleJoin},
	{http.MethodGet, "/api/rooms/{key}/inbox", (*Server).HandleInbox},
	{http.MethodGet, "/api/rooms/{key}/messages", (*Server).HandleGetMessages},
	{http.MethodPost, "/api/rooms/{key}/messages", (*Server).HandlePostMessage},
	{http.MethodPost, "/api/rooms/{key}/proposals", (*Server).HandlePropose},
	{http.MethodPost, "/api/rooms/{key}/votes", (*Server).HandleVote},
	{http.MethodPost, "/api/rooms/{key}/summary", (*Server).HandleSummary},
	{http.MethodGet, "/api/rooms/{key}/result", (*Server).HandleResult},
	{http.MethodPost, "/api/rooms/{key}/close", (*Server).HandleClose},

	{http.MethodGet, "/room/{key}", (*Server).HandlePage},
	{http.MethodGet, "/skill.md", (*Server).HandleSkillMD},
	{http.MethodGet, "/install.md", (*Server).HandleInstallMD},
	{http.MethodGet, "/healthz", (*Server).HandleHealthz},
}

// NewRouter 注册所有路由。
//
// 用 Go 1.22+ 的 ServeMux 方法模式（"POST /api/rooms/{roomId}/join"），
// 不引任何 web 框架：这个服务的路由表就这么大，框架只会增加依赖面。
func NewRouter(s *Server) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range Routes {
		rt := rt
		mux.HandleFunc(rt.Method+" "+rt.Pattern, func(w http.ResponseWriter, r *http.Request) {
			rt.Handle(s, w, r)
		})
	}

	// 兜底：未匹配的路径也返回统一错误形状
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]any{"error": map[string]string{
			"code": "not_found", "message": "没有这个接口：" + r.Method + " " + r.URL.Path,
		}})
	})

	return withCORS(mux)
}

// withCORS 让浏览器端的调试页面能直接调。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("access-control-allow-origin", "*")
		w.Header().Set("access-control-allow-headers", "authorization, content-type")
		w.Header().Set("access-control-allow-methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
