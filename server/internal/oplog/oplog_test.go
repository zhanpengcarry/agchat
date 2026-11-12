package oplog

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testPriv 是种子确定的测试私钥（不是秘密）。
func testPriv(seed string) ed25519.PrivateKey {
	sum := sha256.Sum256([]byte(seed))
	return ed25519.NewKeyFromSeed(sum[:])
}

func mustSeal(t *testing.T, priv ed25519.PrivateKey, prev string, i int, o, a string, payload any) Op {
	t.Helper()
	op, err := Seal(priv, prev, i, int64(1765000000000+i), o, a, payload)
	if err != nil {
		t.Fatalf("Seal(%d,%s): %v", i, o, err)
	}
	return op
}

// TestCanonicalJSONStable 钉住"同一结构不同构造顺序 → 字节完全相同"。
func TestCanonicalJSONStable(t *testing.T) {
	// 同样的内容，三种插入顺序。
	a := map[string]any{}
	a["z"] = 1
	a["a"] = "x"
	a["m"] = []any{true, int64(7)}

	b := map[string]any{}
	b["m"] = []any{true, int64(7)}
	b["a"] = "x"
	b["z"] = 1

	c := map[string]any{}
	c["a"] = "x"
	c["z"] = 1
	c["m"] = []any{true, int64(7)}

	ra, err := CanonicalJSON(a)
	if err != nil {
		t.Fatalf("CanonicalJSON(a): %v", err)
	}
	rb, err := CanonicalJSON(b)
	if err != nil {
		t.Fatalf("CanonicalJSON(b): %v", err)
	}
	rc, err := CanonicalJSON(c)
	if err != nil {
		t.Fatalf("CanonicalJSON(c): %v", err)
	}
	if !bytes.Equal(ra, rb) || !bytes.Equal(ra, rc) {
		t.Fatalf("构造顺序影响了字节:\n a=%s\n b=%s\n c=%s", ra, rb, rc)
	}
	want := `{"a":"x","m":[true,7],"z":1}`
	if string(ra) != want {
		t.Fatalf("规范编码 = %s，期望 %s（键升序、无空格）", ra, want)
	}
	// 幂等：对规范字节再做一次规范编码，字节不变。
	again, err := CanonicalJSON(ra)
	if err != nil {
		t.Fatalf("CanonicalJSON(ra): %v", err)
	}
	if !bytes.Equal(ra, again) {
		t.Fatalf("规范编码不幂等: %s != %s", ra, again)
	}
	// 嵌套对象同样键升序。
	nested, err := CanonicalJSON(map[string]any{"b": map[string]any{"y": 2, "x": 1}})
	if err != nil {
		t.Fatalf("nested: %v", err)
	}
	if string(nested) != `{"b":{"x":1,"y":2}}` {
		t.Fatalf("嵌套键没升序: %s", nested)
	}
	// 结构体也走同一条路径（op 载荷就是这么来的）。
	type sample struct {
		B string `json:"b"`
		A int    `json:"a"`
	}
	got, err := CanonicalJSON(sample{B: "x", A: 1})
	if err != nil {
		t.Fatalf("struct: %v", err)
	}
	if string(got) != `{"a":1,"b":"x"}` {
		t.Fatalf("结构体字段顺序没被规范化: %s", got)
	}
	// HTML 敏感字符不转义（否则同一段文本会有两种字节形态）。
	html, err := CanonicalJSON(map[string]any{"t": "<b>&</b>"})
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	if string(html) != `{"t":"<b>&</b>"}` {
		t.Fatalf("字符串被 HTML 转义了: %s", html)
	}
}

// TestCanonicalJSONRejectsFloatAndNull 钉住 §10.2⑦。
func TestCanonicalJSONRejectsFloatAndNull(t *testing.T) {
	cases := map[string]any{
		"float":     map[string]any{"x": 1.5},
		"float 字面量": json.RawMessage(`{"x":1e3}`),
		"null 成员":   map[string]any{"x": nil},
		"null 嵌套":   json.RawMessage(`{"a":{"b":null}}`),
		"null 数组元素": json.RawMessage(`{"a":[1,null]}`),
		"顶层 null":   json.RawMessage(`null`),
		"空 array":   nil,
		"半截 json":   json.RawMessage(`{"a":`),
		"两个值拼一起":    json.RawMessage(`{"a":1}{"b":2}`),
	}
	for name, v := range cases {
		if _, err := CanonicalJSON(v); !errors.Is(err, ErrBadJSON) {
			t.Fatalf("%s：期望 ErrBadJSON，实际 %v", name, err)
		}
	}
	// 整数 / 负数 / 空数组 都是允许的。
	ok := []any{
		map[string]any{"x": 0},
		map[string]any{"x": -12},
		[]any{},
		map[string]any{"a": []any{}},
	}
	for i, v := range ok {
		if _, err := CanonicalJSON(v); err != nil {
			t.Fatalf("合法输入 #%d 被拒: %v", i, err)
		}
	}
}

