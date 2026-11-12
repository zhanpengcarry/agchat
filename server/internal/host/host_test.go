package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agchat/internal/invite"
)

// newTestHost 造一个临时 home 的宿主（不 Serve：测试直接调方法）。
func newTestHost(t *testing.T) *Host {
	t.Helper()
	home := t.TempDir()
	cfg := DefaultConfig()
	cfg.Home = home
	cfg.DBPath = filepath.Join(home, "agchat.db")
	cfg.Addr = "127.0.0.1:0"
	cfg.DisplayName = "测试宿主"
	cfg.OwnerName = "房主"
	h, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(h.Close)
	return h
}

func mustCreate(t *testing.T, h *Host, seats int) *CreateResp {
	t.Helper()
	res, err := h.Create(context.Background(), CreateReq{Topic: "要不要改月付", Seats: seats, Rounds: 3, Minutes: 10})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return res
}

func mustIssue(t *testing.T, h *Host, ref string, uses int) *IssueResp {
	t.Helper()
	res, err := h.Issue(context.Background(), IssueReq{Ref: ref, Uses: uses})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return res
}

// agentKeyForTest 造一把测试用 agentKey。
func agentKeyForTest(t *testing.T) (pubB64, nonce string, priv ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成 agentKey: %v", err)
	}
	return pubB64Of(pub), "nonce-" + strings.Repeat("a", 8), priv
}

// proofFor 造 join 证明（A/HOST-05：join 必须证明私钥在手）。
func proofFor(priv ed25519.PrivateKey, cardOrRef, nonce string) string {
	return pubB64Of(ed25519.Sign(priv, JoinProofMessage(cardOrRef, nonce)))
}

func pubB64Of(pub []byte) string { return b64url.EncodeToString(pub) }

// TestAdmitCaps 是 arch A-9 的最小负向用例：能力位不在卡上就必须拒。
func TestAdmitCaps(t *testing.T) {
	if ok, _ := admit([]string{invite.CapJoin, invite.CapRead}, invite.CapPost); ok {
		t.Fatal("caps 不含 post，却放行了 post")
	}
	if ok, _ := admit([]string{invite.CapJoin, invite.CapRead}, invite.CapJoin); !ok {
		t.Fatal("caps 含 join，却拒了 join")
	}
	if ok, _ := admit(nil, invite.CapVote); ok {
		t.Fatal("空 caps 放行了 vote")
	}
}

// TestJoinUsesExhausted：卡上的 uses 用完后，同一张卡再 join 必须被拒（理由码稳定）。
func TestJoinUsesExhausted(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 3)
	card := mustIssue(t, h, room.Ref, 1)
	pub1, nonce1, priv1 := agentKeyForTest(t)
	if _, err := h.Join(context.Background(), JoinReq{Card: card.Card, AgentPub: pub1, Nonce: nonce1, Proof: proofFor(priv1, card.Card, nonce1), Alias: "甲"}); err != nil {
		t.Fatalf("第一个 join 应成功: %v", err)
	}
	pub2, nonce2, priv2 := agentKeyForTest(t)
	_, err := h.Join(context.Background(), JoinReq{Card: card.Card, AgentPub: pub2, Nonce: nonce2, Proof: proofFor(priv2, card.Card, nonce2), Alias: "乙"})
	if err == nil {
		t.Fatal("uses=1 的卡第二个 join 竟然成功")
	}
	oe, ok := err.(*opError)
	if !ok || oe.Code != "invite_exhausted" {
		t.Fatalf("期望 invite_exhausted，实际 %v", err)
	}
}

// TestJoinUnknownInviteID：卡里的 ids 不在本场会的签发记录里 → 必须拒。
func TestJoinUnknownInviteID(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 3)
	mustIssue(t, h, room.Ref, 1) // 真实邀请集只有这一个 id
	rc := h.roomByRef(room.Ref)
	if rc == nil {
		t.Fatal("找不到房间")
	}
	// 伪造一张"id 属于别的会"的卡：用真 roomKey 签，但 ids 是捏造的。
	payload := invite.Payload{
		V: invite.CardVersion, API: invite.APIVersion,
		Ref: room.Ref, Key: pubB64(rc.pub),
		IDs: []string{"iv_deadbeef"}, Uses: 1, Seats: 3, IAT: nowMillis(),
		TH: invite.TopicHash("要不要改月付"), HN: "测试宿主", ON: "房主",
		Caps: []string{invite.CapJoin, invite.CapRead},
		RV:   invite.Rules{Res: 1, Vote: 1, Hash: 1}, N: "cardnonce",
	}
	human := []string{"房主 的 agent 邀你参加「要不要改月付」· 3 人 · 不限时前可进 · 本机",
		"卡号（念这 8 位核对）：" + invite.ShortRef(room.Ref)}
	cardText, err := invite.IssueLayered(payload, rc.priv, human)
	if err != nil {
		t.Fatalf("IssueLayered: %v", err)
	}
	pub, nonce, priv := agentKeyForTest(t)
	_, jerr := h.Join(context.Background(), JoinReq{Card: cardText, AgentPub: pub, Nonce: nonce, Proof: proofFor(priv, cardText, nonce), Alias: "丙"})
	if jerr == nil {
		t.Fatal("ids 与实际邀请集不符的卡竟然 join 成功")
	}
	oe, ok := jerr.(*opError)
	if !ok || oe.Code != "invite_unknown" {
		t.Fatalf("期望 invite_unknown，实际 %v", jerr)
	}
}

