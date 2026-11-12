package store

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// =============================================================================
// token_directory 的"最小面"守卫
//
// 这张表是全局的（没有租户作用域）：给定 token 哈希，返回它属于哪个租户/房间/agent。
// 它是本服务唯一能"跨租户"读的东西，所以必须被钉死在三件事上：
//   1. 只有这几列，没有任何正文/密码/昵称；
//   2. 只有两个函数能碰它（一个写、一个读）；
//   3. 读出来的结构体只有归属三元组。
// 任何一条被破坏，下面三个测试都会红。
// =============================================================================

// TestTokenDirectorySchemaIsMinimal 表结构必须**恰好**是这五列。
//
// 加列 = 有人想往里塞东西，必须先改这个测试，等于强制一次评审。
func TestTokenDirectorySchemaIsMinimal(t *testing.T) {
	s, err := Open("file:t_tokdir_schema?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rows, err := s.db.Query(`PRAGMA table_info(token_directory)`)
	if err != nil {
		t.Fatalf("读表结构失败: %v", err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}
	sort.Strings(cols)

	want := []string{"agent_id", "created_at", "room_id", "tenant_id", "token_hash"}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("token_directory 的列被改动了。\n  实际: %v\n  允许: %v\n"+
			"这张表只允许存归属映射；正文/密码/alias 一律不许进。", cols, want)
	}

	// 再显式点名禁止的语义列（防止有人用别的名字绕过）
	banned := []string{"text", "content", "body", "message", "password", "secret", "alias", "plaintext"}
	for _, c := range cols {
		for _, b := range banned {
			if c == b {
				t.Fatalf("token_directory 出现了禁止的列 %q", c)
			}
		}
	}
}

// TestTokenRefExposesOnlyAttribution 读出来的结构体只能有三个字段。
func TestTokenRefExposesOnlyAttribution(t *testing.T) {
	typ := reflect.TypeOf(TokenRef{})
	want := map[string]bool{"TenantID": true, "RoomID": true, "AgentID": true}
	if typ.NumField() != len(want) {
		t.Fatalf("TokenRef 字段数应为 %d，实际 %d —— 不许往上加内容字段", len(want), typ.NumField())
	}
	for i := 0; i < typ.NumField(); i++ {
		if f := typ.Field(i); !want[f.Name] {
			t.Fatalf("TokenRef 出现了非归属字段 %q", f.Name)
		}
	}
}

// TestOnlyTwoFunctionsTouchTokenDirectory 静态扫描：token_directory 只许出现在读写两个函数里，
// 且读的那条 SQL 必须显式列名（不许 SELECT *）。
func TestOnlyTwoFunctionsTouchTokenDirectory(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "store.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 store.go: %v", err)
	}

	// 允许的只有三个：读归属、写归属、以及认回换发 token 时删掉旧映射。
	// 新增任何一个都必须在这里显式登记 —— 等于强制一次"这张表该不该被这么用"的评审。
	allowed := map[string]bool{
		"LookupTokenTenant":           true,
		"PutTokenDirectory":           true,
		"DeleteTokenDirectoryByAgent": true, // reclaim：旧 token 换发后必须删掉归属行
	}
	touched := map[string]bool{}

	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		name := fd.Name.Name
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			sql, ok := evalString(lit, map[string]string{})
			if !ok || !strings.Contains(strings.ToLower(sql), "token_directory") {
				return true
			}
			touched[name] = true
			if !allowed[name] {
				t.Errorf("%s: 函数 %s 碰了 token_directory —— 只允许 LookupTokenTenant / PutTokenDirectory",
					fset.Position(lit.Pos()), name)
			}
			// 读的语句必须显式列名，且只有归属三元组
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "SELECT") {
				// 只禁 "SELECT *"，注释里的 * 不算
				if strings.Contains(strings.ToUpper(sql), "SELECT *") {
					t.Errorf("%s: 不许 SELECT *：%s", fset.Position(lit.Pos()), sql)
				}
				for _, c := range []string{"tenant_id", "room_id", "agent_id"} {
					if !strings.Contains(sql, c) {
						t.Errorf("%s: SELECT 缺少归属列 %q: %s", fset.Position(lit.Pos()), c, sql)
					}
				}
				for _, c := range []string{"text", "password", "alias", "message"} {
					if strings.Contains(sql, c) {
						t.Errorf("%s: SELECT 里出现了内容列 %q: %s", fset.Position(lit.Pos()), c, sql)
					}
				}
			}
			return true
		})
	}

	for name := range allowed {
		if !touched[name] {
			t.Fatalf("预期 %s 会操作 token_directory，但没扫到 —— 扫描逻辑可能坏了", name)
		}
	}
	if len(touched) != len(allowed) {
		t.Fatalf("碰 token_directory 的函数集合异常: %v", touched)
	}
}

// TestTokenDirectoryRoundTrip 正常读写一遍，确保守卫没把功能拦死。
func TestTokenDirectoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open("file:t_tokdir_rt?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	tenant, err := s.NewTenant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("ab", 32)
	if err := s.WithTenant(ctx, tenant, func(tx *Tx) error {
		return tx.PutTokenDirectory(hash, "r_zzzz", "m_1")
	}); err != nil {
		t.Fatal(err)
	}

	ref, found, err := s.LookupTokenTenant(ctx, hash)
	if err != nil || !found {
		t.Fatalf("应查得到: found=%v err=%v", found, err)
	}
	if ref.TenantID != tenant || ref.RoomID != "r_zzzz" || ref.AgentID != "m_1" {
		t.Fatalf("归属信息不对: %+v", ref)
	}

	// 查不到时必须是 found=false, err=nil（不能报错，否则会变成存在性 oracle）
	if _, found, err := s.LookupTokenTenant(ctx, "不存在的哈希"); err != nil || found {
		t.Fatalf("未知 token 应返回 found=false, err=nil，得到 found=%v err=%v", found, err)
	}
}
