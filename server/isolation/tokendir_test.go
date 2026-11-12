// Package isolation —— 第五组：token_directory 这张"不属于任何租户"的表。
//
// 背景：为了写审计日志，后端新增了一张窄映射表 token_directory（token 哈希 → 租户/房间/agent），
// 因为跨租户去查 agents.token_hash 本身是被禁止的形态。
//
// 风险：这张表在设计上"不属于任何租户"，所以它天然是**新的跨租户可读面**——
// 一旦它有任何 HTTP 出口、或被联接到消息查询里，前面所有的租户隔离都白做。
// 这一组就是给它上锁：
//
//	① 它没有 HTTP 出口（任何路径都不该路由到它）；
//	② 它不能与消息内容联接（拿 A 的 token 走遍端点，不该看到 B 的任何内容）；
//	③ 它不泄漏凭证（响应与日志里只允许哈希前缀，不允许完整 token）；
//	④ 它落盘只有 5 个窄字段（没有正文、没有密码），用直接读库文件的方式验证。
package isolation

import (
	"bytes"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

// tokenDirectoryProbePaths 是"这张表可能被谁不小心暴出来"的候选面。
// 断言口径：一律 404。注意 405 也不行——405 同样说明这条路由存在。
var tokenDirectoryProbePaths = []string{
	"/token_directory",
	"/token_directory/",
	"/token-directory",
	"/tokenDirectory",
	"/tokens",
	"/tokens/",
	"/_debug",
	"/_debug/",
	"/_debug/token_directory",
	"/_debug/db",
	"/_debug/dump",
	"/internal",
	"/internal/",
	"/internal/token_directory",
	"/internal/db",
	"/admin/token_directory",
	"/api/_debug/token_directory",
	"/api/internal/token_directory",
}

func TestTokenDirectoryIsNotReachable(t *testing.T) {
	a, b, _ := sharedTenants(t)

	// 在 B 的房间里放一个独特标记，用来做"联接"检测：
	// 只要 A 的任何响应里出现它，就说明有路径把两张表联起来了。
	const marker = "TOKDIR_LEAK_MARKER_7f21"
	waitMyTurn(t, b)
	if r := postMessage(t, b, marker, nil); r.Status != 200 && r.Status != 201 {
		t.Logf("提示：B 放标记消息失败（HTTP %d），联接检测退化为只比对 alias/topic", r.Status)
	}

	// ── ① 没有 HTTP 出口 ─────────────────────────────────────────
	for _, p := range tokenDirectoryProbePaths {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			var body any
			if m == http.MethodPost {
				body = map[string]any{"roomId": b.roomID, "opKey": newOpKey()}
			}
			r := call(t, m, p, a.agentToken, body)
			if r.Status != http.StatusNotFound {
				t.Errorf("① token_directory 可能暴露了 HTTP 出口：%s %s → HTTP %d（期望 404）\n  响应 %s",
					m, p, r.Status, r.snippet())
			}
			expectNo5xx(t, r, "① token_directory 探测 "+m+" "+p)
		}
	}

	// ── ② 不能与消息内容联接 ─────────────────────────────────────
	// 用 A 的身份走遍所有"应该只返回 A 的东西"的端点。
	// 注意：/rooms/{b}/result 是产品明确设计的公开结果页，它本来就该返回 B 的议题，
	// 所以不放在这一组里（放进来会误报）。
	sweep := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/rooms/" + a.roomID + "/inbox?wait=0", nil},
		{http.MethodGet, "/rooms/" + a.roomID + "/messages?since=0", nil},
		{http.MethodGet, "/rooms/" + a.roomID + "/result", nil},
		{http.MethodGet, "/rooms/" + a.roomID, nil},
		{http.MethodGet, "/me", nil},
		{http.MethodGet, "/rooms/" + b.roomID + "/inbox?wait=0", nil},
		{http.MethodGet, "/rooms/" + b.roomID + "/messages?since=0", nil},
	}

	bForbidden := []string{marker, b.alias, b.topic}
	var collected bytes.Buffer

	for _, s := range sweep {
		r := call(t, s.method, s.path, a.agentToken, s.body)
		collected.Write(r.Body)
		expectNo5xx(t, r, "② 联接检测 "+s.path)
		for _, f := range bForbidden {
			if f == "" {
				continue
			}
			if strings.Contains(string(r.Body), f) {
				t.Errorf("② 用 A 的 token 访问 %s，响应里出现了 B 的 %q —— token_directory 被联接到消息内容了\n  响应 %s",
					s.path, f, r.snippet())
			}
		}
	}

	// 顺手把探测路径的响应也纳入凭证扫描
	for _, p := range tokenDirectoryProbePaths {
		if r := call(t, http.MethodGet, p, a.agentToken, nil); r.Status < 500 {
			collected.Write(r.Body)
		}
	}

	// ── ③ 不泄漏完整 token ───────────────────────────────────────
	all := collected.String()
	if serverLog != nil {
		all += serverLog()
	}
	secrets := map[string]string{
		"A 的 agentToken": a.agentToken,
		"B 的 agentToken": b.agentToken,
		"A 的 adminKey":   a.adminKey,
		"B 的 adminKey":   b.adminKey,
	}
	for label, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(all, s) {
			t.Errorf("③ 响应或日志里出现了 %s 原文（前 8 位 %s…）—— 只允许哈希前缀", label, firstN(s, 8))
		}
	}

	// ── ④ 落盘结构：只有 5 个窄字段 ──────────────────────────────
	checkTokenDirectorySchema(t)
}