// TestJoinKeyReplaySameSeat：同一 joinKey 重放 → 同一座位、不重复扣 uses（A-14 / G8）。
func TestJoinKeyReplaySameSeat(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 3)
	card := mustIssue(t, h, room.Ref, 2)
	pub, nonce, priv := agentKeyForTest(t)
	first, err := h.Join(context.Background(), JoinReq{Card: card.Card, AgentPub: pub, Nonce: nonce, Proof: proofFor(priv, card.Card, nonce), Alias: "甲"})
	if err != nil {
		t.Fatalf("首次 join: %v", err)
	}
	second, err := h.Join(context.Background(), JoinReq{Card: card.Card, AgentPub: pub, Nonce: nonce, Proof: proofFor(priv, card.Card, nonce), Alias: "甲"})
	if err != nil {
		t.Fatalf("重放 join 不该失败: %v", err)
	}
	if first.AgentID != second.AgentID {
		t.Fatalf("重放换了座位：%s → %s", first.AgentID, second.AgentID)
	}
	if first.Token != second.Token {
		t.Fatal("重放返回了不同的 token（应返回同一座位凭证）")
	}
	rc := h.roomByRef(room.Ref)
	used := 0
	for _, s := range rc.seats {
		if s.InviteID == card.IDs[0] {
			used++
		}
	}
	if used != 1 {
		t.Fatalf("重放重复扣了 uses：used=%d（应为 1）", used)
	}
}

// TestReclaimByPubKey：座位私钥回来就能拿回自己的座位，不占新座位、不改 agentId。
//
// 这条守的是"沙箱是一次性的"这个现实：agent 的进程换一个，token 就没了。
// 如果认回不成立，一次性卡 + 满员会让 agent 再也回不到自己的座位。
func TestReclaimByPubKey(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 2)
	card := mustIssue(t, h, room.Ref, 1)
	pub, nonce, priv := agentKeyForTest(t)
	first, err := h.Join(context.Background(), JoinReq{Card: card.Card, AgentPub: pub, Nonce: nonce, Proof: proofFor(priv, card.Card, nonce), Alias: "甲"})
	if err != nil {
		t.Fatalf("首次 join: %v", err)
	}

	// ① 立刻用同一把私钥、不带卡、换个 nonce 回来：必须认回原座位。
	//    旧实例 1 秒前还活跃，alias 路径会撞 member_active，所以这里同时也锁住了 ForceReclaim。
	got, err := h.Join(context.Background(), JoinReq{Ref: room.Ref, AgentPub: pub, Nonce: nonce + "-2", Proof: proofFor(priv, room.Ref, nonce+"-2"), Alias: "甲"})
	if err != nil {
		t.Fatalf("凭私钥认回: %v", err)
	}
	if !got.Reclaimed {
		t.Fatal("没标记 reclaimed")
	}
	if got.AgentID != first.AgentID {
		t.Fatalf("认回换了座位：%s → %s", first.AgentID, got.AgentID)
	}
	if got.Token == first.Token {
		t.Fatal("认回没轮换 token（旧 token 应作废）")
	}
	rc := h.roomByRef(room.Ref)
	if len(rc.seats) != 1 {
		t.Fatalf("认回多占了座位：%d", len(rc.seats))
	}

	// ② 认回时卡已被用完也不能挡（invite_exhausted 不该出现）。
	// ③ 一把从没进过场的私钥：必须 404 not_seated，而不是悄悄开新座。
	otherPub, otherNonce, otherPriv := agentKeyForTest(t)
	_, oerr := h.Join(context.Background(), JoinReq{Ref: room.Ref, AgentPub: otherPub, Nonce: otherNonce, Proof: proofFor(otherPriv, room.Ref, otherNonce), Alias: "乙"})
	oe, ok := oerr.(*opError)
	if !ok || oe.Code != "not_seated" {
		t.Fatalf("期望 not_seated，实际 %v", oerr)
	}

	// ④ 只给 ref 又给一把空 pub：参数错误要说清楚。
	if _, err := h.Join(context.Background(), JoinReq{Ref: room.Ref, AgentPub: "x", Nonce: "n", Proof: "AAAA"}); err == nil {
		t.Fatal("坏 agentPub 竟然通过")
	}
}

