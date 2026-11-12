// Package isolation —— 第六组：可信代理语义 与 审计日志落盘。
//
// 这两条验的都是"部署形态"带来的问题，而不是代码里能一眼看出来的 bug：
//
//	① 可信代理（-trusted-proxies）
//	   限流键如果是 TCP 对端 IP，放到 nginx/CDN 后面所有用户共享代理 IP，
//	   按来源限流就退化成全局桶 —— 一个人能把全站打成 429。
//	   修法是维护可信代理 CIDR 白名单，只信任"可信对端"给的 X-Forwarded-For，
//	   并且**取最右侧的非可信地址**（左侧全是客户端可随意伪造的）。
//	   只加信任而不取最右侧，等于把伪造能力白送给攻击者。
//
//	② 审计日志落盘（-audit-log）
//	   只写 stdout 的审计等于没有审计：进程重启就没了。
//	   但落盘开关必须"打不开就启动失败"，不允许静默降级成不记录 ——
//	   安全开关静默失效，比没有这个开关更危险。
package isolation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// enumProbe 打一次枚举探测，返回状态码。
func enumProbe(base, id string, hdr map[string]string) (int, error) {
	r, err := doAtH(base, http.MethodGet, "/rooms/"+id, "", hdr, nil)
	if err != nil {
		return 0, err
	}
	return r.Status, nil
}

// exhaustEnum 连续打 n 次，把当前"来源"的枚举配额打满。
func exhaustEnum(t *testing.T, base string, hdr map[string]string, n int) map[int]int {
	t.Helper()
	statuses := map[int]int{}
	for i := 0; i < n; i++ {
		st, err := enumProbe(base, fmt.Sprintf("r_pad%03d", i), hdr)
		if err != nil {
			t.Fatalf("打满配额时第 %d 次探测失败：%v", i, err)
		}
		statuses[st]++
	}
	return statuses
}

