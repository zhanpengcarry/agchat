package store

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestAssertTenantScope 锁住"漏写租户条件就报错"这条规则本身。
func TestAssertTenantScope(t *testing.T) {
	bad := []string{
		`SELECT * FROM rooms WHERE id = ?`,
		`UPDATE rooms SET state = ? WHERE id = ?`,
		`DELETE FROM messages WHERE seq = ?`,
		`INSERT INTO rooms (id, topic) VALUES (?, ?)`,
		``,
	}
	for _, q := range bad {
		if err := assertTenantScope(q); err == nil {
			t.Errorf("应当拒绝没有租户作用域的 SQL: %s", q)
		}
	}

	good := []string{
		`SELECT * FROM rooms WHERE tenant_id = ? AND id = ?`,
		`UPDATE rooms SET state = ? WHERE tenant_id = ? AND id = ?`,
		`DELETE FROM messages WHERE tenant_id = ? AND room_id = ?`,
		`INSERT INTO rooms (tenant_id, id) VALUES (?, ?)`,
		`INSERT INTO room_directory (room_id, tenant_id, created_at) VALUES (?,?,?)`,
	}
	for _, q := range good {
		if err := assertTenantScope(q); err != nil {
			t.Errorf("不应拒绝合法的 SQL: %s (%v)", q, err)
		}
	}
}

// TestWithTenantRejectsEmptyTenant 空租户直接拒绝，避免"忘记传租户"变成全库查询。
func TestWithTenantRejectsEmptyTenant(t *testing.T) {
	s, err := Open("file:t_scope?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	err = s.WithTenant(context.Background(), "", func(tx *Tx) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "空 tenantID") {
		t.Fatalf("空租户应被拒绝，得到 %v", err)
	}
}

// =============================================================================
// 静态扫描：本包所有 SQL 都必须带 tenant_id
// =============================================================================

// evalString 把 Go 表达式还原成字符串字面量（处理 "a" + "b" 与同文件 const）。
func evalString(e ast.Expr, consts map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, lok := evalString(v.X, consts)
		r, rok := evalString(v.Y, consts)
		if !lok || !rok {
			return "", false
		}
		return l + r, true
	case *ast.Ident:
		s, ok := consts[v.Name]
		return s, ok
	case *ast.ParenExpr:
		return evalString(v.X, consts)
	}
	return "", false
}

// TestNoUnscopedSQLInSource 静态扫描：store 包里的每条业务 SQL 都带租户作用域。
//
// 运行时还有 assertTenantScope 兜底，这里是"编译期之外的第二道网"：
// 有人复制粘贴出一条 `WHERE id = ?`，go test 立刻红。
func TestNoUnscopedSQLInSource(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "store.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 store.go 失败: %v", err)
	}

	// 收集同文件的字符串常量（roomCols / agentCols / msgCols 等）
	consts := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		gd, ok := n.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			return true
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if s, ok := evalString(vs.Values[i], consts); ok {
						consts[name.Name] = s
					}
				}
			}
		}
		return true
	})

	// 不在此包管辖的两类：租户表本身、两张只做路由/审计的窄表（已注明理由）
	allowed := []string{"tenant_id", "room_directory", "token_directory", "schema_migrations", "tenants "}

	// 拼接表达式的"操作数"节点不单独检查，否则 "INSERT INTO agents (" 这种
	// 半截字面量会被误报（真正的完整语句由外层 BinaryExpr 评估后统一检查）。
	operands := map[token.Pos]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if be, ok := n.(*ast.BinaryExpr); ok && be.Op == token.ADD {
			operands[be.X.Pos()] = true
			operands[be.Y.Pos()] = true
		}
		return true
	})

	seen := map[string]bool{}
	count := 0
	ast.Inspect(f, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok || operands[e.Pos()] {
			return true
		}
		s, ok := evalString(e, consts)
		if !ok {
			return true
		}
		trimmed := strings.TrimSpace(s)
		up := strings.ToUpper(trimmed)
		isBusinessSQL := strings.HasPrefix(up, "SELECT ") || strings.HasPrefix(up, "INSERT INTO") ||
			strings.HasPrefix(up, "UPDATE ") || strings.HasPrefix(up, "DELETE FROM")
		if !isBusinessSQL || seen[trimmed] {
			return true
		}
		seen[trimmed] = true
		count++

		for _, a := range allowed {
			if strings.Contains(strings.ToLower(trimmed), a) {
				return true
			}
		}
		t.Errorf("%s: SQL 缺少租户作用域:\n%s", fset.Position(e.Pos()), trimmed)
		return true
	})

	if count == 0 {
		t.Fatal("没扫到任何 SQL —— 扫描逻辑坏了，别当成通过")
	}
}
