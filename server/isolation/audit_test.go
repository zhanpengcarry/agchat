// Package isolation —— 第三组：审计日志生效性。
//
// 这一组验的是"出事之后能不能查"，属于数据隔离的一部分：
//
//	隔离做得好  = 攻击拿不到数据；
//	审计做得好  = 攻击发生了但没人知道 —— 这等于没隔离，只是运气好。
//
// 判定口径：
//  1. 打一次跨租户访问，进程输出里必须出现对应的审计记录（能定位到房间/结论）；
//  2. **日志里绝不能出现 token / adminKey 原文**——审计日志本身是第二个泄漏面，
//     把凭证写进日志，等于把隔离的钥匙贴在门上；
//  3. 允许记录 token 的哈希前缀（用于关联），但不能是可复用的凭证。
//
// 为什么这条能做黑盒：审计日志是对外可观察的行为。测试只读被测进程的
// stdout/stderr（由启动器捕获），不 import 任何业务代码，也不读数据库。
package isolation

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// auditMarkers 是"这条日志确实是在记跨租户拒绝"的候选特征。
// 不断言具体措辞（那是实现细节），只要能定位到目标房间或者明确表达了拒绝即可。
var auditMarkers = []string{
	"cross", "deny", "denied", "forbidden", "unauthorized", "forbid",
	"audit", "tenant", "room_not_member", "not_member",
	"越权", "跨租户", "拒绝", "无权",
}

func TestAuditLogOfCrossTenantAttempt(t *testing.T) {
	if serverLog == nil {
		t.Skip("当前用 AGCHAT_URL 打外部实例，读不到被测进程日志，跳过审计用例")
	}

	a, b, _ := sharedTenants(t)

	before := serverLog()

	// 三个方向各打一次，不押注实现只记其中一个入口
	attempts := []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/rooms/" + b.roomID + "/inbox?wait=0", nil},
		{http.MethodGet, "/rooms/" + b.roomID + "/messages?since=0", nil},
		{http.MethodPost, "/rooms/" + b.roomID + "/messages",
			map[string]any{"text": "审计探针", "opKey": newOpKey()}},
	}
	for _, at := range attempts {
		r := call(t, at.method, at.path, a.agentToken, at.body)
		if r.Status != http.StatusNotFound {
			t.Errorf("审计用例前置：跨租户 %s %s 期望 404，实际 %d",
				at.method, at.path, r.Status)
		}
	}

	// 审计写入可能是异步的（通常走一个有缓冲的 logger），轮询等它落地
	var fresh string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		now := serverLog()
		if len(now) > len(before) {
			fresh = now[len(before):]
			// 拿到新增内容后再等一小会儿，让同一批的后续行也落进来
			if strings.TrimSpace(fresh) != "" {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
	}

	if strings.TrimSpace(fresh) == "" {
		t.Fatalf("审计用例：跨租户访问之后，进程输出里没有任何新日志。\n"+
			"  期望：至少一条能定位到这次拒绝的记录（目标房间 %s）\n"+
			"  若服务端把审计写进独立文件而不是 stdout/stderr，请提供 -audit-log 类开关，\n"+
			"  或让测试用环境变量拿到文件路径。", b.roomID)
	}

	// ① 这条日志必须能定位到"发生了什么"
	located := strings.Contains(fresh, b.roomID)
	matched := ""
	for _, m := range auditMarkers {
		if strings.Contains(strings.ToLower(fresh), strings.ToLower(m)) {
			matched = m
			break
		}
	}
	if !located && matched == "" {
		t.Errorf("审计用例：新增了日志但看起来不像一条跨租户审计记录——\n"+
			"  既没有目标房间 id（%s），也没有任何拒绝类关键词（%v）。\n  新增内容：\n%s",
			b.roomID, auditMarkers, firstN(fresh, 500))
	} else {
		how := []string{}
		if located {
			how = append(how, "包含目标 roomId")
		}
		if matched != "" {
			how = append(how, "包含关键词 "+matched)
		}
		t.Logf("审计用例：已记录跨租户尝试（%s）", strings.Join(how, " / "))
	}

	// ② 硬要求：日志里不许出现任何凭证原文（这是本次最关键的断言）
	all := serverLog()
	secrets := map[string]string{
		"A 的 agentToken": a.agentToken,
		"B 的 agentToken": b.agentToken,
		"A 的 adminKey":   a.adminKey,
		"B 的 adminKey":   b.adminKey,
	}
	for label, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(all, s) {
			t.Errorf("审计用例：日志里出现了 %s 原文（前 8 位 %s…）—— 审计日志本身成了凭证泄漏面",
				label, firstN(s, 8))
		}
	}

	// ③ 允许哈希前缀，但必须是"前缀"而不是可复用凭证
	assertAuditOnlyStoresHashPrefix(t, fresh, a.agentToken)
}

var hexRe = regexp.MustCompile(`^[0-9a-fA-F]{4,32}$`)

// assertAuditOnlyStoresHashPrefix 解析结构化审计事件，确认：
//   - 事件里确实带着一个短期标识（用于关联），
//   - 这个标识是哈希前缀形态，而不是 token 原文或其开头一段。
func assertAuditOnlyStoresHashPrefix(t *testing.T, fresh, rawToken string) {
	t.Helper()

	type event map[string]any
	var best event
	for _, line := range strings.Split(fresh, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev event
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		// 找出那个"跨租户拒绝"事件
		blob := strings.ToLower(line)
		if strings.Contains(blob, "cross") || strings.Contains(blob, "deny") ||
			strings.Contains(blob, "reject") || strings.Contains(blob, "越权") ||
			strings.Contains(blob, "拒绝") {
			best = ev
			break
		}
	}

	if best == nil {
		t.Logf("③ 未解析到结构化审计事件（可能是纯文本日志），仅保留'无原文 token'的断言")
		return
	}

	// 打印一次事件键，便于人工核对字段是否只有归属信息
	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	t.Logf("③ 审计事件字段：%v", keys)

	var prefix string
	for _, k := range []string{"token_hash_prefix", "token_prefix", "token_fp", "token_id"} {
		if v, ok := best[k].(string); ok && v != "" {
			prefix = v
			break
		}
	}
	if prefix == "" {
		t.Logf("③ 审计事件里没有 token 短标识字段（允许：不等于泄漏，只是关联能力弱一些）")
		return
	}

	if !hexRe.MatchString(prefix) {
		t.Errorf("③ 审计里的 token 标识 %q 不是十六进制哈希前缀形态，可能直接记了 token 片段", prefix)
	}
	if len(prefix) > 16 {
		t.Errorf("③ 审计里的 token 标识长度 %d 超过 16，前缀过长容易被暴力还原", len(prefix))
	}
	// 最要命的一种错：前缀取的是 token 原文的头几位（那等于泄漏了一半凭证）
	if rawToken != "" && strings.HasPrefix(rawToken, prefix) {
		t.Errorf("③ 审计里的 token 标识 %q 是 token 原文的前缀 —— 这不是哈希，是凭证泄漏", prefix)
	}
	t.Logf("③ 审计只记了哈希前缀 %q（长度 %d，非 token 原文前缀）", prefix, len(prefix))
}
