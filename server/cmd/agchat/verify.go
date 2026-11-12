package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"agchat/internal/oplog"
	"agchat/internal/verify"
)

// runVerify 是 `agchat verify --bundle X [--json]` 的实现。
//
// 注册方式是 main.go 里的 verifyHook（client.go 声明）：本文件在 init 里接上，
// 这样 main.go 不需要知道 verify 的实现细节（eng-host 与 eng-verify 的边界）。
//
// 退出码（docs/09 §6）：0=verified / 1=failed / 2=unverifiable。
// 参数用法错误也返回 2：本命令不承诺"用法错的调用是可以通过的"。
//
// 输出纪律（docs/09 §7 v1.1）：--json 时单行 JSON 只写 stdout，人话只写 stderr。
// 人读输出必须能把判决落到"第几条 op"上（failed 尤其如此）。
// 话术禁令（arch C-6/C-8）：这里只许说「结论可复算」「转写稿未被改」
// 「宿主干预可枚举」，不许说「已证明」「不可篡改」。
func init() { verifyHook = runVerify }

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	bundlePath := fs.String("bundle", "", "证据包路径（bundle.zip）")
	asJSON := fs.Bool("json", false, "输出单行机器可读 JSON 到 stdout，人话走 stderr")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "用法：agchat verify --bundle bundle.zip [--json]")
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "多余参数：%v\n用法：agchat verify --bundle bundle.zip [--json]\n", fs.Args())
		return 2
	}
	if *bundlePath == "" {
		fmt.Fprintln(os.Stderr, "缺 --bundle：用法：agchat verify --bundle bundle.zip [--json]")
		return 2
	}

	b, err := oplog.LoadBundle(*bundlePath)
	if err != nil {
		return emitVerifyReport(verify.Report{
			State: verify.StateUnverifiable,
			Code:  verify.CodeBundleUnreadable,
			Codes: []string{verify.CodeBundleUnreadable},
			Reasons: []string{
				"证据包读不出来：" + err.Error(),
				"这属于结构性失败：文件不完整或不是本工具能读的 bundle；无法据此判断会议结论",
			},
		}, *asJSON, os.Stdout, os.Stderr)
	}

	rep, err := verify.Run(b)
	if err != nil {
		return emitVerifyReport(verify.Report{
			State: verify.StateUnverifiable,
			Code:  verify.CodeBundleUnreadable,
			Codes: []string{verify.CodeBundleUnreadable},
			Reasons: []string{
				"证据包无法判定：" + err.Error(),
			},
		}, *asJSON, os.Stdout, os.Stderr)
	}
	return emitVerifyReport(rep, *asJSON, os.Stdout, os.Stderr)
}

// emitVerifyReport 输出判决并返回退出码。stdout/stderr 显式传入，便于测试。
func emitVerifyReport(rep verify.Report, asJSON bool, stdout, stderr io.Writer) int {
	if asJSON {
		if err := json.NewEncoder(stdout).Encode(rep); err != nil {
			fmt.Fprintf(stderr, "写出 JSON 失败：%v\n", err)
			return 2
		}
		fmt.Fprintf(stderr, "%s\n", verifyHeadline(rep))
		return rep.ExitCode()
	}
	writeVerifyHuman(stdout, rep)
	return rep.ExitCode()
}

// verifyHeadline 是一句话人话（JSON 模式下走 stderr）。
func verifyHeadline(rep verify.Report) string {
	switch rep.State {
	case verify.StateVerified:
		return "判决：verified —— 结论可复算，转写稿未被改，全部 op 签名有效。"
	case verify.StateFailed:
		return fmt.Sprintf("判决：failed —— 第 %s 条 op 处发现不一致（原因码 %s）。",
			failAtText(rep.FailAt), rep.Code)
	default:
		return fmt.Sprintf("判决：unverifiable —— 证据不足，无法判定（原因码 %s）。", rep.Code)
	}
}

// writeVerifyHuman 写人读报告：先给判决、再给两条指纹、再给位置与原因。
func writeVerifyHuman(w io.Writer, rep verify.Report) {
	fmt.Fprintln(w, verifyHeadline(rep))
	fmt.Fprintf(w, "原因码：%s\n", rep.Code)
	fmt.Fprintf(w, "op 签名：%d/%d 有效\n", rep.SigsValid, rep.SigsTotal)
	fmt.Fprintf(w, "结论指纹 decisionSet.hash：%s\n", matchText(rep.DecisionMatch))
	fmt.Fprintf(w, "转写指纹 transcriptHash：%s\n", matchText(rep.TranscriptMatch))
	if rep.FailAt > 0 {
		fmt.Fprintf(w, "位置：第 %d 条 op\n", rep.FailAt)
	} else if rep.State == verify.StateFailed {
		fmt.Fprintln(w, "位置：无法定位到单条 op（这是整条流重算出来的指纹级不一致）")
	}
	if rep.AckNote != "" {
		fmt.Fprintf(w, "ack 收据：%s\n", rep.AckNote)
	}
	if len(rep.Reasons) > 0 {
		fmt.Fprintln(w, "说明：")
		for _, r := range rep.Reasons {
			fmt.Fprintf(w, "  - %s\n", r)
		}
	}
	// 诚实边界（arch C-6/C-8）：只列能证明什么，不许升级成"已证明/不可篡改"。
	fmt.Fprintln(w, "边界：本判决只覆盖「结论可复算 / 转写稿未被改 / 干预可枚举」；"+
		"它不证明宿主未作弊，也不证明每个座位的实际操作者是谁。")
}

func matchText(match bool) string {
	if match {
		return "一致"
	}
	return "不一致或未比对成功"
}

func failAtText(at int) string {
	if at <= 0 {
		return "?"
	}
	return fmt.Sprintf("%d", at)
}