// TestHashShapeAndValidation 钉住 h = sha256("agchat-op:v1"+0x00+canonical{i,t,o,a,p,ph})。
func TestHashShapeAndValidation(t *testing.T) {
	payload := []byte(`{"text":"hi"}`)
	h, err := Hash("", 1, 1765000000000, OpPost, "m_2", payload)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if len(h) != 64 || !isLowerHex(h) {
		t.Fatalf("h 不是 64 位小写 hex: %q", h)
	}
	// 纯函数：同样输入 → 同样 hash。
	h2, err := Hash(ZeroHash, 1, 1765000000000, OpPost, "m_2", payload)
	if err != nil {
		t.Fatalf("Hash(ZeroHash): %v", err)
	}
	if h != h2 {
		t.Fatalf("prevHex='' 与 ZeroHash 应当等价: %s != %s", h, h2)
	}
	// 手工重算一遍，证明域前缀与规范编码口径一致。
	body, err := CanonicalJSON(map[string]any{
		"i": 1, "t": int64(1765000000000), "o": OpPost, "a": "m_2",
		"p": json.RawMessage(`{"text":"hi"}`), "ph": ZeroHash,
	})
	if err != nil {
		t.Fatalf("canonical body: %v", err)
	}
	sum := sha256.Sum256(append([]byte("agchat-op:v1\x00"), body...))
	if got := hexString(sum[:]); got != h {
		t.Fatalf("手工重算 %s != Hash %s", got, h)
	}
	// 载荷以规范形式入域：换个写法（空格/键序）但语义相同 → 同一个 h。
	loose, err := Hash("", 1, 1765000000000, OpPost, "m_2", []byte(`{ "text" : "hi" }`))
	if err != nil {
		t.Fatalf("Hash(loose): %v", err)
	}
	if loose != h {
		t.Fatalf("语义相同的载荷应得到同一个 h: %s != %s", loose, h)
	}
	// 任何一个字段变化都必须改 h。
	variants := []struct {
		name string
		h    string
	}{
		{"i", mustHash(t, "", 2, 1765000000000, OpPost, "m_2", payload)},
		{"t", mustHash(t, "", 1, 1765000000001, OpPost, "m_2", payload)},
		{"o", mustHash(t, "", 1, 1765000000000, OpVote, "m_2", payload)},
		{"a", mustHash(t, "", 1, 1765000000000, OpPost, "m_3", payload)},
		{"p", mustHash(t, "", 1, 1765000000000, OpPost, "m_2", []byte(`{"text":"hi!"}`))},
		{"ph", mustHash(t, "ff"+strings.Repeat("0", 62), 1, 1765000000000, OpPost, "m_2", payload)},
	}
	for _, v := range variants {
		if v.h == h {
			t.Fatalf("改 %s 后 h 没变", v.name)
		}
	}
	// 非法输入 fail closed。
	if _, err := Hash("", 0, 1765000000000, OpPost, "m", payload); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("i=0 期望 ErrInvalidHash，实际 %v", err)
	}
	if _, err := Hash("", 1, 0, OpPost, "m", payload); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("t=0 期望 ErrInvalidHash，实际 %v", err)
	}
	if _, err := Hash("", 1, 1765000000000, "", "m", payload); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("空 o 期望 ErrInvalidHash，实际 %v", err)
	}
	if _, err := Hash("", 1, 1765000000000, OpPost, "", payload); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("空 a 期望 ErrInvalidHash，实际 %v", err)
	}
	if _, err := Hash("not-a-hash", 1, 1765000000000, OpPost, "m", payload); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("坏 ph 期望 ErrInvalidHash，实际 %v", err)
	}
	if _, err := Hash("", 1, 1765000000000, OpPost, "m", nil); !errors.Is(err, ErrBadJSON) {
		t.Fatalf("空载荷期望 ErrBadJSON，实际 %v", err)
	}
	if _, err := Hash("", 1, 1765000000000, OpPost, "m", []byte(`{"x":1.5}`)); !errors.Is(err, ErrBadJSON) {
		t.Fatalf("浮点载荷期望 ErrBadJSON，实际 %v", err)
	}
}

