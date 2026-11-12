// SQLite 实现 + 迁移。
//
// 为什么用 modernc.org/sqlite 而不是 mattn/go-sqlite3：
// 前者是纯 Go 实现，不需要 cgo。交叉编译、Windows 上直接 go build、
// 塞进 scratch 镜像都不用额外装编译器。
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open 打开（或创建）数据库并跑完迁移。
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", buildDSN(dsn))
	if err != nil {
		return nil, err
	}
	// 单连接：写操作天然串行，杜绝 SQLITE_BUSY。
	// 本服务的所有查询都是"读一个小聚合"，连接池不是瓶颈；
	// 长轮询等待期间**不持有连接**，所以单连接不会把并发请求堵死。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// buildDSN 补上 pragma 参数。已经带了 ? 的 DSN（比如内存库）要用 & 续接，
// 否则会把参数粘到上一个值后面（"no such cache mode: shared?_pragma=..."）。
func buildDSN(dsn string) string {
	if strings.Contains(dsn, "_pragma") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	extra := sep + "_pragma=busy_timeout(5000)&_txlock=immediate"
	if strings.Contains(dsn, "mode=memory") || strings.Contains(dsn, ":memory:") {
		// 内存库不支持 WAL
		return dsn + extra + "&_pragma=journal_mode(MEMORY)"
	}
	return dsn + extra + "&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
}

// OpenMemory 给测试用：独立的进程内数据库。
func OpenMemory() (*Store, error) {
	return Open("file::memory:?cache=shared&_pragma=journal_mode(MEMORY)")
}

// migrate 按文件名顺序执行 migrations/*.sql，执行过的记在 schema_migrations。
// 只认纯 SQL；postgres_rls.sql.example 不会被匹配到（扩展名不是 .sql）。
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	for _, name := range files {
		var seen int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, name).Scan(&seen)
		if err != nil {
			return err
		}
		if seen > 0 {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("迁移 %s 失败: %w", name, err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, strftime('%s','now')*1000)`,
			name); err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion 返回已应用的迁移数量（/healthz 与测试用）。
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations`).Scan(&n)
	return n, err
}
