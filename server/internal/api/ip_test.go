package api

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agchat/internal/config"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("CIDR %q 非法: %v", s, err)
	}
	return n
}

func req(remote, xff, xri string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/rooms/r_zzzz", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	if xri != "" {
		r.Header.Set("X-Real-IP", xri)
	}
	return r
}

// TestTrustedProxySemantics 可信代理语义，逐条钉死。
func TestTrustedProxySemantics(t *testing.T) {
	none := NewIPResolver(nil)
	trusted := NewIPResolver([]*net.IPNet{
		mustCIDR(t, "127.0.0.1/32"),
		mustCIDR(t, "10.0.0.0/8"),
	})

	cases := []struct {
		name     string
		resolver *IPResolver
		remote   string
		xff      string
		xri      string
		wantIP   string
		wantPeer string
	}{
		{
			name: "没配可信代理：XFF 一律忽略", resolver: none,
			remote: "127.0.0.1:5555", xff: "203.0.113.9",
			wantIP: "127.0.0.1", wantPeer: "127.0.0.1",
		},
		{
			name: "对端不在可信列表：XFF 忽略", resolver: trusted,
			remote: "8.8.8.8:5555", xff: "203.0.113.9",
			wantIP: "8.8.8.8", wantPeer: "8.8.8.8",
		},
		{
			name: "对端可信：取 XFF 里的客户端", resolver: trusted,
			remote: "127.0.0.1:5555", xff: "203.0.113.9",
			wantIP: "203.0.113.9", wantPeer: "127.0.0.1",
		},
		{
			// 攻击者自己塞了 1.2.3.4，真代理在右边追加了真实地址 —— 只能取最右侧非可信那个
			name: "伪造链：取最右侧非可信地址", resolver: trusted,
			remote: "127.0.0.1:5555", xff: "1.2.3.4, 203.0.113.9",
			wantIP: "203.0.113.9", wantPeer: "127.0.0.1",
		},
		{
			name: "多级代理：跳过可信段", resolver: trusted,
			remote: "127.0.0.1:5555", xff: "6.6.6.6, 10.0.0.5",
			wantIP: "6.6.6.6", wantPeer: "127.0.0.1",
		},
		{
			name: "整条链都可信：取最左", resolver: trusted,
			remote: "127.0.0.1:5555", xff: "10.0.0.7, 10.0.0.5",
			wantIP: "10.0.0.7", wantPeer: "127.0.0.1",
		},
		{
			name: "XFF 有非法项：整条作废，退回对端", resolver: trusted,
			remote: "127.0.0.1:5555", xff: "not-an-ip, 203.0.113.9",
			wantIP: "127.0.0.1", wantPeer: "127.0.0.1",
		},
		{
			name: "带端口的 XFF 项要能剥掉", resolver: trusted,
			remote: "127.0.0.1:5555", xff: "203.0.113.9:443",
			wantIP: "203.0.113.9", wantPeer: "127.0.0.1",
		},
		{
			name: "没有 XFF 时用 X-Real-IP", resolver: trusted,
			remote: "127.0.0.1:5555", xri: "198.51.100.7",
			wantIP: "198.51.100.7", wantPeer: "127.0.0.1",
		},
		{
			name: "XFF 优先于 X-Real-IP", resolver: trusted,
			remote: "127.0.0.1:5555", xff: "203.0.113.9", xri: "198.51.100.7",
			wantIP: "203.0.113.9", wantPeer: "127.0.0.1",
		},
	}

	for _, c := range cases {
		gotIP, gotPeer := c.resolver.Source(req(c.remote, c.xff, c.xri))
		if gotIP != c.wantIP || gotPeer != c.wantPeer {
			t.Errorf("%s: 得到 ip=%q peer=%q，期望 ip=%q peer=%q",
				c.name, gotIP, gotPeer, c.wantIP, c.wantPeer)
		}
	}
}

// TestRateLimitUsesResolvedIP 限流桶必须按解析后的来源计，
// 否则反代后面所有用户共用一个桶，一个攻击者就能把全站打成 429。
func TestRateLimitUsesResolvedIP(t *testing.T) {
	silentLogs(t)
	ts, srv := newServerWithHandle(t, config.Config{
		JoinRate: 1000, EnumRate: 2, TurnTimeout: time.Hour, Briefing: 30 * time.Second,
		TrustedProxies: []*net.IPNet{mustCIDR(t, "127.0.0.1/32")},
	})
	_ = srv

	probe := func(xff string) int {
		r, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/rooms/r_zzzz", nil)
		r.Header.Set("X-Forwarded-For", xff)
		res, err := ts.Client().Do(r)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}

	// 来源 A 用满额度
	if code := probe("203.0.113.9"); code != 404 {
		t.Fatalf("A 第 1 次应 404，得到 %d", code)
	}
	if code := probe("203.0.113.9"); code != 404 {
		t.Fatalf("A 第 2 次应 404，得到 %d", code)
	}
	if code := probe("203.0.113.9"); code != 429 {
		t.Fatalf("A 第 3 次应 429，得到 %d", code)
	}

	// 换一个来源 B：不应该被 A 的额度连带（这就是"按来源计而不是全局计"）
	if code := probe("198.51.100.7"); code != 404 {
		t.Fatalf("B 第 1 次应 404（不该被 A 拖累），得到 %d", code)
	}
}