// TestSignVerifySig 钉住签名域与"跨域不可复用"。
func TestSignVerifySig(t *testing.T) {
	priv := testPriv("op-signer")
	pub := priv.Public().(ed25519.PublicKey)
	h, err := Hash("", 1, 1765000000000, OpPost, "m_2", []byte(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	sig := SignOp(priv, h)
	if sig == "" {
		t.Fatal("SignOp 返回空")
	}
	if err := VerifySig(h, sig, pub); err != nil {
		t.Fatalf("VerifySig: %v", err)
	}
	// 换 h / 换公钥 / 坏编码 都必须失败。
	if err := VerifySig(strings.Repeat("a", 64), sig, pub); !errors.Is(err, ErrInvalidSig) {
		t.Fatalf("换 h 期望 ErrInvalidSig，实际 %v", err)
	}
	if err := VerifySig(h, sig, testPriv("other").Public().(ed25519.PublicKey)); !errors.Is(err, ErrInvalidSig) {
		t.Fatalf("换公钥期望 ErrInvalidSig，实际 %v", err)
	}
	if err := VerifySig(h, "not base64 !!", pub); !errors.Is(err, ErrInvalidSig) {
		t.Fatalf("坏编码期望 ErrInvalidSig，实际 %v", err)
	}
	if err := VerifySig(h, sig[:10], pub); !errors.Is(err, ErrInvalidSig) {
		t.Fatalf("短签名期望 ErrInvalidSig，实际 %v", err)
	}
	// SignOp 的入参防御。
	if got := SignOp(ed25519.PrivateKey{1, 2, 3}, h); got != "" {
		t.Fatalf("坏私钥应返回空，实际 %q", got)
	}
	if got := SignOp(priv, ZeroHash); got != "" {
		t.Fatalf("ZeroHash 不该被签，实际 %q", got)
	}
	if got := SignOp(priv, "zz"); got != "" {
		t.Fatalf("坏 h 应返回空，实际 %q", got)
	}
	// 门禁 G2：本包**不提供**通用 Sign()，只有带域的 SignOp —— 用错域写不出来。
	// （这一条由编译期保证：oplog.Sign 不存在；这里只钉住"域确实是 op 域"。）
	// 跨域复用：把 ack 域的签名当 op 签名用，必须验不过。
	ack, err := SignAck(priv, "ref-x", 1, h, 1765000000000)
	if err != nil {
		t.Fatalf("SignAck: %v", err)
	}
	if err := VerifySig(h, ack.S, pub); !errors.Is(err, ErrInvalidSig) {
		t.Fatalf("ack 签名冒充 op 签名竟然通过（跨域复用）: %v", err)
	}
	// `s` 不进 hash：改 s 不影响 h。
	op, err := Seal(priv, "", 1, 1765000000000, OpPost, "m_2", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	op.S = "AAAA"
	again, err := HashOp(op)
	if err != nil {
		t.Fatalf("HashOp: %v", err)
	}
	if again != h {
		t.Fatalf("s 字段参与了 hash: %s != %s", again, h)
	}
}

// TestAckSignVerify 钉住收据：Ack{ref,i,h,t,s}，由 roomKey 签（§10.2③）。
func TestAckSignVerify(t *testing.T) {
	priv := testPriv("room-key")
	pub := priv.Public().(ed25519.PublicKey)
	h, err := Hash("", 1, 1765000000000, OpPost, "m_2", []byte(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	a, err := SignAck(priv, "ref-abc", 1, h, 1765000000123)
	if err != nil {
		t.Fatalf("SignAck: %v", err)
	}
	if a.Ref != "ref-abc" || a.I != 1 || a.H != h || a.T != 1765000000123 || a.S == "" {
		t.Fatalf("ack 字段不对: %+v", a)
	}
	if err := VerifyAck(a, pub); err != nil {
		t.Fatalf("VerifyAck: %v", err)
	}
	// 任何一个字段被改都必须失败（ref 也进签名体）。
	tampered := []Ack{a, a, a, a, a}
	tampered[0].Ref = "ref-other"
	tampered[1].I = 2
	tampered[2].H = strings.Repeat("b", 64)
	tampered[3].T = a.T + 1
	tampered[4].S = flipB64(a.S)
	for i, bad := range tampered {
		if err := VerifyAck(bad, pub); !errors.Is(err, ErrInvalidSig) {
			t.Fatalf("篡改 #%d 期望 ErrInvalidSig，实际 %v", i, err)
		}
	}
	if err := VerifyAck(a, testPriv("other").Public().(ed25519.PublicKey)); !errors.Is(err, ErrInvalidSig) {
		t.Fatalf("换公钥期望 ErrInvalidSig，实际 %v", err)
	}
	// 缺 ref 的 ack 不许签。
	if _, err := SignAck(priv, "", 1, h, 1); !errors.Is(err, ErrInvalidSig) {
		t.Fatalf("空 ref 期望 ErrInvalidSig，实际 %v", err)
	}
	// ack 的签名域与 op 不同：拿 op 签名冒充 ack 必须失败。
	opSig := SignOp(priv, h)
	if err := VerifyAck(Ack{Ref: a.Ref, I: a.I, H: a.H, T: a.T, S: opSig}, pub); !errors.Is(err, ErrInvalidSig) {
		t.Fatalf("op 签名冒充 ack 竟然通过（跨域复用）: %v", err)
	}
}

// TestChainTamper 钉住链的完整性：改一条 op 的 payload → 链断，并指出第几条。
func TestChainTamper(t *testing.T) {
	priv := testPriv("chain")
	pub := priv.Public().(ed25519.PublicKey)
	key := func(Op) (ed25519.PublicKey, error) { return pub, nil }

	ops := []Op{
		mustSeal(t, priv, ZeroHash, 1, OpCreate, "host", map[string]any{"ref": "ref-x", "seats": 3}),
		mustSeal(t, priv, "", 2, OpJoin, "host", map[string]any{"inviteId": "iv_1", "alias": "乙"}),
		mustSeal(t, priv, "", 3, OpPost, "m_2", map[string]any{"text": "你好", "alias": "乙"}),
	}
	// 正确地把 ph 串起来（Seal 需要上一条的 h，这里手工补链，模拟宿主的追加路径）。
	for i := 1; i < len(ops); i++ {
		ops[i].PH = ops[i-1].H
		ops[i], _ = SealOp(ops[i], priv)
	}
	// SealOp 依赖 i/t/o/a/p/ph 重算 h 与 s；确认链是闭合的。
	if pos, err := VerifyChain(ops, key); err != nil {
		t.Fatalf("合法链被拒（第 %d 条）: %v", pos, err)
	}
	if HeadOf(ops) != ops[len(ops)-1].H {
		t.Fatalf("HeadOf=%s，末条 h=%s", HeadOf(ops), ops[len(ops)-1].H)
	}

	// ① 改第 2 条 op 的 payload → 必须断在第 2 条。
	tampered := append([]Op{}, ops...)
	tampered[1].P = json.RawMessage(`{"inviteId":"iv_1","alias":"丙"}`)
	pos, err := VerifyChain(tampered, key)
	if !errors.Is(err, ErrChainBroken) {
		t.Fatalf("改 payload 期望 ErrChainBroken，实际 %v", err)
	}
	if pos != 2 {
		t.Fatalf("改 payload 应断在第 2 条，实际第 %d 条", pos)
	}

	// ② 改第 1 条的 h → 断在第 1 条。
	tampered = append([]Op{}, ops...)
	tampered[0].H = strings.Repeat("c", 64)
	if pos, err := VerifyChain(tampered, key); !errors.Is(err, ErrChainBroken) || pos != 1 {
		t.Fatalf("改 h 期望第 1 条 ErrChainBroken，实际第 %d 条 %v", pos, err)
	}

	// ③ 改第 3 条的签名 → 断在第 3 条。
	tampered = append([]Op{}, ops...)
	tampered[2].S = flipB64(tampered[2].S)
	if pos, err := VerifyChain(tampered, key); !errors.Is(err, ErrChainBroken) || pos != 3 {
		t.Fatalf("改签名期望第 3 条 ErrChainBroken，实际第 %d 条 %v", pos, err)
	}

	// ④ 删掉中间一条（链断在"下一条的 ph 对不上"）。
	gap := []Op{ops[0], ops[2]}
	if pos, err := VerifyChain(gap, key); !errors.Is(err, ErrChainBroken) || pos != 2 {
		t.Fatalf("缺条期望第 2 条 ErrChainBroken，实际第 %d 条 %v", pos, err)
	}

	// ⑤ 换作者公钥（伪签）。
	if pos, err := VerifyChain(ops, func(Op) (ed25519.PublicKey, error) {
		return testPriv("imposter").Public().(ed25519.PublicKey), nil
	}); !errors.Is(err, ErrChainBroken) || pos != 1 {
		t.Fatalf("伪签期望第 1 条 ErrChainBroken，实际第 %d 条 %v", pos, err)
	}

	// ⑥ 空链是合法的（已提交前缀可以为空），HeadOf 给 ZeroHash。
	if pos, err := VerifyChain(nil, key); err != nil {
		t.Fatalf("空链不该报错（%d）: %v", pos, err)
	}
	if HeadOf(nil) != ZeroHash {
		t.Fatalf("空链 HeadOf 应为 ZeroHash")
	}
}

// TestTranscriptHashSensitive 钉住"改一个字 → 哈希变"。
func TestTranscriptHashSensitive(t *testing.T) {
	reply := 3
	msgs := []TranscriptMsg{
		{I: 1, Seq: 1, Alias: "甲", Round: 1, Text: "我建议月付"},
		{I: 2, Seq: 2, Alias: "乙", Round: 1, Text: "同意", ReplyTo: &reply, Support: "support-1"},
	}
	h, err := TranscriptHash(msgs)
	if err != nil {
		t.Fatalf("TranscriptHash: %v", err)
	}
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
		t.Fatalf("transcriptHash 形状不对（应与 decisionHash 一致）: %q", h)
	}
	// 确定性。
	again, err := TranscriptHash(append([]TranscriptMsg{}, msgs...))
	if err != nil {
		t.Fatalf("TranscriptHash(again): %v", err)
	}
	if again != h {
		t.Fatalf("同样的消息得到不同 hash: %s != %s", again, h)
	}
	// 改一个字 → 必须变。
	mutate := []func([]TranscriptMsg){
		func(m []TranscriptMsg) { m[0].Text = "我建议月付。" },
		func(m []TranscriptMsg) { m[1].Text = "同意了" },
		func(m []TranscriptMsg) { m[0].Alias = "丙" },
		func(m []TranscriptMsg) { m[0].Round = 2 },
		func(m []TranscriptMsg) { m[1].Seq = 9 },
		func(m []TranscriptMsg) { m[1].ReplyTo = nil },
		func(m []TranscriptMsg) { m[1].Support = "support-2" },
		func(m []TranscriptMsg) { m[0].I = 10 },
		func(m []TranscriptMsg) { _ = m },
	}
	for i, fn := range mutate[:len(mutate)-1] {
		copied := append([]TranscriptMsg{}, msgs...)
		fn(copied)
		got, err := TranscriptHash(copied)
		if err != nil {
			t.Fatalf("mutation #%d: %v", i, err)
		}
		if got == h {
			t.Fatalf("mutation #%d 改了内容但 hash 没变", i)
		}
	}
	// 顺序不同 → 不同（数组是顺序敏感的）。
	swapped := []TranscriptMsg{msgs[1], msgs[0]}
	if got, err := TranscriptHash(swapped); err != nil || got == h {
		t.Fatalf("换顺序后 hash 应改变（err=%v got=%s）", err, got)
	}
	// 空转写稿也有稳定的 hash。
	empty, err := TranscriptHash(nil)
	if err != nil {
		t.Fatalf("TranscriptHash(nil): %v", err)
	}
	emptySlice, err := TranscriptHash([]TranscriptMsg{})
	if err != nil {
		t.Fatalf("TranscriptHash(空切片): %v", err)
	}
	if empty != emptySlice {
		t.Fatal("nil 与空切片应得到同一个 hash")
	}
	// alias 缺失必须拒（B-13：不许回查库补 alias）。
	if _, err := TranscriptHash([]TranscriptMsg{{I: 1, Seq: 1, Round: 1, Text: "x"}}); !errors.Is(err, ErrBadJSON) {
		t.Fatalf("缺 alias 期望 ErrBadJSON，实际 %v", err)
	}
	if _, err := TranscriptHash([]TranscriptMsg{{I: 0, Seq: 1, Alias: "甲", Round: 1}}); !errors.Is(err, ErrBadJSON) {
		t.Fatalf("i=0 期望 ErrBadJSON，实际 %v", err)
	}
}

// TestOpJSONKeys 钉住 ops.jsonl 的行结构：JSON 键严格是 i,t,o,a,p,ph,h,s。
func TestOpJSONKeys(t *testing.T) {
	priv := testPriv("keys")
	op := mustSeal(t, priv, ZeroHash, 1, OpPost, "m_2", map[string]any{"text": "hi"})
	raw, err := CanonicalJSON(op)
	if err != nil {
		t.Fatalf("CanonicalJSON(op): %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("op 不是 JSON 对象: %v", err)
	}
	want := []string{"a", "h", "i", "o", "p", "ph", "s", "t"}
	if len(m) != len(want) {
		t.Fatalf("op 键数 %d，期望 %d: %s", len(m), len(want), raw)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("op 缺键 %q: %s", k, raw)
		}
	}
	// p 必须是内嵌对象（不是 base64 字符串）。
	if !strings.HasPrefix(string(m["p"]), "{") {
		t.Fatalf("p 不是内嵌 JSON: %s", m["p"])
	}
	// 行内不能有换行（jsonl 一行一条）。
	if strings.Contains(string(raw), "\n") {
		t.Fatalf("规范编码里有换行: %s", raw)
	}
}

// TestReasonEnum 钉住 §10.2⑥ 的 reason 闭枚。
func TestReasonEnum(t *testing.T) {
	for _, r := range []string{
		ReasonTurnTimeout, ReasonRoundIdle, ReasonRoundsDone, ReasonBudget,
		ReasonTimeLimit, ReasonVotesComplete, ReasonSummaryDone, ReasonSummaryTimeout,
		ReasonNoWinner, ReasonAdminClose, ReasonResumeClockAdjust,
	} {
		if !KnownReason(r) {
			t.Fatalf("%q 应当已知", r)
		}
	}
	if KnownReason("i_said_so") {
		t.Fatal("未知 reason 不该通过")
	}
	// Reasons 必须升序（KnownReason 用二分）。
	for i := 1; i < len(Reasons); i++ {
		if Reasons[i-1] >= Reasons[i] {
			t.Fatalf("Reasons 未升序: %q >= %q", Reasons[i-1], Reasons[i])
		}
	}
}

// ───────────────────────────── bundle ─────────────────────────────

func testBundle(t *testing.T) Bundle {
	t.Helper()
	priv := testPriv("bundle-room")
	ops := []Op{mustSeal(t, priv, ZeroHash, 1, OpCreate, "host", map[string]any{"ref": "ref-x", "seats": 1})}
	ops = append(ops, mustSeal(t, priv, ops[0].H, 2, OpPost, "m_1", map[string]any{"text": "hi", "alias": "甲"}))
	ack, err := SignAck(priv, "ref-x", 2, ops[1].H, 1765000000002)
	if err != nil {
		t.Fatalf("SignAck: %v", err)
	}
	return Bundle{
		Manifest: Manifest{
			Ref:          "ref-x",
			Protocol:     Protocol,
			ResultSchema: "1",
			VoteRule:     "1",
			HashDomain:   HashDomain,
			Seats:        1,
			InviteIDs:    []string{"iv_1"},
			MaxRounds:    6,
			MaxAgents:    2,
			MaxMinutes:   30,
			MaxOutput:    1000,
			Topic:        "月付定价",
		},
		Ops:            ops,
		Acks:           []Ack{ack},
		Result:         json.RawMessage(`{"decisionSet":{"hash":"sha256:abc"},"state":"ended"}`),
		DecisionHash:   "sha256:deadbeef",
		TranscriptHash: "sha256:cafebabe",
	}
}

// TestBundleRoundTrip 钉住 zip 的读写与字节可复现性。
func TestBundleRoundTrip(t *testing.T) {
	b := testBundle(t)
	var buf1, buf2 bytes.Buffer
	if err := WriteBundle(&buf1, b); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	if err := WriteBundle(&buf2, b); err != nil {
		t.Fatalf("WriteBundle(2): %v", err)
	}
	if !bytes.Equal(buf1.Bytes(), buf2.Bytes()) {
		t.Fatal("同样的内容写出不同的字节（zip 时间戳没固定？）")
	}
	got, err := ReadBundle(bytes.NewReader(buf1.Bytes()))
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}
	if got.Manifest.Ref != b.Manifest.Ref || got.Manifest.Seats != b.Manifest.Seats {
		t.Fatalf("manifest 往返不一致: %+v", got.Manifest)
	}
	if got.Manifest.OpsVersion != OpsVersion || got.Manifest.BundleVersion != BundleVersion {
		t.Fatalf("manifest 版本号没补默认值: %+v", got.Manifest)
	}
	if len(got.Ops) != len(b.Ops) || len(got.Acks) != len(b.Acks) {
		t.Fatalf("ops/acks 条数不一致: %d/%d", len(got.Ops), len(got.Acks))
	}
	for i := range b.Ops {
		if got.Ops[i].H != b.Ops[i].H || got.Ops[i].I != b.Ops[i].I {
			t.Fatalf("第 %d 条 op 往返不一致", i+1)
		}
	}
	if got.DecisionHash != b.DecisionHash || got.TranscriptHash != b.TranscriptHash {
		t.Fatalf("声明值往返不一致: %+v", got)
	}
	if got.Manifest.Expect.DecisionHash != b.DecisionHash {
		t.Fatal("manifest.expect 没写进去")
	}
	if !bytes.Equal(got.Result, b.Result) {
		t.Fatalf("result.json 往返不一致: %s", got.Result)
	}
	// zip 里必须有那 5 个文件（result 有内容时）。
	zr, err := zip.NewReader(bytes.NewReader(buf1.Bytes()), int64(buf1.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, n := range []string{FileManifest, FileOps, FileAcks, FileResult, FileTarget} {
		if !names[n] {
			t.Fatalf("缺文件 %s（有 %v）", n, names)
		}
	}
}

// TestBundleMissingOptionalFiles 钉住"缺 acks/result 也读得回来"（缺收据 → unverifiable）。
func TestBundleMissingOptionalFiles(t *testing.T) {
	b := testBundle(t)
	b.Acks = nil
	b.Result = nil
	var buf bytes.Buffer
	if err := WriteBundle(&buf, b); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	got, err := ReadBundle(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}
	if len(got.Acks) != 0 || len(got.Result) != 0 {
		t.Fatalf("缺省文件不该被凭空造出来: acks=%d result=%q", len(got.Acks), got.Result)
	}
}

// TestBundleRejects 覆盖证据包的 fail closed。
func TestBundleRejects(t *testing.T) {
	// ① 不是 zip。
	if _, err := ReadBundle(strings.NewReader("not a zip")); !errors.Is(err, ErrBadBundle) {
		t.Fatalf("非 zip 期望 ErrBadBundle，实际 %v", err)
	}
	// ② 缺必需条目。
	zipMissing, err := zipOf(t, map[string][]byte{FileOps: []byte("{}\n")})
	if err != nil {
		t.Fatalf("zipOf: %v", err)
	}
	if _, err := ReadBundle(bytes.NewReader(zipMissing)); !errors.Is(err, ErrBadBundle) {
		t.Fatalf("缺 manifest 期望 ErrBadBundle，实际 %v", err)
	}
	// ③ manifest.expect 与 verify-target.json 不一致（包被改过）。
	manRaw, _ := CanonicalJSON(Manifest{
		Ref: "ref-x", ResultSchema: "1", VoteRule: "1", Seats: 1, InviteIDs: []string{"iv_1"},
		Expect: Expect{DecisionHash: "sha256:aaa", TranscriptHash: "sha256:bbb"},
	})
	targetRaw, _ := CanonicalJSON(VerifyTarget{Ref: "ref-x", DecisionHash: "sha256:aaa", TranscriptHash: "sha256:ccc"})
	zipBad, err := zipOf(t, map[string][]byte{
		FileManifest: manRaw,
		FileOps:      []byte(""),
		FileTarget:   targetRaw,
	})
	if err != nil {
		t.Fatalf("zipOf: %v", err)
	}
	if _, err := ReadBundle(bytes.NewReader(zipBad)); !errors.Is(err, ErrBadBundle) {
		t.Fatalf("两份声明值不一致期望 ErrBadBundle，实际 %v", err)
	}
	// ④ bundleVersion 太新 → fail closed（未来格式不猜）。
	manFuture, _ := CanonicalJSON(Manifest{
		Ref: "ref-x", ResultSchema: "1", VoteRule: "1", Seats: 1, InviteIDs: []string{"iv_1"},
		BundleVersion: BundleVersion + 1,
	})
	target2, _ := CanonicalJSON(VerifyTarget{Ref: "ref-x"})
	zipFuture, err := zipOf(t, map[string][]byte{
		FileManifest: manFuture, FileOps: []byte(""), FileTarget: target2,
	})
	if err != nil {
		t.Fatalf("zipOf: %v", err)
	}
	if _, err := ReadBundle(bytes.NewReader(zipFuture)); !errors.Is(err, ErrBadBundle) {
		t.Fatalf("未来 bundleVersion 期望 ErrBadBundle，实际 %v", err)
	}
	// ⑤ manifest 缺规则版本 → 写包时就要拒（C-3）。
	bad := testBundle(t)
	bad.Manifest.ResultSchema = ""
	if err := WriteBundle(&bytes.Buffer{}, bad); !errors.Is(err, ErrBadBundle) {
		t.Fatalf("缺 resultSchema 期望 ErrBadBundle，实际 %v", err)
	}
	bad = testBundle(t)
	bad.Manifest.InviteIDs = nil
	if err := WriteBundle(&bytes.Buffer{}, bad); !errors.Is(err, ErrBadBundle) {
		t.Fatalf("缺 inviteIds 期望 ErrBadBundle，实际 %v", err)
	}
	// ⑥ 载荷里有浮点的 op 不许进包（规范编码会拒）。
	bad = testBundle(t)
	bad.Ops[1].P = json.RawMessage(`{"score":1.5}`)
	if err := WriteBundle(&bytes.Buffer{}, bad); !errors.Is(err, ErrBadBundle) {
		t.Fatalf("浮点载荷期望 ErrBadBundle，实际 %v", err)
	}
}

// TestSaveLoadBundle 覆盖路径版便利函数（原子写）。
func TestSaveLoadBundle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bundle.zip")
	b := testBundle(t)
	if err := SaveBundle(path, b); err != nil {
		t.Fatalf("SaveBundle: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("包没落盘: %v", err)
	}
	got, err := LoadBundle(path)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if got.Manifest.Ref != b.Manifest.Ref || len(got.Ops) != len(b.Ops) {
		t.Fatalf("往返不一致: %+v", got.Manifest)
	}
	// 目录里不该留下临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("临时文件没清干净: %d 个 entry", len(entries))
	}
}

// TestSealAndHeadOf 覆盖 Append 语义的便利函数。
func TestSealAndHeadOf(t *testing.T) {
	priv := testPriv("seal")
	op1, err := Seal(priv, "", 1, 1765000000000, OpCreate, "host", map[string]any{"ref": "r"})
	if err != nil {
		t.Fatalf("Seal(1): %v", err)
	}
	if op1.PH != ZeroHash {
		t.Fatalf("首条 ph 应为 ZeroHash，实际 %s", op1.PH)
	}
	if HeadOf([]Op{op1}) != op1.H {
		t.Fatal("HeadOf 不是末条 h")
	}
	op2, err := Seal(priv, op1.H, 2, 1765000000001, OpPost, "m_1", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("Seal(2): %v", err)
	}
	if op2.PH != op1.H {
		t.Fatalf("第二条 ph 应等于首条 h: %s != %s", op2.PH, op1.H)
	}
	if HeadOf([]Op{op1, op2}) != op2.H {
		t.Fatal("HeadOf 不是末条 h")
	}
	// SealOp 等价于用 op 自己的字段重新 Seal。
	pre := Op{I: 2, T: 1765000000001, O: OpPost, A: "m_1", P: op2.P, PH: op1.H}
	sealed, err := SealOp(pre, priv)
	if err != nil {
		t.Fatalf("SealOp: %v", err)
	}
	if sealed.H != op2.H || sealed.S != op2.S {
		t.Fatalf("SealOp 与 Seal 不一致: %+v vs %+v", sealed, op2)
	}
	// 坏私钥必须报错，不许 panic。
	if _, err := Seal(ed25519.PrivateKey{1}, "", 1, 1, OpPost, "a", map[string]any{}); !errors.Is(err, ErrInvalidSig) {
		t.Fatalf("坏私钥期望 ErrInvalidSig，实际 %v", err)
	}
}

// ───────────────────────────── 测试小工具 ─────────────────────────────

func mustHash(t *testing.T, prev string, i int, ts int64, o, a string, payload []byte) string {
	t.Helper()
	h, err := Hash(prev, i, ts, o, a, payload)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	return h
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return len(s) > 0
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

// flipB64 改掉 base64url 串里的一个字符（模拟"改一个字"）。
func flipB64(s string) string {
	if s == "" {
		return "A"
	}
	b := []byte(s)
	if b[5] == 'A' {
		b[5] = 'B'
	} else {
		b[5] = 'A'
	}
	return string(b)
}

// zipOf 手工造一个 zip（用来测"读包时的 fail closed"）。
func zipOf(t *testing.T, files map[string][]byte) ([]byte, error) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
