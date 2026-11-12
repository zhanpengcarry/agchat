// Package budget 实现 est_tokens 计量。
//
// 为什么不用真实 token 数：服务端拿不到 agent 平台的计费口径，
// 也无法验证。所以定义一个**可复现、可对账**的替代单位：
//
//	中文（含全角标点、假名）1 字 = 1 token
//	其余字符（ASCII/数字/半角符号）每 4 个 = 1 token，向上取整
//	混排按字符类分别累加
//
// 这个单位只用于"预算控制"，不用于计费结算，所以不需要和任何
// 厂商的 tokenizer 对齐——只要同一房间内所有人用同一把尺子即可。
package budget

// IsCJK 判断一个码点是否按"一字一 token"计。
func IsCJK(cp rune) bool {
	return (cp >= 0x3000 && cp <= 0x303f) || // CJK 标点
		(cp >= 0x3400 && cp <= 0x4dbf) ||
		(cp >= 0x4e00 && cp <= 0x9fff) || // 基本汉字
		(cp >= 0xf900 && cp <= 0xfaff) ||
		(cp >= 0xff00 && cp <= 0xff60) || // 全角字符
		(cp >= 0x3040 && cp <= 0x30ff) // 日文假名
}

// EstTokens 估算一段文本的 est_tokens。
func EstTokens(text string) int {
	if text == "" {
		return 0
	}
	cjk, rest := 0, 0
	for _, r := range text {
		if IsCJK(r) {
			cjk++
		} else {
			rest++
		}
	}
	return cjk + (rest+3)/4 // 向上取整的 ceil(rest/4)
}

// Ledger 是一份预算账本视图。
type Ledger struct {
	Used  int `json:"used"`
	Total int `json:"total"`
	Left  int `json:"left"`
	Pct   int `json:"pct"`
}

// 预算红线。这三个数字是本产品的核心承诺，改动前请看 README。
const (
	ReservePct = 5  // 永远预留 5% 给收敛轮
	WarnPct    = 80 // 到这里在 notes 里提醒
	HardPct    = 95 // 到这里强制进入收敛轮
)

// View 返回账本视图。
func View(used, total int) Ledger {
	left := total - used
	if left < 0 {
		left = 0
	}
	pct := 0
	if total > 0 {
		pct = int(float64(used)/float64(total)*100 + 0.5) // 四舍五入
	}
	return Ledger{Used: used, Total: total, Left: left, Pct: pct}
}

// HardCap 返回辩论轮的写入硬上限（95% 线）。
func HardCap(total int) int { return total * HardPct / 100 }
