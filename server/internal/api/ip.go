package api

import (
	"net"
	"net/http"
	"strings"
)

// IPResolver 负责把"请求"翻译成"真实来源 IP"。
//
// 为什么需要它：直连时 TCP 对端就是客户端，但一放到 nginx / CDN 后面，
// 所有用户共享代理那一个 IP —— 按来源 IP 限流会退化成**全局桶**，
// 一个攻击者就能把全站打成 429。所以要能认代理转发的真实来源。
//
// 但**不能无条件信 X-Forwarded-For**：任何人都能自己加这个头，
// 无条件采信等于把限流键交给攻击者自己填（换一个 XFF 就换一个桶，限流形同虚设）。
// 所以只有 TCP 对端落在 -trusted-proxies 指定的 CIDR 内时才解析转发头。
type IPResolver struct {
	trusted []*net.IPNet
}

// NewIPResolver 用可信 CIDR 列表构造解析器；传 nil 表示谁都不信。
func NewIPResolver(trusted []*net.IPNet) *IPResolver {
	return &IPResolver{trusted: trusted}
}

// Source 返回 (解析后的来源 IP, TCP 对端 IP)。
//
//	ip   —— 用于限流键与审计日志，是"能代表客户端"的那个地址
//	peer —— TCP 对端，审计日志里一并保留，事后能区分"直连"还是"经过代理"
func (r *IPResolver) Source(req *http.Request) (ip, peer string) {
	peer = remoteHost(req.RemoteAddr)
	if r == nil || len(r.trusted) == 0 || !r.contains(peer) {
		// 对端不可信（或没配可信代理）→ 一律忽略转发头，用对端地址。
		// 这是默认行为，也是安全默认。
		return peer, peer
	}

	if xff := strings.TrimSpace(req.Header.Get("X-Forwarded-For")); xff != "" {
		if chain, ok := parseForwardedChain(xff); ok {
			// 从右往左找第一个"非可信"地址：右边是我们信任的代理，
			// 第一个不可信的就是真实客户端。这样即使客户端自己伪造了左边的条目，
			// 也影响不到结果。
			for i := len(chain) - 1; i >= 0; i-- {
				if r.contains(chain[i]) {
					continue
				}
				return chain[i], peer
			}
			// 整条链都是可信代理 → 最左边那个就是最初的客户端
			return chain[0], peer
		}
		// 头里有一项不是合法 IP：整条链不可信，宁可退回对端地址
	}

	if xri := strings.TrimSpace(req.Header.Get("X-Real-IP")); xri != "" {
		if net.ParseIP(xri) != nil {
			return xri, peer
		}
	}
	return peer, peer
}

func (r *IPResolver) contains(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	for _, n := range r.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseForwardedChain 解析 X-Forwarded-For。任何一项非法就整条作废。
func parseForwardedChain(h string) ([]string, bool) {
	var out []string
	for _, part := range strings.Split(h, ",") {
		s := strings.TrimSpace(part)
		if s == "" {
			continue
		}
		// 有些代理会带端口，例如 "203.0.113.9:5678"
		if host, _, err := net.SplitHostPort(s); err == nil {
			s = host
		}
		if net.ParseIP(s) == nil {
			return nil, false
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// remoteHost 取 RemoteAddr 里的主机部分。
func remoteHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}
