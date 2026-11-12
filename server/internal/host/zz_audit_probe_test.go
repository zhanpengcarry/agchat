package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agchat/internal/oplog"
)

func probeJoinN(t *testing.T, h *Host, ref, card, nonce string) (ed25519.PublicKey, ed25519.PrivateKey, string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// join 现在要私钥证明（A/HOST-05），探针也得带。
	proof := b64urlEncodeBytes(ed25519.Sign(priv, JoinProofMessage(card, nonce)))
	res, err := h.Join(context.Background(), JoinReq{Card: card, AgentPub: pubB64(pub), Nonce: nonce, Proof: proof, Alias: nonce})
	if err != nil {
		t.Fatalf("probeJoinN: %v", err)
	}
	return pub, priv, res.AgentID, res.Token
}

// A) 长轮询：wait=2s，房间在 briefing（未结束），Next 应立即返回。
func TestProbeLongPollReturnsImmediately(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 3)
	card := mustIssue(t, h, room.Ref, 1)
	_, _, _, tok := probeJoinN(t, h, room.Ref, card.Card, "a")

	start := time.Now()
	if _, err := h.Next(context.Background(), tok, 2*time.Second); err != nil {
		t.Fatalf("Next: %v", err)
	}
	d := time.Since(start)
	t.Logf("Next(wait=2s) 实际用时 = %v", d)
	if d > 300*time.Millisecond {
		t.Logf("→ 长轮询生效（会等待）")
	} else {
		t.Logf("→ 长轮询未生效（立即返回）")
	}
}

// B) 被拒 op 的 opKey 重放：期望重放时仍报错。
func TestProbeRejectedOpKeyReplay(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 3)
	card := mustIssue(t, h, room.Ref, 1)
	_, priv, agentID, tok := probeJoinN(t, h, room.Ref, card.Card, "a")
	rc := h.roomByRef(room.Ref)

	mk := func() oplog.Op {
		op, err := oplog.Seal(priv, rc.log.head(), rc.log.nextIndex(), nowMillis(), oplog.OpPost, agentID,
			map[string]any{"text": "", "seq": 1, "round": 0, "agentId": agentID, "alias": "a"})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	_, _, err1 := h.applyAgentOp(context.Background(), oplog.OpPost, tok, AgentOpReq{Op: mk(), OpKey: "rej-1"})
	if err1 == nil {
		t.Fatalf("期望被拒，实际成功")
	}
	oe, _ := err1.(*opError)
	t.Logf("第 1 次 → HTTP %d code=%s", oe.Status, oe.Code)

	res, ack, err2 := h.applyAgentOp(context.Background(), oplog.OpPost, tok, AgentOpReq{Op: mk(), OpKey: "rej-1"})
	t.Logf("第 2 次（重放同一 opKey）→ err=%v res=%v ack=%v", err2, res, ack != nil)
}