func TestTrustedProxySemantics(t *testing.T) {
	if serverBin == "" {
		t.Skip("当前用 AGCHAT_URL 打外部实例，无法另起实例，跳过")
	}
	if !supportsFlag(serverBin, "trusted-proxies") {
		t.Skip("被测版本还没有 -trusted-proxies，可信代理语义尚未实现")
	}

	const limit = 5
	const spoofIP = "203.0.113.9"

	// ── 情形 A：没配可信代理时，XFF 必须被忽略 ──────────────────────
	t.Run("未配置可信代理时忽略XFF", func(t *testing.T) {
		srv, err := launchWithHost(serverBin, "127.0.0.1", []string{"-enum-rate", fmt.Sprint(limit)})
		if err != nil {
			t.Fatalf("启动失败：%v", err)
		}
		defer srv.stop()

		// 先把 127.0.0.1 的桶打满（不带 XFF）
		if st := exhaustEnum(t, srv.base, nil, limit+2); st[http.StatusTooManyRequests] == 0 {
			t.Fatalf("限流没生效，无法验证 XFF 是否被忽略：%v", st)
		}

		// 带一个伪造 XFF 再打：如果被信任就会拿到新配额（404）
		st, err := enumProbe(srv.base, "r_xffa001", map[string]string{"X-Forwarded-For": spoofIP})
		if err != nil {
			t.Fatalf("探测失败：%v", err)
		}
		if st == http.StatusNotFound {
			t.Errorf("未配置 -trusted-proxies 时 XFF 却生效了：带 X-Forwarded-For=%s 拿到了新配额（404）。"+
				"这等于让任意客户端靠改一个请求头就绕过限流", spoofIP)
		} else {
			t.Logf("情形A 通过：未配置可信代理时 XFF 被忽略（仍为 %d，按 TCP 对端 127.0.0.1 计）", st)
		}
	})

	// ── 情形 B：可信对端给的 XFF 应当生效 ──────────────────────────
	t.Run("可信代理下按XFF计", func(t *testing.T) {
		srv, err := launchWithHost(serverBin, "127.0.0.1",
			[]string{"-enum-rate", fmt.Sprint(limit), "-trusted-proxies", "127.0.0.1/32"})
		if err != nil {
			t.Fatalf("启动失败：%v", err)
		}
		defer srv.stop()

		// 打满 127.0.0.1 自己的桶
		if st := exhaustEnum(t, srv.base, nil, limit+2); st[http.StatusTooManyRequests] == 0 {
			t.Fatalf("限流没生效：%v", st)
		}

		// 同一个连接、只改 XFF → 应当被当作另一个来源，拿到新配额
		st, err := enumProbe(srv.base, "r_xffb001", map[string]string{"X-Forwarded-For": spoofIP})
		if err != nil {
			t.Fatalf("探测失败：%v", err)
		}
		if st != http.StatusNotFound {
			t.Errorf("已配置 -trusted-proxies 127.0.0.1/32，但带 X-Forwarded-For=%s 仍被限流（%d）。"+
				"说明 XFF 没有被采纳，反向代理部署下限流依然会退化成全局桶", spoofIP, st)
		} else {
			t.Logf("情形B 通过：可信对端提供的 XFF 生效，%s 拿到了独立配额", spoofIP)
		}
	})

	// ── 情形 C：伪造链必须取"最右侧的非可信地址" ────────────────────
	t.Run("伪造链取最右侧非可信地址", func(t *testing.T) {
		srv, err := launchWithHost(serverBin, "127.0.0.1",
			[]string{"-enum-rate", fmt.Sprint(limit), "-trusted-proxies", "127.0.0.1/32"})
		if err != nil {
			t.Fatalf("启动失败：%v", err)
		}
		defer srv.stop()

		legit := map[string]string{"X-Forwarded-For": spoofIP}

		// 先把"真实客户端" 203.0.113.9 的桶打满
		if st := exhaustEnum(t, srv.base, legit, limit+2); st[http.StatusTooManyRequests] == 0 {
			t.Fatalf("无法把 %s 的桶打满：%v", spoofIP, st)
		}

		// 对照组：1.1.1.1 这个身份是干净的
		stClean, err := enumProbe(srv.base, "r_xffc000", map[string]string{"X-Forwarded-For": "1.1.1.1"})
		if err != nil {
			t.Fatalf("探测失败：%v", err)
		}
		if stClean != http.StatusNotFound {
			t.Logf("提示：1.1.1.1 单独访问未拿到新配额（%d），伪造链用例的判别力会打折扣", stClean)
		}

		// 攻击者把伪造地址塞在左边：XFF = "1.1.1.1, 203.0.113.9"
		// 正确实现取最右侧非可信 = 203.0.113.9（桶已满 → 429）
		// 错误实现取最左侧 1.1.1.1（桶是空的 → 404，攻击者成功换身份）
		stForged, err := enumProbe(srv.base, "r_xffc001",
			map[string]string{"X-Forwarded-For": "1.1.1.1, " + spoofIP})
		if err != nil {
			t.Fatalf("探测失败：%v", err)
		}

		switch stForged {
		case http.StatusTooManyRequests:
			t.Logf("情形C 通过：伪造链取的是最右侧非可信地址 %s（正确），攻击者换不成身份", spoofIP)
		case http.StatusNotFound:
			t.Errorf("伪造链被取成了最左侧的 1.1.1.1（攻击者伪造的那个），而不是最右侧非可信的 %s。"+
				"这意味着任何人只要在 XFF 里随便写个 IP 就能无限绕过限流", spoofIP)
		default:
			t.Errorf("伪造链探测返回意外状态 %d", stForged)
		}
	})
}

// joinAt 在指定实例上进场。
func joinAt(base, roomID, password, alias string) (joinInfo, error) {
	var info joinInfo
	r, err := doAt(base, http.MethodPost, "/rooms/"+roomID+"/join", "", map[string]any{
		"password": password,
		"alias":    alias,
		"model":    "isolation-test",
	})
	if err != nil {
		return info, err
	}
	if r.Status != 200 && r.Status != 201 {
		return info, fmt.Errorf("进场返回 HTTP %d：%s", r.Status, r.snippet())
	}
	if err := json.Unmarshal(r.Body, &info); err != nil {
		return info, err
	}
	if info.AgentToken == "" {
		return info, fmt.Errorf("进场响应缺 agentToken：%s", r.snippet())
	}
	return info, nil
}

