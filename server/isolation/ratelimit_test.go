// Package isolation —— 第四组：枚举限流本身。
//
// 为什么单独一条、而且要另起一个实例：
//
//	404 挡住的是"内容"，挡不住"速度"。攻击者就算每次只拿到 404，
//	只要允许他每分钟打一万次，房间 id 空间（r_ + 4 位）迟早被扫完。
//	所以限流是隔离的一部分，不是性能优化。
//
// 主测试实例为了让"存在性不可区分"能被统计出来，是关掉限流的（-enum-rate 0），
// 因此这里另起一个受限实例专测，并且额外验两件容易被忽略的事：
//
//	① 429 也要"不可区分"—— 不能把 404 的泄漏换成 429 的泄漏；
//	② 限流要"按攻击者计"—— 别人把桶打满，不能连带把我也限了。
package isolation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// doAt 是对指定 base 发请求的最小版本（主 base 用包级 baseURL，这里要打第二个实例）。
func doAt(base, method, path, token string, body any) (resp, error) {
	return doAtH(base, method, path, token, nil, body)
}

func doAtH(base, method, path, token string, hdr map[string]string, body any) (resp, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return resp{}, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return resp{}, err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return resp{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return resp{Status: res.StatusCode, Body: raw, Method: method, Path: path}, nil
}

func createRoomAt(base, topic string) (roomInfo, error) {
	var info roomInfo
	r, err := doAt(base, http.MethodPost, "/rooms", "", map[string]any{
		"topic":        topic,
		"maxAgents":    2,
		"maxRounds":    2,
		"tokenBudget":  100000,
		"minutesLimit": 60,
	})
	if err != nil {
		return info, err
	}
	if r.Status != 200 && r.Status != 201 {
		return info, fmt.Errorf("建房失败 HTTP %d：%s", r.Status, r.snippet())
	}
	if err := json.Unmarshal(r.Body, &info); err != nil {
		return info, err
	}
	return info, nil
}

func TestEnumRateLimiterBlocksBulkEnumeration(t *testing.T) {
	if serverBin == "" {
		t.Skip("当前用 AGCHAT_URL 打外部实例，无法另起受限实例，跳过")
	}
	if !supportsFlag(serverBin, "enum-rate") {
		t.Skip("被测版本没有 -enum-rate，枚举限流尚未实现")
	}

	// 独立实例：限流压到 10 次/分钟，方便快速打穿。
	// 用双栈监听（[::]）是为了让 127.0.0.1 和 ::1 成为两个**真实的**来源 IP，
	// 这样"限流按来源隔离"才验得出来——靠 X-Forwarded-For 不行，服务端未必信任它。
	const limit = 10
	srv, err := launchWithHost(serverBin, "[::]", []string{"-enum-rate", fmt.Sprint(limit)})
	if err != nil {
		t.Fatalf("启动受限实例失败：%v", err)
	}
	defer srv.stop()
	t.Logf("受限实例基地址 %s（另一来源 %s）", srv.base, srv.baseAt("::1"))

	// 一个真实存在的房间，用来做"存在 vs 不存在"的对照
	room, err := createRoomAt(srv.base, newUniqueTopic("ratelimit-room"))
	if err != nil {
		t.Fatalf("在受限实例上建房失败：%v", err)
	}

	// ── 阶段 1：用不存在的 id 把桶打满，抓住第一个 429 ───────────────
	var bodyNonexist []byte
	var durNonexist time.Duration
	statuses := map[int]int{}
	for i := 0; i < limit*3+10; i++ {
		id := fmt.Sprintf("r_scan%03d", i)
		s := time.Now()
		r, err := doAt(srv.base, http.MethodGet, "/rooms/"+id, "", nil)
		d := time.Since(s)
		if err != nil {
			t.Fatalf("第 %d 次探测失败：%v", i, err)
		}
		statuses[r.Status]++
		if r.Status == http.StatusTooManyRequests && bodyNonexist == nil {
			bodyNonexist = append([]byte(nil), r.Body...)
			durNonexist = d
		}
	}
	t.Logf("阶段1（不存在房间）状态分布 %v，-enum-rate=%d", statuses, limit)

	if statuses[http.StatusTooManyRequests] == 0 {
		t.Fatalf("限流没生效：连续探测 %d 次都没有 429，房间空间可被无限扫描", limit*3+10)
	}
	// 断言的是**不变量**，不是精确计数：
	//   ① 最终必须出现 ≥1 个 429 —— 这是真正要保住的性质（限流确实会生效）
	//   ② 前面 404 的数量允许浮动，只约束上界（放行太多说明限流没压住）
	// 高负载下 404→429 的切换点会漂移，锁死计数只会制造假红灯；
	// 但绝不能 Skip —— 限流是唯一的枚举防护，跳过等于让它静默失效。
	if statuses[http.StatusNotFound] > limit*2+5 {
		t.Errorf("限流放行了 %d 次 404（阈值 %d），超出容忍带 2×+5", statuses[http.StatusNotFound], limit)
	}
	if n := statuses[http.StatusOK]; n > 0 {
		t.Errorf("有 %d 次探测返回 200 —— 不存在的房间不该有 200", n)
	}
	for code, n := range statuses {
		if code >= 500 {
			t.Errorf("出现 %d 次 5xx（%d）", n, code)
		}
	}

	// ── 阶段 2：桶已满，改用"真实存在"的房间再打，必须也是 429 ─────────
	var bodyExist []byte
	var durExist time.Duration
	statusExist := 0
	for i := 0; i < 5; i++ {
		s := time.Now()
		r, err := doAt(srv.base, http.MethodGet, "/rooms/"+room.RoomID, "", nil)
		d := time.Since(s)
		if err != nil {
			t.Fatalf("存在房间探测失败：%v", err)
		}
		if i == 0 {
			statusExist, bodyExist, durExist = r.Status, append([]byte(nil), r.Body...), d
		}
	}

	if statusExist != http.StatusTooManyRequests {
		t.Errorf("限流对'存在'和'不存在'区别对待：不存在 → 429，存在 → %d。"+
			"这说明限流发生在存在性判断之后，429/404 的差异本身就是新的 oracle", statusExist)
	}

	// 429 的 body 必须逐字节一致（先按原样比，再退一步抹掉回显 id 比）
	if bodyNonexist != nil && bodyExist != nil {
		if bytes.Equal(bodyNonexist, bodyExist) {
			t.Logf("429 不可区分：存在/不存在两种输入的响应体逐字节一致（%d 字节）", len(bodyExist))
		} else {
			n1 := strings.ReplaceAll(string(bodyNonexist), "r_scan", "{ID}")
			n2 := strings.ReplaceAll(string(bodyExist), room.RoomID, "{ID}")
			if n1 == n2 {
				t.Logf("429 不可区分：抹掉回显 id 后一致（原始不同只因 body 回显了 id）")
			} else {
				t.Errorf("429 响应体在'存在'与'不存在'两种输入下不同，等于把 404 的泄漏换成了 429 的泄漏：\n"+
					"  不存在：%s\n  存在  ：%s", firstN(string(bodyNonexist), 200), firstN(string(bodyExist), 200))
			}
		}
	}

	// 429 的耗时也必须同量级
	if durNonexist > 0 && durExist > 0 {
		hi, lo := durExist, durNonexist
		if hi < lo {
			hi, lo = lo, hi
		}
		ratio := float64(hi) / float64(lo)
		t.Logf("429 耗时：不存在 %v / 存在 %v（比值 %.2f×）", durNonexist, durExist, ratio)
		if ratio > 5 {
			t.Errorf("429 在两种输入下耗时相差 %.1f 倍，可能走了不同分支", ratio)
		}
	}

	// ── 阶段 3：换一个**真实**来源，应当有独立配额 ──────────────────
	// 限流的价值在于"按攻击者计"，不是"按全局限"。如果一个人把桶打满、
	// 全世界都被连带 429，那这套限流本身就变成了 DoS 放大器。
	srv6 := srv.baseAt("::1")
	rX, err := doAt(srv6, http.MethodGet, "/rooms/r_otheraaaa", "", nil)
	if err != nil {
		t.Skipf("本机不可用 IPv6 回环（%v），无法构造第二个来源 IP，跳过按来源隔离的断言", err)
	}
	t.Logf("阶段3：换来源 ::1 后探测 → %d", rX.Status)

	switch rX.Status {
	case http.StatusNotFound:
		t.Logf("限流按来源隔离：127.0.0.1 把桶打满之后，::1 仍然拿到全新配额")
	case http.StatusTooManyRequests:
		t.Errorf("限流是全局的而不是按来源的：127.0.0.1 打满后 ::1 也被连带 429。" +
			"这会让一个攻击者把所有人挡在门外（DoS 放大器）")
	default:
		t.Errorf("换来源探测返回了意外状态 %d", rX.Status)
	}

	// 附带记录：限流用的"来源"是不是 TCP 对端而不是 X-Forwarded-For。
	// 这不影响正确性，但决定部署形态——反向代理后面所有请求都来自代理 IP，
	// 若不配可信代理，按来源限流会退化成全局桶。这条只做记录，不判失败。
	rFF, _ := doAtH(srv.base, http.MethodGet, "/rooms/r_ffprobe", "",
		map[string]string{"X-Forwarded-For": "203.0.113.7"}, nil)
	t.Logf("附注：带 X-Forwarded-For(203.0.113.7) 探测 → %d。"+
		"若为 429，说明限流键取的是 TCP 对端 IP；部署在反向代理后需配置可信代理，"+
		"否则全部用户会共享同一个桶", rFF.Status)
}