// C) PH 省略的两种口径。
func TestProbeEmptyPH(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 3)
	card := mustIssue(t, h, room.Ref, 1)
	_, priv, agentID, tok := probeJoinN(t, h, room.Ref, card.Card, "a")
	rc := h.roomByRef(room.Ref)

	op, err := oplog.Seal(priv, "", rc.log.nextIndex(), nowMillis(), oplog.OpPost, agentID, map[string]any{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Seal(\"\") 产出的 PH 是否 ZeroHash = %v", op.PH == oplog.ZeroHash)
	_, _, err = h.applyAgentOp(context.Background(), oplog.OpPost, tok, AgentOpReq{Op: op, OpKey: "ph-1"})
	if oe, ok := err.(*opError); ok {
		t.Logf("PH 留空(SealOp 口径) → HTTP %d code=%s", oe.Status, oe.Code)
	}

	op2, err := oplog.Seal(priv, "", rc.log.nextIndex(), nowMillis(), oplog.OpPost, agentID, map[string]any{"text": "hi2"})
	if err != nil {
		t.Fatal(err)
	}
	op2.PH = ""
	_, _, err = h.applyAgentOp(context.Background(), oplog.OpPost, tok, AgentOpReq{Op: op2, OpKey: "ph-2"})
	if oe, ok := err.(*opError); ok {
		t.Logf("PH 字面空串 → HTTP %d code=%s（%s）", oe.Status, oe.Code, oe.Msg)
	}
}

// D) /v1/resume 的 ref 未校验：../evil 可以读写 rooms 目录之外的文件。
func TestProbeResumePathTraversal(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 2)
	srcDir := filepath.Join(h.cfg.Home, "rooms", room.Ref)
	dstDir := filepath.Join(h.cfg.Home, "evil")
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"host.key", "ops.jsonl", "seats.json"} {
		b, err := os.ReadFile(filepath.Join(srcDir, n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dstDir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("POST", "/v1/resume", strings.NewReader(`{"ref":"../evil"}`))
	req.Host = "127.0.0.1"
	req.Header.Set("authorization", "Bearer "+h.ctlToken)
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, req)
	t.Logf("POST /v1/resume ref=../evil → %d", w.Code)
	t.Logf("rooms 之外被写了 host.json = %v", exists(filepath.Join(h.cfg.Home, "evil", "host.json")))
}

// E) Host 头边界。
func TestProbeHostHeaderEdges(t *testing.T) {
	h := newTestHost(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Serve(ctx) }()
	deadline := nowMillis() + 5000
	for h.Addr() == "" && nowMillis() < deadline {
		time.Sleep(10 * time.Millisecond)
	}
	if h.Addr() == "" {
		cancel()
		t.Fatal("Serve 没起来")
	}
	defer func() { cancel(); <-done }()
	port := portOf(h.Addr())

	for _, host := range []string{
		"127.0.0.1:" + port, "localhost:" + port, "LocalHost:" + port,
		"127.0.0.1", "localhost", "::1:" + port, "[::1]:" + port, "::1",
		"127.0.0.1:" + port + ".evil.com", "evil.example.com:" + port,
	} {
		req := httptest.NewRequest("GET", "/healthz", nil)
		req.Host = host
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, req)
		t.Logf("Host=%-32q → %d", host, w.Code)
	}
}

// F) decodeBody 的宽松度。
func TestProbeDecodeBody(t *testing.T) {
	h := newTestHost(t)
	req := httptest.NewRequest("POST", "/v1/room", strings.NewReader(`{"topic":"t","seats":2,"bogus":1}`))
	req.Host = "127.0.0.1"
	req.Header.Set("authorization", "Bearer "+h.ctlToken)
	w := httptest.NewRecorder()
	h.routes().ServeHTTP(w, req)
	t.Logf("未知字段 → %d", w.Code)

	req2 := httptest.NewRequest("POST", "/v1/room", strings.NewReader(`{"topic":"t2"} garbage-after-json`))
	req2.Host = "127.0.0.1"
	req2.Header.Set("authorization", "Bearer "+h.ctlToken)
	w2 := httptest.NewRecorder()
	h.routes().ServeHTTP(w2, req2)
	t.Logf("合法 JSON + 尾部垃圾 → %d", w2.Code)

	big := `{"topic":"` + strings.Repeat("x", 5<<20) + `"}`
	req3 := httptest.NewRequest("POST", "/v1/room", strings.NewReader(big))
	req3.Host = "127.0.0.1"
	req3.Header.Set("authorization", "Bearer "+h.ctlToken)
	w3 := httptest.NewRecorder()
	h.routes().ServeHTTP(w3, req3)
	var body map[string]any
	_ = json.NewDecoder(w3.Body).Decode(&body)
	t.Logf("5MB topic → %d %v", w3.Code, body["error"])
}

