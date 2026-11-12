// Command agchatd 是多 agent 讨论室的后台服务。
//
// 设计取舍一句话总结：
//   - 状态放 SQLite（modernc 纯 Go 驱动，无 cgo），单实例跑，先不做分布式；
//   - 隔离靠四层（租户列 + Go 层强制作用域 + PG RLS 样例 + 凭证三元组/404）；
//   - 长轮询用进程内事件订阅，等待期间不持有数据库连接。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"agchat/internal/api"
	"agchat/internal/audit"
	"agchat/internal/config"
	"agchat/internal/room"
	"agchat/internal/store"
)

func main() {
	log.SetFlags(log.Ltime)
	// 审计日志走结构化输出（一行一个 JSON），方便直接进日志系统做检索与告警。
	// 生命周期日志仍用 log 包，人读更顺。
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "参数错误:", err)
		os.Exit(2)
	}
	// 容器 healthcheck 模式：探一次就退，不进服务主循环。
	if cfg.Healthcheck {
		os.Exit(probeHealth(cfg.Addr))
	}
	if err := run(cfg); err != nil {
		log.Printf("启动失败: %v", err)
		os.Exit(1)
	}
}

// probeHealth 让二进制自己探 /healthz。
// 运行镜像是 distroless（没有 shell、没有 curl/wget），容器 healthcheck 只能用这个办法。
func probeHealth(addr string) int {
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	cli := &http.Client{Timeout: 3 * time.Second}
	resp, err := cli.Get("http://" + host + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck 失败:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck 状态码:", resp.StatusCode)
		return 1
	}
	return 0
}

func run(cfg config.Config) error {
	// 审计日志先开：配置了却打不开时直接启动失败（静默失效比没有更危险）。
	var auditFile *audit.Writer
	if cfg.AuditLog != "" {
		w, err := audit.Open(cfg.AuditLog, cfg.AuditFsync)
		if err != nil {
			return fmt.Errorf("审计日志不可用，拒绝启动: %w", err)
		}
		auditFile = w
		defer func() { _ = auditFile.Close() }()
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(audit.Multi(os.Stdout, auditFile), &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	st, err := store.Open(cfg.DB)
	if err != nil {
		return fmt.Errorf("打开数据库: %w", err)
	}

	engine := room.New(st, cfg.TurnTimeout, cfg.Briefing, cfg.SummaryTimeout)
	engine.SetVoteGrace(cfg.VoteGrace)
	engine.SetDefaults(room.Defaults{
		TokenBudget:     cfg.DefaultBudget,
		MaxMinutes:      cfg.DefaultMinutes,
		MaxRounds:       cfg.MaxRounds,
		MaxOutputTokens: cfg.MaxOutputTokens,
		MaxAgents:       cfg.MaxAgents,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 后台状态推进：用 SafeGo 包住，panic 只记日志、不让进程消失
	room.SafeGo("ticker", func() { engine.RunTicker(ctx, cfg.TickEvery) })

	docs := api.LoadDocs(cfg.SkillPath, cfg.JoinPath)
	srv := api.New(engine, st, cfg, docs)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.NewRouter(srv),
		ReadHeaderTimeout: 10 * time.Second,
		// 长轮询最长 20s，所以不能设过短的 ReadTimeout/WriteTimeout
		IdleTimeout: 120 * time.Second,
	}

	printBanner(cfg, st)

	errCh := make(chan error, 1)
	room.SafeGo("http-server", func() { errCh <- httpSrv.ListenAndServe() })

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			break
		}
		return err
	case <-sigCh:
		log.Println("收到退出信号：先关 listener → 等长轮询跑完 → 再关数据库")
	}

	// 1) 停止后台 tick
	cancel()

	// 2) Shutdown 会先关闭监听，再等待在途请求（包括挂着的长轮询）自然结束
	shutCtx, shutCancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer shutCancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Printf("优雅退出等待超时（%s），强制结束: %v", cfg.ShutdownGrace, err)
	} else {
		log.Println("长轮询已全部结束")
	}

	// 3) 最后关数据库
	if err := st.Close(); err != nil {
		return fmt.Errorf("关闭数据库: %w", err)
	}
	log.Println("已退出")
	return nil
}

func printBanner(cfg config.Config, st *store.Store) {
	base := "http://127.0.0.1" + cfg.Addr
	if cfg.Addr == "" || cfg.Addr[0] != ':' {
		base = "http://" + cfg.Addr
	}
	fmt.Printf("agchat 讨论室已启动  %s\n", base)
	fmt.Printf("  数据库      %s\n", cfg.DB)
	fmt.Printf("  回合超时    %s（到点跳过不说话的 agent）\n", cfg.TurnTimeout)
	fmt.Printf("  join 限流   %d 次/分钟/房间/IP\n", cfg.JoinRate)
	fmt.Printf("  枚举限流    %d 次/分钟/IP（0 = 关闭）\n", cfg.EnumRate)
	if len(cfg.TrustedProxies) == 0 {
		fmt.Printf("  可信代理    未配置 —— 只认 TCP 对端 IP，忽略 X-Forwarded-For（安全默认）\n")
	} else {
		var nets []string
		for _, n := range cfg.TrustedProxies {
			nets = append(nets, n.String())
		}
		fmt.Printf("  可信代理    %s —— 仅对这些来源采信 X-Forwarded-For\n", strings.Join(nets, ", "))
	}
	if cfg.AuditLog == "" {
		fmt.Printf("  审计日志    只写 stdout\n")
	} else {
		fmt.Printf("  审计日志    %s（JSONL 追加，fsync=%v；未做轮转，生产请配 logrotate）\n", cfg.AuditLog, cfg.AuditFsync)
	}
	fmt.Printf("  skill       %s   → %s/skill.md\n", cfg.SkillPath, base)
	fmt.Printf("  接入说明    %s   → %s/install.md\n", cfg.JoinPath, base)
	fmt.Println()
	fmt.Println("1) 建房间（会分配一个租户）")
	fmt.Printf("   curl -s -X POST %s/api/rooms -H \"content-type: application/json\" "+
		"-d \"{\\\"topic\\\":\\\"要不要把定价从年付改月付\\\",\\\"tokenBudget\\\":200000,\\\"maxRounds\\\":3,\\\"maxAgents\\\":3}\"\n", base)
	fmt.Println("2) 把 roomId + password 发给 agent，它自己加入")
	fmt.Printf("   curl -s -X POST %s/api/rooms/<roomId>/join -H \"content-type: application/json\" "+
		"-d \"{\\\"password\\\":\\\"<password>\\\",\\\"alias\\\":\\\"小周 Claude\\\",\\\"model\\\":\\\"claude-sonnet\\\"}\"\n", base)
	fmt.Println("3) 看结论页（只读）")
	fmt.Printf("   %s/room/<roomId>\n", base)
	fmt.Println()
	fmt.Printf("agent 读规则：%s/skill.md   健康检查：%s/healthz\n", base, base)
}
