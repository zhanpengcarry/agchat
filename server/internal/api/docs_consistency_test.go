package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentFacingDocs 是 agent 真正会读到的两份文件：
//   - skills/agchat/SKILL.md —— 通过 /skill.md 输出，join 响应里的 brief.skillUrl 指向它
//   - mvp/join.md           —— 通过 /install.md 输出
//
// 它们不参与编译，所以代码改了文案不改也不会有任何编译错误或红灯。
// v2.2 就吃了这个亏：Go 里删了 403 self_vote_forbidden，SKILL.md 却还在教 agent
// 「不能投给自己」，结果规则改了个寂寞。这条测试把"文案是否跟着规格走"变成红灯。
func readAgentDoc(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "skills", "agchat", name)
	if name == "join.md" {
		path = filepath.Join("..", "..", "..", "mvp", name)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		// 允许在只复制了 server/ 的环境（如 Docker 构建层）里跳过；
		// 仓库内跑 go test 时这两份文件一定存在。
		t.Skipf("读不到 agent 文档 %s（%v），跳过文案一致性检查", path, err)
	}
	return string(b)
}

func TestSkillDocMatchesV22VoteRules(t *testing.T) {
	s := readAgentDoc(t, "SKILL.md")

	if strings.Contains(s, "self_vote_forbidden") {
		t.Fatal("SKILL.md 仍在讲已删除的 403 self_vote_forbidden —— v2.2 允许自投，agent 会照着弃权")
	}
	if strings.Contains(s, "不能投给自己") || strings.Contains(s, "不要投给自己") {
		t.Fatal("SKILL.md 仍在禁止自投，与 v2.2「允许自投 + 跨票主序」相反")
	}
	for _, want := range []string{"投给自己", "跨票"} {
		if !strings.Contains(s, want) {
			t.Fatalf("SKILL.md 缺少 v2.2 的关键表述「%s」：agent 读不到就无法按新规则投票", want)
		}
	}
}

// TestJoinDocIsNotV1 防止 /install.md 再退回 v1 状态机。
//
// join.md 曾经整篇停留在 v1：讲 converging 状态、finalStance、yourQuota，
// 按它做的 agent 在 v2 房间里第一步就卡住。它不是"旧版本说明"，它是线上文档。
func TestJoinDocIsNotV1(t *testing.T) {
	s := readAgentDoc(t, "join.md")

	for _, banned := range []string{"converging", "yourQuota"} {
		if strings.Contains(s, banned) {
			t.Fatalf("join.md 仍含 v1 时代的「%s」，agent 会按已不存在的流程走", banned)
		}
	}
	if !strings.Contains(s, "/skill.md") {
		t.Fatal("join.md 没有指向 /skill.md —— 它是给 agent 的最小清单，完整规则必须指向 skill")
	}
	if !strings.Contains(s, "voting") {
		t.Fatal("join.md 没提 voting 阶段，v2 的投票环节在接入说明里消失了")
	}
}

// TestFallbackDocsAreV2 内置兜底文案也必须是 v2。
//
// 兜底只在"文件读不到"时生效——而那正是最危险的时刻：服务端已经在跑了，
// agent 却按 v1 的 converging / finalStance 去聊一个不存在的阶段。
func TestFallbackDocsAreV2(t *testing.T) {
	docs := map[string][]byte{"fallbackSkill": fallbackSkill, "fallbackJoin": fallbackJoin}
	for name, b := range docs {
		s := string(b)
		for _, banned := range []string{"converging", "收敛轮"} {
			if strings.Contains(s, banned) {
				t.Fatalf("%s 仍是 v1 口径（含「%s」）：\n%s", name, banned, s)
			}
		}
		for _, want := range []string{"inbox", "voting"} {
			if !strings.Contains(s, want) {
				t.Fatalf("%s 缺少 v2 要素「%s」：\n%s", name, want, s)
			}
		}
	}
	// 兜底 skill 必须告诉 agent 概述这一步怎么走，否则获胜者无处可去
	if !strings.Contains(string(fallbackSkill), "youAreWinner") {
		t.Fatal("fallbackSkill 没提 inbox.youAreWinner，获胜者不会去写概述")
	}
	// 兜底 join 必须提到"可以投自己"，否则 v2.2 的自投规则在兜底路径上失传
	if !strings.Contains(string(fallbackJoin), "可以投自己") {
		t.Fatal("fallbackJoin 没提「可以投自己」，与 v2.2 相反")
	}
}
