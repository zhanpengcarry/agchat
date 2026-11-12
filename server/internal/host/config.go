// Package host 是 S1 的本地宿主：一个进程、一个 SQLite 文件、一个 ticker，
// 把 room 引擎 + 邀请卡 + 操作日志 + 本地控制面拼成一个可直接被 agent 调用的
// HTTP 服务（docs/09）。
//
// 三条纪律（改这个包之前请先读）：
//  1. 引擎复用是硬约束：宿主只做装配、能力校验（admit）、日志与证据，
//     **任何"这票算不算数"的判断都在 room 包里**（em §2）。
//  2. 只允许两条路径触发状态跃迁：ticker 与"改状态请求前的先推进"（G12/G13）。
//     无凭证端点一律不 tick（arch R-7）。
//  3. 一进程 / 一库 / 一 ticker。房间不单独建库（用户明确要求轻量化）。
package host

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TenantID 是本地宿主的固定租户。store.WithTenant 只拒绝空串，
// 所以常量 "local" 让既有引擎与存储层 0 行改动（A-26：宿主不拿它做任何逻辑判断）。
const TenantID = "local"

// Version 是宿主版本，写进 rendezvous 文件（arch B-17）。
const Version = "0.1.0-s1"

// Config 是宿主进程的全部可配置项。
type Config struct {
	Home        string // 数据目录（AGCHAT_HOME 覆盖），默认 ~/.agchat
	DBPath      string // 单库路径，默认 <Home>/agchat.db
	Addr        string // 监听地址，默认 127.0.0.1:0
	ExposeLAN   bool   // --expose-lan 才绑 0.0.0.0，并把内网地址写进卡
	DisplayName string // 宿主显示名（卡里的 hn）
	OwnerName   string // 房主显示名（卡里的 on）
	Version     string

	TickEvery      time.Duration // 1s
	TurnTimeout    time.Duration // 90s
	Briefing       time.Duration // 120s
	SummaryTimeout time.Duration // 60s
	VoteGrace      time.Duration // 10s
}

// DefaultConfig 返回规格默认值（docs/09 §5/§7）。
func DefaultConfig() Config {
	return Config{
		Home:           HomeDir(),
		Addr:           "127.0.0.1:0",
		DisplayName:    defaultDisplayName(),
		OwnerName:      defaultOwnerName(),
		Version:        Version,
		TickEvery:      time.Second,
		TurnTimeout:    90 * time.Second,
		Briefing:       120 * time.Second,
		SummaryTimeout: 60 * time.Second,
		VoteGrace:      10 * time.Second,
	}
}

// HomeDir 返回数据目录：AGCHAT_HOME 优先，否则 ~/.agchat。
func HomeDir() string {
	if v := strings.TrimSpace(os.Getenv("AGCHAT_HOME")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".agchat"
	}
	return filepath.Join(home, ".agchat")
}

func defaultDisplayName() string {
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		return "agchat@" + h
	}
	return "agchat 本地宿主"
}

func defaultOwnerName() string {
	if v := strings.TrimSpace(os.Getenv("USERNAME")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("USER")); v != "" {
		return v
	}
	return "房主"
}

// ensureDefaults 补齐零值（CLI 只改关心的字段）。
func (c *Config) ensureDefaults() {
	d := DefaultConfig()
	if c.Home == "" {
		c.Home = d.Home
	}
	if c.DBPath == "" {
		c.DBPath = filepath.Join(c.Home, "agchat.db")
	}
	if c.Addr == "" {
		c.Addr = d.Addr
	}
	if c.DisplayName == "" {
		c.DisplayName = d.DisplayName
	}
	if c.OwnerName == "" {
		c.OwnerName = d.OwnerName
	}
	if c.Version == "" {
		c.Version = d.Version
	}
	if c.TickEvery <= 0 {
		c.TickEvery = d.TickEvery
	}
	if c.TurnTimeout <= 0 {
		c.TurnTimeout = d.TurnTimeout
	}
	if c.Briefing <= 0 {
		c.Briefing = d.Briefing
	}
	if c.SummaryTimeout <= 0 {
		c.SummaryTimeout = d.SummaryTimeout
	}
	if c.VoteGrace < 0 {
		c.VoteGrace = 0
	}
}

// ---- 数据目录布局（docs/09 §7）---------------------------------------------

func (c Config) controlTokenPath() string  { return filepath.Join(c.Home, "control.token") }
func (c Config) hostKeyPath() string       { return filepath.Join(c.Home, "host.key") }
func (c Config) homeJSONPath() string      { return filepath.Join(c.Home, "host.json") }
func (c Config) roomsDir() string          { return filepath.Join(c.Home, "rooms") }
func (c Config) seatsDir() string          { return filepath.Join(c.Home, "seats") }
func (c Config) roomDir(ref string) string { return filepath.Join(c.Home, "rooms", ref) }
func (c Config) roomSecretPath(ref string) string {
	return filepath.Join(c.Home, "rooms", ref, "host.key")
}
func (c Config) roomJSONPath(ref string) string {
	return filepath.Join(c.Home, "rooms", ref, "host.json")
}
func (c Config) roomPidPath(ref string) string {
	return filepath.Join(c.Home, "rooms", ref, "host.pid")
}
func (c Config) opsPath(ref string) string {
	return filepath.Join(c.Home, "rooms", ref, "ops.jsonl")
}
func (c Config) cardsPath(ref string) string {
	return filepath.Join(c.Home, "rooms", ref, "cards.json")
}
func (c Config) SeatPath(ref string) string {
	return filepath.Join(c.Home, "seats", ref+".key")
}
func (c Config) ReceiptsPath(ref string) string {
	return filepath.Join(c.Home, "seats", ref+".receipts.jsonl")
}