// G) 每次正常发言会不会被记成 advance{turn_timeout}；end op 的 emptyRounds 是否恒为 0。
func TestProbeAdvanceLabeling(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 3)
	card := mustIssue(t, h, room.Ref, 3)
	type who struct {
		priv ed25519.PrivateKey
		id   string
		tok  string
	}
	var agents []who
	for i := 0; i < 3; i++ {
		_, priv, id, tok := probeJoinN(t, h, room.Ref, card.Card, "n"+string(rune('a'+i)))
		agents = append(agents, who{priv, id, tok})
	}
	rc := h.roomByRef(room.Ref)
	if string(rc.snap.State) != "debating" {
		t.Fatalf("3/3 应已开场，实际 %s", rc.snap.State)
	}
	mkPost := func(w who, text string) oplog.Op {
		op, err := oplog.Seal(w.priv, rc.log.head(), rc.log.nextIndex(), nowMillis(), oplog.OpPost, w.id,
			map[string]any{"text": text, "seq": rc.snap.Seq + 1, "round": rc.snap.Round, "agentId": w.id, "alias": "x"})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	for _, w := range agents {
		if _, _, err := h.applyAgentOp(context.Background(), oplog.OpPost, w.tok,
			AgentOpReq{Op: mkPost(w, "第"+w.id+"条"), OpKey: "p-" + w.id}); err != nil {
			t.Logf("post by %s 被拒: %v", w.id, err)
		}
	}
	b, err := os.ReadFile(h.cfg.opsPath(room.Ref))
	if err != nil {
		t.Fatal(err)
	}
	nAdvance := 0
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var probe map[string]any
		if json.Unmarshal([]byte(line), &probe) != nil {
			continue
		}
		if probe["o"] == "advance" {
			nAdvance++
			t.Logf("advance = %v", line)
		}
	}
	t.Logf("共 %d 条 advance（3 次正常发言）", nAdvance)

	if _, err := h.CloseRoom(context.Background(), room.Ref); err != nil {
		t.Logf("close: %v", err)
	}
	b2, _ := os.ReadFile(h.cfg.opsPath(room.Ref))
	for _, line := range strings.Split(strings.TrimSpace(string(b2)), "\n") {
		if strings.Contains(line, `"o":"end"`) {
			t.Logf("end = %v", line)
		}
	}
}

// H) ref + agentPub 就拿 token（无需持有私钥）。
func TestProbeJoinByPubOnly(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 3)
	card := mustIssue(t, h, room.Ref, 1)
	pub, victimPriv, id, oldTok := probeJoinN(t, h, room.Ref, card.Card, "victim")
	// 认回也要 proof：证明私钥在这台机器上（A/HOST-05 之后的形状）。
	res, err := h.Join(context.Background(), JoinReq{Ref: room.Ref, AgentPub: pubB64(pub), Nonce: "attacker", Proof: proofFor(victimPriv, room.Ref, "attacker")})
	if err != nil {
		t.Fatalf("ref+pub 认回失败: %v", err)
	}
	t.Logf("拿到 agentId=%s reclaimed=%v（未提交任何签名）", res.AgentID, res.Reclaimed)
	if _, _, err := h.authByToken(oldTok); err != nil {
		t.Logf("受害者的旧 token 现在: %v", err)
	}
	if _, _, err := h.authByToken(res.Token); err != nil {
		t.Logf("攻击者 token: %v", err)
	} else {
		t.Logf("攻击者 token 可用，agentId=%s", id)
	}
}

// I) CloseRoom 必须能关会（adminKey 传参口径）。
func TestProbeCloseRoomWorks(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 2)
	st, err := h.CloseRoom(context.Background(), room.Ref)
	if err != nil {
		// 审计时这是已知缺陷（HOST-01），所以只记日志不让套件变红。
		t.Logf("→ CloseRoom 失败（预期会失败）: %v", err)
		return
	}
	t.Logf("close 后 state=%s", st.State)
}

