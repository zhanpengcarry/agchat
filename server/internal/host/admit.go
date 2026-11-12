package host

import (
	"agchat/internal/invite"
	"agchat/internal/oplog"
)

// admit 是**能力校验的唯一入口**（arch A-9）。
//
// S1 只有 direct 一条载体，但仍然必须走这个函数：否则 S2 接 envelope 时，
// collect 路径会绕过 HTTP handler 里的检查，成为一条没人守的门。
//
// 约定：caps 是"join 时冻结在座位上的能力位"，来自卡的闭枚枚举
// {join,read,post,propose,vote}（arch A-8）。alias 永不参与判断。
func admit(caps []string, want string) (bool, string) {
	if want == "" {
		return true, ""
	}
	for _, c := range caps {
		if c == want {
			return true, ""
		}
	}
	return false, "这张卡没有「" + capLabel(want) + "」能力（caps 在进场时冻结，不能事后追加）"
}

func capLabel(capability string) string {
	switch capability {
	case invite.CapJoin:
		return "进场"
	case invite.CapRead:
		return "读"
	case invite.CapPost:
		return "发言"
	case invite.CapPropose:
		return "提方案"
	case invite.CapVote:
		return "投票"
	}
	return capability
}

// capForKind 把 op 类型映射到能力位。
//
// ★ summary 不在卡的闭枚能力位里（它是"获胜者才能写"的权限，判定在引擎的
// summaryAuthor）。宿主这一层只要求"能写"，具体谁是获胜者由 room 包判。
func capForKind(kind string) string {
	switch kind {
	case oplog.OpPost:
		return invite.CapPost
	case oplog.OpPropose:
		return invite.CapPropose
	case oplog.OpVote:
		return invite.CapVote
	case oplog.OpSummary:
		return invite.CapPost
	}
	return ""
}