// checkTokenDirectorySchema 直接读 SQLite 文件里的建表语句。
//
// 这不是白盒：验的是"落盘的东西有没有多余字段"，属于可观察行为，而且
// 不改业务代码、不 import 业务包，不读业务逻辑。
func checkTokenDirectorySchema(t *testing.T) {
	t.Helper()

	if serverDBPath == "" {
		t.Skip("④ 拿不到被测进程的 db 文件路径（AGCHAT_URL 模式或非 -db 启动），跳过表结构断言")
	}

	var raw bytes.Buffer
	found := false
	for _, p := range []string{serverDBPath, serverDBPath + "-wal"} {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		found = true
		raw.Write(data)
		// sqlite 页之间可能有空隙，补一个分隔符，避免两段拼出新单词
		raw.WriteByte('\n')
	}
	if !found {
		t.Skipf("④ 读不到 db 文件 %s，跳过表结构断言", serverDBPath)
	}

	ddl := extractCreateTable(raw.String(), "token_directory")
	if ddl == "" {
		t.Skipf("④ 库文件里没有找到 token_directory 的建表语句（实现可能用了别的方式做归属映射）")
	}

	cols := columnNames(ddl)
	if len(cols) == 0 {
		t.Fatalf("④ 解析不出 token_directory 的列名，建表语句原文：%s", firstN(ddl, 400))
	}
	t.Logf("④ token_directory 实际列：%v", cols)

	required := []string{"token_hash", "tenant_id", "room_id", "agent_id", "created_at"}
	colSet := map[string]bool{}
	for _, c := range cols {
		colSet[c] = true
	}
	missing := []string{}
	for _, r := range required {
		if !colSet[r] {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		t.Errorf("④ token_directory 缺少预期列 %v（实际 %v）—— 它应当是一张只有归属关系的窄表", missing, cols)
	}

	// 严禁出现正文/凭证类列。只看"列名"，不看类型（否则 TEXT 会被误伤）。
	forbidden := []string{"text", "body", "content", "message", "msg", "password", "passwd",
		"secret", "alias", "token_plain", "raw_token", "plain"}
	extra := []string{}
	for _, c := range cols {
		for _, f := range forbidden {
			if c == f || strings.Contains(c, f) {
				extra = append(extra, c)
				break
			}
		}
	}
	if len(extra) > 0 {
		t.Errorf("④ token_directory 里出现了不该有的列 %v —— 这张表只该存归属关系，"+
			"一旦有正文或凭证，它就成了跨租户的第二个数据源", extra)
	}
}

// extractCreateTable 从 sqlite 原始字节里捞出指定表的建表语句。
//
// 注意不能简单地"找 CREATE TABLE 然后读到分号"：库里的迁移表会把整段 SQL 当文本存着，
// 里面既有注释也有分号，很容易截错段落。这里要求表名紧跟 CREATE TABLE，
// 然后按括号配平找到真正的语句结尾。
func extractCreateTable(raw, table string) string {
	re := regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["` + "`" + `\[\]]?` +
		regexp.QuoteMeta(table) + `["` + "`" + `\[\]]?\s*\(`)
	loc := re.FindStringIndex(raw)
	if loc == nil {
		return ""
	}
	open := loc[1] - 1 // 指向 '('
	depth := 0
	for i := open; i < len(raw); i++ {
		switch raw[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return raw[loc[0] : i+1]
			}
		}
	}
	return raw[loc[0]:] // 括号没配平，返回剩余部分交给调用方报错
}

// stripSQLComments 去掉 -- 行注释和 /* */ 块注释，避免把注释当成列定义。
func stripSQLComments(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if i+1 < len(s) && s[i] == '-' && s[i+1] == '-' {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
			continue
		}
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			i += 2
			for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
				i++
			}
			i++
			continue
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

// columnNames 从 CREATE TABLE 语句里抽列名（跳过表约束）。
func columnNames(ddl string) []string {
	ddl = stripSQLComments(ddl)
	i := strings.Index(ddl, "(")
	j := strings.LastIndex(ddl, ")")
	if i < 0 || j <= i {
		return nil
	}
	body := ddl[i+1 : j]

	var cols []string
	depth := 0
	var cur strings.Builder
	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s == "" {
			return
		}
		f := strings.Fields(s)
		if len(f) == 0 {
			return
		}
		name := strings.ToLower(strings.Trim(f[0], "\"`[]'"))
		switch name {
		case "primary", "foreign", "unique", "check", "constraint":
			return // 表级约束，不是列
		}
		if name != "" {
			cols = append(cols, name)
		}
	}

	for _, ch := range body {
		switch ch {
		case '(':
			depth++
			cur.WriteRune(ch)
		case ')':
			depth--
			cur.WriteRune(ch)
		case ',':
			if depth == 0 {
				flush()
			} else {
				cur.WriteRune(ch)
			}
		default:
			cur.WriteRune(ch)
		}
	}
	flush()
	return cols
}