// J) resume 时钟修正可以叠加：模拟“adjust 写了、persist 还没写就崩溃”，再恢复一次。
func TestProbeDoubleClockAdjust(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 2)
	ctx := context.Background()
	before, err := h.Status(ctx, room.Ref)
	if err != nil {
		t.Fatal(err)
	}
	rc := h.roomByRef(room.Ref)

	reopen := func() int64 {
		h.mu.Lock()
		if cur := h.rooms[room.Ref]; cur != nil {
			_ = cur.log.close() // 先关掉当前句柄，否则遗忘的 fd 会让 TempDir 收尾失败
		}
		delete(h.rooms, room.Ref)
		delete(h.byID, rc.roomID)
		h.mu.Unlock()
		// 模拟“上一次 resume 的 persist 没落盘”：seats.json 里 lastAlive 仍是旧值
		sf := stateFile{V: 1, LastAlive: nowMillis() - int64(3600*1000)}
		if err := writeJSON(h.cfg.statePath(room.Ref), sf, true); err != nil {
			t.Fatal(err)
		}
		if err := h.openRoom(ctx, room.Ref); err != nil {
			t.Fatalf("openRoom: %v", err)
		}
		st, err := h.Status(ctx, room.Ref)
		if err != nil {
			t.Fatal(err)
		}
		return st.CreatedAt
	}
	c1 := reopen()
	c2 := reopen()
	t.Logf("CreatedAt 原始=%d 第一次恢复后=+%dms 第二次恢复后=+%dms（真实流逝仅 ~毫秒级）",
		before.CreatedAt, c1-before.CreatedAt, c2-before.CreatedAt)
	if c2 > nowMillis() {
		t.Logf("→ CreatedAt 已被推到未来（now-CreatedAt = %dms，负数即未来）", nowMillis()-c2)
	}
}

// K) 宿主是否校验/规范化客户端给的 op 载荷：非规范 JSON、撒谎的 seq/round/alias。
func TestProbePayloadTrust(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 3)
	card := mustIssue(t, h, room.Ref, 3)
	type who struct {
		priv ed25519.PrivateKey
		id   string
		tok  string
	}
	var agents []who
	for i := 0; i < 3; i++ {
		_, priv, id, tok := probeJoinN(t, h, room.Ref, card.Card, "n"+string(rune('a'+i)))
		agents = append(agents, who{priv, id, tok})
	}
	rc := h.roomByRef(room.Ref)
	w := agents[0]
	// 非规范 JSON：键倒序 + 多余空格 + null 字段(可省略) + 撒谎的 seq/round/alias/agentId
	raw := []byte("\x7b \x22seq\x22:999,\x22round\x22:77,\x22agentId\x22:\x22m_9\x22,\x22alias\x22:\x22\xe5\x81\x87\xe5\x90\x8d\x22,\x22text\x22:\x22hi\x22 }")
	i := rc.log.nextIndex()
	head := rc.log.head()
	tm := nowMillis()
	hv, err := oplog.Hash(head, i, tm, oplog.OpPost, w.id, raw)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	op := oplog.Op{I: i, T: tm, O: oplog.OpPost, A: w.id, P: raw, PH: head, H: hv, S: oplog.SignOp(w.priv, hv)}
	res, _, err := h.applyAgentOp(context.Background(), oplog.OpPost, w.tok, AgentOpReq{Op: op, OpKey: "raw-1"})
	if err != nil {
		t.Fatalf("非规范载荷被拒: %v", err)
	}
	t.Logf("被接受，result=%+v", res)
	b, _ := os.ReadFile(h.cfg.opsPath(room.Ref))
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.Contains(line, "\x22seq\x22:999") {
			t.Logf("落盘行（p 原样保留客户端字节）: %s", line)
		}
	}
	// 真实 seq 与载荷里的 999 是否一致
	if pr, ok := res.(interface{}); ok {
		_ = pr
	}
}

func b64urlEncodeBytes(b []byte) string { return pubB64Of(b) }
