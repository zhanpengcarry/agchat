// Package config 解析启动配置：flag 优先，其次环境变量，最后默认值。
package config

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是进程的全部可调参数。
type Config struct {
	Addr          string        // 监听地址 :8788
	DB            string        // SQLite 文件路径
	TurnTimeout   time.Duration // 回合超时：占着回合不说话就跳过
	JoinRate      int           // 同一房间同一 IP 每分钟最多 join 次数
	EnumRate      int           // 同一 IP 每分钟最多房间枚举请求（0 = 关闭）
	RoomRate      int           // 同一 IP 每 10 分钟最多建会数（0 = 关闭；压测/批量导入时调大）
	Briefing      time.Duration // 建房间后多久自动开讨论
	TickEvery     time.Duration // 后台状态推进间隔
	SkillPath     string        // skill 说明书路径（唯一来源，不复制进二进制）
	JoinPath      string        // 一页式接入说明路径
	ShutdownGrace time.Duration // 优雅退出等最长的长轮询时间

	// TrustedProxies 是允许代理 X-Forwarded-For 的上游 CIDR。
	// 默认空 = 谁都不信，只用 TCP 对端 IP（安全默认）。
	TrustedProxies []*net.IPNet

	// AuditLog 为空时审计日志只写 stdout；配置后以 JSONL 追加写文件。
	AuditLog   string
	AuditFsync bool

	// Healthcheck 为 true 时不启动服务，只探一次本机 /healthz 后退出。
	// 用途：distroless 运行镜像里没有 curl/wget，容器 healthcheck 只能让二进制自己探。
	Healthcheck bool

	// v2 会议默认值与硬上限（规格 §十）
	DefaultBudget   int
	DefaultMinutes  int
	MaxRounds       int
	MaxOutputTokens int
	MaxAgents       int
	SummaryTimeout  time.Duration
	VoteGrace       time.Duration
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// parseCIDRs 解析逗号分隔的 CIDR 列表；有任何一项非法就报错（启动期失败优于运行期静默）。
func parseCIDRs(spec string) ([]*net.IPNet, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var out []*net.IPNet
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ip, network, err := net.ParseCIDR(part)
		if err != nil {
			// 允许写成裸 IP（当成 /32 或 /128）
			if single := net.ParseIP(part); single != nil {
				bits := 32
				if single.To4() == nil {
					bits = 128
				}
				out = append(out, &net.IPNet{IP: single, Mask: net.CIDRMask(bits, bits)})
				continue
			}
			return nil, fmt.Errorf("trusted-proxies 里的 %q 不是合法 CIDR", part)
		}
		network.IP = ip.Mask(network.Mask)
		out = append(out, network)
	}
	return out, nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// Load 解析命令行。参数名与环境变量名同名（大写）。
func Load(args []string) (Config, error) {
	fs := flag.NewFlagSet("agchatd", flag.ContinueOnError)
	var c Config

	fs.StringVar(&c.Addr, "addr", envStr("ADDR", ":8788"), "监听地址")
	fs.StringVar(&c.DB, "db", envStr("DB", "agchat.db"), "SQLite 文件路径（:memory: 也行）")
	fs.DurationVar(&c.TurnTimeout, "turn-timeout", envDur("TURN_TIMEOUT", 90*time.Second), "回合超时，到点跳过不说话的 agent")
	fs.IntVar(&c.JoinRate, "join-rate", envInt("JOIN_RATE", 8), "同一房间同一 IP 每分钟最多 join 次数")
	fs.IntVar(&c.RoomRate, "room-rate", envInt("ROOM_RATE", 10), "同一 IP 每 10 分钟最多建会数（0 = 关闭）")
	fs.IntVar(&c.EnumRate, "enum-rate", envInt("ENUM_RATE", 60), "同一 IP 每分钟最多房间查询次数（只作用于不需要 token 的枚举口；0 = 关闭）")
	fs.DurationVar(&c.Briefing, "briefing", envDur("BRIEFING", 120*time.Second), "建房间后多久自动开讨论")
	fs.DurationVar(&c.TickEvery, "tick", envDur("TICK", time.Second), "后台状态推进间隔")
	fs.StringVar(&c.SkillPath, "skill", envStr("SKILL", "../skills/agchat/SKILL.md"), "skill 说明书路径（/skill.md 的内容来源）")
	fs.StringVar(&c.JoinPath, "join", envStr("JOIN", "../mvp/join.md"), "接入说明路径（/install.md 的内容来源）")
	fs.DurationVar(&c.ShutdownGrace, "shutdown-grace", envDur("SHUTDOWN_GRACE", 30*time.Second), "优雅退出等待上限")

	trusted := fs.String("trusted-proxies", envStr("TRUSTED_PROXIES", ""),
		"可信反代 CIDR 逗号列表（默认空 = 不认 X-Forwarded-For，只用 TCP 对端 IP）")
	fs.StringVar(&c.AuditLog, "audit-log", envStr("AUDIT_LOG", ""),
		"审计日志路径（JSONL 追加写；空 = 只写 stdout）")
	fs.BoolVar(&c.AuditFsync, "audit-fsync", envBool("AUDIT_FSYNC", true),
		"每条审计日志写完后 fsync（默认开）")

	fs.IntVar(&c.DefaultBudget, "default-budget", envInt("DEFAULT_BUDGET", 20000000), "建会不传 tokenBudget 时的默认预算")
	fs.IntVar(&c.DefaultMinutes, "default-minutes", envInt("DEFAULT_MINUTES", 30), "默认讨论时长（分钟）")
	fs.IntVar(&c.MaxRounds, "max-rounds", envInt("MAX_ROUNDS", 10), "轮数默认值/硬上限（规格夹到 10）")
	fs.IntVar(&c.MaxOutputTokens, "max-output-tokens", envInt("MAX_OUTPUT_TOKENS", 800), "单条输出上限（est_tokens）")
	fs.IntVar(&c.MaxAgents, "max-agents", envInt("MAX_AGENTS", 6), "会议人数硬上限")
	fs.DurationVar(&c.VoteGrace, "vote-grace", envDur("VOTE_GRACE", 10*time.Second),
		"全员投完后额外等待的宽限期：给被误判掉线的成员回来补投（0 = 立即结算）")
	fs.DurationVar(&c.SummaryTimeout, "summary-timeout", envDur("SUMMARY_TIMEOUT", 120*time.Second),
		"等待获胜者写概述的超时（从它最后一次活动起算，硬上限 2×），超时用机械摘要兜底")
	fs.BoolVar(&c.Healthcheck, "healthcheck", envBool("HEALTHCHECK", false),
		"只探测本机 /healthz 后退出：0 = 健康，1 = 不健康（给容器 healthcheck 用）")

	if err := fs.Parse(args); err != nil {
		return c, err
	}

	parsed, err := parseCIDRs(*trusted)
	if err != nil {
		return c, err
	}
	c.TrustedProxies = parsed
	return c, nil
}
