package room

import (
	"fmt"
	"strings"

	"agchat/internal/domain"
)

// BuildPrompt 生成 §六「预先出示的 prompt」——每个 agent 进房拿到的同一份讨论框架。
//
// ★ 人数必须用 **maxAgents**，不能用"此刻已进房的人数"：
//
//	prompt 在 join 那一刻生成，第一个人看到的永远是"共 1 位参会者" ——
//	那会让它按"一个人的会议"来组织语言（自我陈述而不是交锋），而后面进来的人
//	看到的又是别的数。用上限表述，t=0 时就是对的，也不会随后续进场而自相矛盾。
//	真实名单在开场时通过 inbox.roster 下发。
func BuildPrompt(r *domain.Room, a *domain.Agent) string {
	var b strings.Builder

	b.WriteString("【agchat 讨论室 · 会议规则】\n\n")
	fmt.Fprintf(&b, "议题：%s\n", r.Topic)
	if r.Rules != "" {
		fmt.Fprintf(&b, "房间规则：%s\n", r.Rules)
	} else {
		b.WriteString("房间规则：（无额外规则）\n")
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "本场会议最多 %d 位参会者，你是第 %d 位进入的 agent。\n", r.MaxAgents, a.Ord+1)
	if a.IsModerator(r) {
		b.WriteString("你是本次会议的主席，第 1 轮由你首先发言，并且你要在收尾轮总结分歧。\n")
	} else {
		b.WriteString("请依次发言，不要抢话。\n")
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "流程（共 %d 轮）：\n", r.MaxRounds)
	b.WriteString("- 第 1 轮：给出你的立场和理由。\n")
	if r.MaxRounds > 2 {
		fmt.Fprintf(&b, "- 第 2–%d 轮：必须回复别人的具体观点（replyTo），指出你不同意哪一句、为什么。期间可以提交方案（每人最多 %d 个）。\n",
			r.MaxRounds-1, MaxProposalsPerAgent)
	} else {
		fmt.Fprintf(&b, "- 期间可以提交方案（每人最多 %d 个）。\n", MaxProposalsPerAgent)
	}
	fmt.Fprintf(&b, "- 第 %d 轮：说明你支持哪个方案及理由。\n", r.MaxRounds)
	b.WriteString("- 之后进入投票，每人 1 票，选出你认为最对的方案。\n")
	b.WriteString("- 得票最多的 agent 的负责人要写一段简要概述。\n\n")

	b.WriteString("硬约束：\n")
	// 措辞按 Lead 裁决：不写死"字"，因为英文发言的字与 est_tokens 差 4 倍
	fmt.Fprintf(&b, "- 单条发言不超过 %d est_tokens（中文约等于字数），超出会被拒收。\n", r.MaxOutputTokens)
	fmt.Fprintf(&b, "- 全房间 token 预算 %d，用尽会强制进入投票。\n", r.TokenBudget)
	fmt.Fprintf(&b, "- 讨论最长 %d 分钟。\n\n", r.MaxMinutes)

	b.WriteString("发言纪律：\n")
	b.WriteString("- 不许客套。「我同意」「说得好」不是发言。\n")
	b.WriteString("- 不复述议题。\n")
	b.WriteString("- 分歧就说分歧，不要为了和谐改口。\n")
	// ★ v2.2 换了投票规则，这段 prompt 必须跟着换 —— 它是每个 agent 进房读的第一份话。
	//   v2.1 写的是「不要投给自己提的方案」，结果逼着"唯一认真提了方案的人"去投别人的
	//   一句话提案，2/3 的"多数"是规则产物（见 docs/07 §十五）。文案不改，代码改了也白改。
	// ★ v2.3 起"自投不能构成多数"是结构事实（每人 1 票、每个方案 1 个作者 ⇒ 自投最多 1 张），
	//   不再是一条审查规则。措辞必须与 skill.md/README 一致，否则 agent 会以为有一条额外限制
	//   （一位验证 agent 就指出过 prompt 与 skill 在这里"有张力"）。
	b.WriteString("- 投票时**可以投给你自己提的方案**：投你认为最对的那个，不是「最像共识」的那个。\n")
	b.WriteString("- 胜出看**跨票**：别人投给你的才算数；自投票单独计数，但每人只有 1 票、每个方案只有 1 个作者，所以自投最多 1 张、堆不出多数。既别靠自投凑数，也别为了避嫌去投一个你认为更差的方案。\n")

	return b.String()
}
