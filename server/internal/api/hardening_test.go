package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
)

// =============================================================================
// 对外接口面的守卫
//
// token_directory 是全局表（无租户作用域），所以必须保证它**没有任何 HTTP 出口**，
// 而且只被 404 拒绝路径读。
// =============================================================================

// TestRoutesAreExplicitlyWhitelisted 路由清单必须与这里逐条一致。
//
// 新增任何路由都会让这个测试红 —— 强制一次"这条口子该不该对外"的评审，
// 尤其防止有人为了排查顺手加个 /api/_debug/tokens。
func TestRoutesAreExplicitlyWhitelisted(t *testing.T) {
	got := make([]string, 0, len(Routes))
	for _, rt := range Routes {
		got = append(got, rt.Method+" "+rt.Pattern)
	}
	sort.Strings(got)

	want := []string{
		"GET /api/rooms/{key}",
		"GET /api/rooms/{key}/inbox",
		"GET /api/rooms/{key}/messages",
		"GET /api/rooms/{key}/result",
		"GET /healthz",
		"GET /install.md",
		"GET /room/{key}",
		"GET /skill.md",
		"POST /api/rooms",
		"POST /api/rooms/{key}/close",
		"POST /api/rooms/{key}/join",
		"POST /api/rooms/{key}/messages",
		"POST /api/rooms/{key}/proposals",
		"POST /api/rooms/{key}/summary",
		"POST /api/rooms/{key}/votes",
	}
	sort.Strings(want)

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("路由清单变了。\n  实际:\n    %s\n  期望:\n    %s",
			strings.Join(got, "\n    "), strings.Join(want, "\n    "))
	}

	// 再加一道：任何路径都不许出现这些字样
	forbidden := []string{"token", "directory", "debug", "internal", "admin", "_"}
	for _, p := range got {
		lower := strings.ToLower(p)
		for _, f := range forbidden {
			if strings.Contains(lower, f) {
				t.Fatalf("路由 %q 含禁止字样 %q —— 内部表/调试口不许对外", p, f)
			}
		}
	}
}

// TestNoDebugEndpointsServed 真的打一遍常见调试路径，必须都是 404 且不回显内部表名。
func TestNoDebugEndpointsServed(t *testing.T) {
	ts := newTestServer(t)
	for _, p := range []string{
		"/api/token_directory",
		"/api/token-directory",
		"/api/tokens",
		"/api/_debug/tokens",
		"/api/_debug",
		"/debug/token_directory",
		"/api/rooms/_debug",
		"/api/rooms/r_zzzz/_debug",
	} {
		res, body := get(t, ts, p, "")
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s 应 404，得到 %d", p, res.StatusCode)
		}
		if strings.Contains(string(body), "token_hash") || strings.Contains(string(body), "agent_id") {
			t.Fatalf("%s 的响应里出现了内部字段: %s", p, body)
		}
	}
}

// TestTokenDirectoryOnlyReadFromRejectPath 静态扫描：LookupTokenTenant 只许在 auditReject 里调用，
// auditReject 只许被两条 404 路径调用。
//
// 这条保证"跨租户读"这件事在代码里只有**一个**入口，且它只在凭证被拒时发生 ——
// 正常请求永远碰不到 token_directory。
func TestTokenDirectoryOnlyReadFromRejectPath(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析 api 包: %v", err)
	}

	lookupCallers := map[string]bool{}
	auditCallers := map[string]bool{}

	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fun := call.Fun.(type) {
					case *ast.SelectorExpr:
						switch fun.Sel.Name {
						case "LookupTokenTenant":
							lookupCallers[fd.Name.Name] = true
						case "auditReject":
							auditCallers[fd.Name.Name] = true
						}
					}
					return true
				})
			}
		}
	}

	// LookupTokenTenant 只能出现在 auditReject 里
	for name := range lookupCallers {
		if name != "auditReject" {
			t.Fatalf("函数 %s 调用了 LookupTokenTenant —— 只允许 auditReject 读这张表", name)
		}
	}
	if !lookupCallers["auditReject"] {
		t.Fatal("没扫到 auditReject 调用 LookupTokenTenant，扫描逻辑可能坏了")
	}

	// auditReject 只能被这两条 404 路径调用
	allowed := map[string]bool{"resolveRoom": true, "writeEngineErr": true}
	for name := range auditCallers {
		if !allowed[name] {
			t.Fatalf("函数 %s 调用了 auditReject —— 只允许 404 拒绝路径（resolveRoom / writeEngineErr）", name)
		}
	}
	if len(auditCallers) == 0 {
		t.Fatal("没扫到 auditReject 的任何调用点，扫描逻辑可能坏了")
	}
}
