package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"agchat/internal/host"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	switch args[0] {
	case "room":
		return cmdRoom(args[1:])
	case "invite":
		return cmdInvite(args[1:])
	case "join":
		return cmdJoin(args[1:])
	case "next":
		return cmdNext(args[1:])
	case "post":
		return cmdPost(args[1:])
	case "propose":
		return cmdPropose(args[1:])
	case "vote":
		return cmdVote(args[1:])
	case "summary":
		return cmdSummary(args[1:])
	case "result":
		return cmdResult(args[1:])
	case "export":
		return cmdExport(args[1:])
	case "verify":
		if verifyHook != nil {
			return verifyHook(args[1:])
		}
		fmt.Fprintln(os.Stderr, "verify: 尚未就绪（cmd/agchat/verify.go 由 eng-verify 提供）")
		return exitFail
	case "host":
		return cmdHost(args[1:])
	case "help", "-h", "--help":
		usage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", args[0])
		usage()
		return exitUsage
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `agchat — 本地宿主 CLI（S1 直连档）

  agchat room new --topic T [--seats 3] [--rounds 6] [--minutes 30] [--addr 127.0.0.1:0] [--expose-lan] [--db PATH] [--json]
  agchat room status [--ref REF] [--json]
  agchat room close --ref REF
  agchat room resume --ref REF [--addr ADDR] [--expose-lan]
  agchat invite  --ref REF [--uses N] [--exp MILLIS] [--json]
  agchat join    --card FILE|TEXT [--as 名字] [--model M] [--yes] [--json]
  agchat next    --ref REF [--wait 20] [--json]
  agchat post    --ref REF --text T [--reply-to N] [--support P]
  agchat propose --ref REF --title T --body B [--revises P]
  agchat vote    --ref REF (--proposal P | --abstain) [--reason R]
  agchat summary --ref REF --text T
  agchat result  --ref REF [--json]
  agchat export  --ref REF [--out bundle.zip]
  agchat verify  --bundle bundle.zip [--json]

数据目录：AGCHAT_HOME 或 ~/.agchat
`)
}

// newFlags 造一个不往 stderr 乱打 usage 的 FlagSet。
func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// cmdHost 是隐藏的守护进程入口：`agchat host serve`。
func cmdHost(args []string) int {
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, "用法: agchat host serve [--addr ADDR] [--expose-lan] [--home DIR]")
		return exitUsage
	}
	fs := newFlags("host serve")
	addr := fs.String("addr", "", "监听地址（默认 127.0.0.1:0）")
	expose := fs.Bool("expose-lan", false, "绑 0.0.0.0 并把内网地址写进卡")
	home := fs.String("home", "", "数据目录（默认 AGCHAT_HOME 或 ~/.agchat）")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage
	}
	cfg := loadCfg()
	if *home != "" {
		cfg.Home = *home
		cfg.DBPath = *home + string(os.PathSeparator) + "agchat.db"
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	cfg.ExposeLAN = *expose

	h, err := host.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "宿主启动失败:", err)
		return exitFail
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := h.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "宿主退出:", err)
		return exitFail
	}
	return exitOK
}