func TestConsentDryRunDoesNotJoin(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 3)
	card := mustIssue(t, h, room.Ref, 1)
	pub, nonce, priv := agentKeyForTest(t)
	res, err := h.Join(context.Background(), JoinReq{Card: card.Card, AgentPub: pub, Nonce: nonce, Proof: proofFor(priv, card.Card, nonce), DryRun: true})
	if err != nil {
		t.Fatalf("dryRun: %v", err)
	}
	if res.Joined {
		t.Fatal("dryRun 竟然进场了")
	}
	if res.Consent == nil || res.Consent.Topic == "" || res.Consent.Estimate["high"] == 0 {
		t.Fatalf("同意门信息不全: %+v", res.Consent)
	}
	rc := h.roomByRef(room.Ref)
	if len(rc.seats) != 0 {
		t.Fatalf("dryRun 写了座位：%d", len(rc.seats))
	}
	if rc.snap.Agents != 0 {
		t.Fatalf("dryRun 推进了状态：agents=%d", rc.snap.Agents)
	}
}

// TestHTTPGuards：Host 头校验、无 CORS、status 需要控制面 token（A-24 / G13）。
func TestHTTPGuards(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Serve(ctx) }()
	// 等 Serve 写出 addr
	deadline := nowMillis() + 5000
	for h.Addr() == "" && nowMillis() < deadline {
		time.Sleep(10 * time.Millisecond)
	}
	addr := h.Addr()
	if addr == "" {
		cancel()
		t.Fatal("Serve 没起来")
	}
	defer func() { cancel(); <-done }()

	// 1) 正常 Host：/healthz 200，且没有 access-control-allow-origin
	req, _ := http.NewRequest("GET", "http://"+addr+"/healthz", nil)
	req.Host = addr
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz 状态 %d", resp.StatusCode)
	}
	if v := resp.Header.Get("access-control-allow-origin"); v != "" {
		t.Fatalf("出现了 access-control-allow-origin=%q（arch A-24 禁止）", v)
	}

	// 2) 伪造 Host（DNS rebinding）→ 403
	req2, _ := http.NewRequest("GET", "http://"+addr+"/healthz", nil)
	req2.Host = "evil.example.com"
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("bad host 请求: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != 403 {
		t.Fatalf("伪造 Host 应 403，实际 %d", resp2.StatusCode)
	}

	// 3) /v1/status 无控制面 token → 401
	resp3, err := http.Get("http://" + addr + "/v1/status?ref=" + room.Ref)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != 401 {
		t.Fatalf("无 token 的 status 应 401，实际 %d", resp3.StatusCode)
	}

	// 4) 带 token → 200
	req4, _ := http.NewRequest("GET", "http://"+addr+"/v1/status?ref="+room.Ref, nil)
	req4.Header.Set("authorization", "Bearer "+h.ctlToken)
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatalf("status+token: %v", err)
	}
	_ = resp4.Body.Close()
	if resp4.StatusCode != 200 {
		t.Fatalf("带 token 的 status 应 200，实际 %d", resp4.StatusCode)
	}
}

// TestBodyTooLarge：超过 4MB 的请求体必须给 413 且码可辨认。
//
// 为什么单独测：原先超长和"不是 JSON"都回 400 bad_json，排查时会往语法方向查，
// 而真实原因是体积。
func TestBodyTooLarge(t *testing.T) {
	h := newTestHost(t)
	mustCreate(t, h, 2)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Serve(ctx) }()
	deadline := nowMillis() + 5000
	for h.Addr() == "" && nowMillis() < deadline {
		time.Sleep(10 * time.Millisecond)
	}
	addr := h.Addr()
	if addr == "" {
		cancel()
		t.Fatal("Serve 没起来")
	}
	defer func() { cancel(); <-done }()

	big := `{"card":"` + strings.Repeat("A", 5<<20) + `"}`
	req, _ := http.NewRequest("POST", "http://"+addr+"/v1/join", strings.NewReader(big))
	req.Host = addr
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 413 {
		t.Fatalf("超长请求体应 413，实际 %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	e, _ := body["error"].(map[string]any)
	if e == nil || e["code"] != "body_too_large" {
		t.Fatalf("错误码不是 body_too_large: %v", body)
	}
}