func TestAuditLogFileSink(t *testing.T) {
	if serverBin == "" {
		t.Skip("当前用 AGCHAT_URL 打外部实例，无法另起实例，跳过")
	}
	if !supportsFlag(serverBin, "audit-log") {
		t.Skip("被测版本还没有 -audit-log，审计落盘尚未实现")
	}

	// ── A. 落盘内容：有事件、无原文凭证 ─────────────────────────────
	t.Run("写入JSONL且不含原文token", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "audit.jsonl")
		srv, err := launchWithHost(serverBin, "127.0.0.1", []string{"-audit-log", logPath})
		if err != nil {
			t.Fatalf("带 -audit-log 启动失败：%v", err)
		}
		defer srv.stop()

		roomA, err := createRoomAt(srv.base, newUniqueTopic("auditfile-a"))
		if err != nil {
			t.Fatalf("建房失败：%v", err)
		}
		roomB, err := createRoomAt(srv.base, newUniqueTopic("auditfile-b"))
		if err != nil {
			t.Fatalf("建房失败：%v", err)
		}
		joined, err := joinAt(srv.base, roomA.RoomID, roomA.Password, "audit-agent")
		if err != nil {
			t.Fatalf("进场失败：%v", err)
		}

		// 打一次跨租户
		r, err := doAt(srv.base, http.MethodGet, "/rooms/"+roomB.RoomID+"/inbox?wait=0", joined.AgentToken, nil)
		if err != nil {
			t.Fatalf("跨租户请求失败：%v", err)
		}
		if r.Status != http.StatusNotFound {
			t.Fatalf("前置：跨租户应 404，实际 %d", r.Status)
		}

		// 审计写入可能异步，轮询文件
		var content string
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			b, err := os.ReadFile(logPath)
			if err == nil && len(b) > 0 {
				content = string(b)
				if strings.Contains(content, roomB.RoomID) {
					break
				}
			}
			time.Sleep(80 * time.Millisecond)
		}

		if strings.TrimSpace(content) == "" {
			t.Fatalf("审计文件 %s 是空的或没有生成 —— -audit-log 没真正落盘", logPath)
		}
		if !strings.Contains(content, roomB.RoomID) {
			t.Errorf("审计文件里没有出现被攻击的房间 %s。\n  文件内容：%s",
				roomB.RoomID, firstN(content, 600))
		} else {
			t.Logf("落盘生效：审计文件中已记录目标房间 %s", roomB.RoomID)
		}

		// 落盘文件里绝不能有原文凭证
		secrets := map[string]string{
			"agentToken": joined.AgentToken,
			"adminKey":   roomA.AdminKey,
		}
		for label, s := range secrets {
			if s != "" && strings.Contains(content, s) {
				t.Errorf("审计文件里出现了 %s 原文（前 8 位 %s…）—— 落盘日志成了凭证泄漏面",
					label, firstN(s, 8))
			}
		}

		// 行格式：每行应当是一个独立 JSON 对象（JSONL），便于后续采集
		lines := 0
		badLines := 0
		for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			lines++
			var probe map[string]any
			if json.Unmarshal([]byte(line), &probe) != nil {
				badLines++
			}
		}
		if lines == 0 {
			t.Errorf("审计文件没有任何有效行")
		} else if badLines > 0 {
			t.Errorf("审计文件有 %d/%d 行不是合法 JSON（不是 JSONL，采集端会解析失败）", badLines, lines)
		} else {
			t.Logf("落盘生效：%d 行，全部是合法 JSONL", lines)
		}
	})

	// ── B. 打不开就必须启动失败，不允许静默降级 ─────────────────────
	t.Run("不可写路径应启动失败", func(t *testing.T) {
		// 拿一个已存在的**目录**当文件路径：任何写操作都会失败
		unwritable := t.TempDir()

		srv, err := launchWithHost(serverBin, "127.0.0.1", []string{"-audit-log", unwritable})
		if err == nil {
			srv.stop()
			t.Fatalf("审计日志路径不可写（%s 是个目录）时服务仍然启动成功了。"+
				"安全开关打不开就静默降级成不记录，比没有这个开关更危险", unwritable)
		}
		t.Logf("启动被正确拒绝（符合预期）：%v", firstN(err.Error(), 200))
	})
}
