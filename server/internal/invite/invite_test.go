package invite

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agchat/internal/oplog"
)

// testKey 是种子确定的测试密钥（不是秘密，只用于向量与单测）。
func testKey(seed byte) ed25519.PrivateKey { return goldenKey(seed) }

func testPayload(priv ed25519.PrivateKey) Payload {
	p := goldenPayload(priv)
	p.Exp = 0
	p.EP = nil
	return p
}

// TestRefOfAndKeyEncoding 钉住 A-3 的规范身份形状：43 字符 base64url 无填充。
func TestRefOfAndKeyEncoding(t *testing.T) {
	priv := testKey(9)
	pub := priv.Public().(ed25519.PublicKey)
	ref := RefOf(pub)
	if len(ref) != 43 {
		t.Fatalf("ref 长度 %d，期望 43", len(ref))
	}
	if strings.ContainsAny(ref, "+/=") {
		t.Fatalf("ref 不是 base64url 无填充: %q", ref)
	}
	if again := RefOf(pub); again != ref {
		t.Fatalf("RefOf 不稳定: %s != %s", again, ref)
	}
	keyStr := KeyOf(pub)
	back, err := PubFromKey(keyStr)
	if err != nil {
		t.Fatalf("PubFromKey: %v", err)
	}
	if !back.Equal(pub) {
		t.Fatal("PubFromKey 往返不一致")
	}
	if _, err := PubFromKey("short"); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("坏 key 期望 ErrBadSignature，实际 %v", err)
	}
}

// TestTopicHash 钉住 th 的口径（§10.1 规则 1：唯一一份 normalize）。
func TestTopicHash(t *testing.T) {
	h := TopicHash("月付定价")
	if !isHex16(h) {
		t.Fatalf("TopicHash 不是 16 位 hex: %q", h)
	}
	// 首尾空白与零宽字符不影响 th。
	for _, variant := range []string{
		"  月付定价  ",
		"月付定价\t\n",
		"月付定价\u200b",
		"月付定价\ufeff",
	} {
		if got := TopicHash(variant); got != h {
			t.Fatalf("normalize 不一致：%q → %s，基准 %s", variant, got, h)
		}
	}
	// 内部空白：连续空白折叠成**一个**空格（保守口径：不删空格，只折叠）。
	spaced := TopicHash("月付 定价")
	for _, variant := range []string{"月付  定价", "月付\t定价", "月付\u3000定价", "月付\u200b 定价"} {
		if got := TopicHash(variant); got != spaced {
			t.Fatalf("内部空白折叠不一致：%q → %s，期望 %s", variant, got, spaced)
		}
	}
	if spaced == h {
		t.Fatal("'月付 定价' 与 '月付定价' 应当是不同的 th")
	}
	// 差一个字必须变。
	if got := TopicHash("月付定價"); got == h {
		t.Fatal("改一个字 hash 没变")
	}
}

// TestHumanError 钉住 §2.4 的三句人话（原样字符串）+ 两个加法。
func TestHumanError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{ErrTruncated, "卡不完整：转发时被截断了。请让对方把 begin/end 之间的整段重发一次。"},
		{ErrBadSignature, "卡对不上：内容被改过，或不是这台主机的签发卡。不要用它。"},
		{ErrWrongRoom, "这是另一场会的卡。"},
		{ErrRulesMismatch, HumanRulesMismatch},
		{ErrExpired, HumanExpired},
		{errors.New("其它错误"), HumanUnknown},
	}
	for _, c := range cases {
		if got := HumanError(c.err); got != c.want {
			t.Fatalf("HumanError(%v) = %q，期望 %q", c.err, got, c.want)
		}
	}
	// §10.1④：rv 不符属于第三类（身份/会话），不是"卡对不上"。
	if !errors.Is(ErrRulesMismatch, ErrWrongRoom) {
		t.Fatal("ErrRulesMismatch 必须同时命中 ErrWrongRoom（第三类）")
	}
	if errors.Is(ErrRulesMismatch, ErrBadSignature) {
		t.Fatal("ErrRulesMismatch 不该被当成 ErrBadSignature")
	}
	// 包装后仍要能翻译成人话。
	wrapped := errors.Join(ErrTruncated)
	if HumanError(wrapped) != HumanTruncated {
		t.Fatal("包装后的哨兵没被翻译")
	}
}

// TestIssueRejectsBadPayload 钉住签发侧的 fail closed 校验。
func TestIssueRejectsBadPayload(t *testing.T) {
	priv := testKey(3)
	base := testPayload(priv)

	mutate := func(fn func(p *Payload)) Payload {
		p := base
		p.IDs = append([]string{}, base.IDs...)
		p.Caps = append([]string{}, base.Caps...)
		fn(&p)
		return p
	}
	cases := map[string]Payload{
		"v=2":         mutate(func(p *Payload) { p.V = 2 }),
		"api=0":       mutate(func(p *Payload) { p.API = 0 }),
		"ref 不自洽":     mutate(func(p *Payload) { p.Ref = RefOf(testKey(4).Public().(ed25519.PublicKey)) }),
		"ids 空":       mutate(func(p *Payload) { p.IDs = nil }),
		"ids 重复":      mutate(func(p *Payload) { p.IDs = []string{"a", "a"} }),
		"uses=0":      mutate(func(p *Payload) { p.Uses = 0 }),
		"uses>seats":  mutate(func(p *Payload) { p.Uses = 9 }),
		"seats=0":     mutate(func(p *Payload) { p.Seats = 0 }),
		"exp<0":       mutate(func(p *Payload) { p.Exp = -1 }),
		"iat=0":       mutate(func(p *Payload) { p.IAT = 0 }),
		"iat>exp":     mutate(func(p *Payload) { p.Exp = 1000; p.IAT = 2000 }),
		"th 不是 hex":   mutate(func(p *Payload) { p.TH = "zz" }),
		"caps 空":      mutate(func(p *Payload) { p.Caps = nil }),
		"caps 未知":     mutate(func(p *Payload) { p.Caps = append(p.Caps, "admin") }),
		"caps 重复":     mutate(func(p *Payload) { p.Caps = append(p.Caps, CapJoin) }),
		"rv 不是 1,1,1": mutate(func(p *Payload) { p.RV.Hash = 2 }),
		"n 空":         mutate(func(p *Payload) { p.N = "" }),
		"ep.k 未知":     mutate(func(p *Payload) { p.EP = []Endpoint{{K: "carrier-pigeon", U: "x"}} }),
		"lan 缺 ins":   mutate(func(p *Payload) { p.EP = []Endpoint{{K: EndpointLAN, U: "http://192.168.1.2:1"}} }),
		"ep.u 空":      mutate(func(p *Payload) { p.EP = []Endpoint{{K: EndpointLoopback, U: ""}} }),
	}
	for name, p := range cases {
		if _, err := Issue(p, priv); !errors.Is(err, ErrInvalidPayload) {
			t.Fatalf("%s：期望 ErrInvalidPayload，实际 %v", name, err)
		}
	}
	// 私钥长度不对也必须拒（不能 panic）。
	if _, err := Issue(base, ed25519.PrivateKey{1, 2, 3}); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("坏私钥期望 ErrInvalidPayload，实际 %v", err)
	}
	// 正确载荷必须能签出来。
	if _, err := Issue(base, priv); err != nil {
		t.Fatalf("合法载荷签发失败: %v", err)
	}
}

// TestIssueMachineLineShape 钉住 §2.2 的机读块形状与长度上限。
func TestIssueMachineLineShape(t *testing.T) {
	priv := testKey(5)
	p := testPayload(priv)
	block, err := Issue(p, priv)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !strings.HasPrefix(block, BeginMarker+"\n"+MachinePrefix) || !strings.HasSuffix(block, "\n"+EndMarker) {
		t.Fatalf("机读块包裹形状不对: %q", block)
	}
	line := strings.Split(strings.TrimSuffix(strings.TrimPrefix(block, BeginMarker+"\n"), "\n"+EndMarker), "\n")
	if len(line) != 1 {
		t.Fatalf("机读块必须是单行，实际 %d 行", len(line))
	}
	payloadSeg, sigSeg := splitSegments(t, line[0])
	raw, err := decodeB64(payloadSeg)
	if err != nil {
		t.Fatalf("payload 段解码: %v", err)
	}
	// 规范编码：键升序、无空格、无 null。
	if strings.ContainsAny(string(raw), " \n\t") {
		t.Fatalf("payload 不是规范编码（有多余空白）: %s", raw)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("payload 出现 null: %s", raw)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("payload 不是 JSON 对象: %v", err)
	}
	if _, ok := m["exp"]; ok {
		t.Fatalf("exp=0 应当被 omitempty 省掉: %s", raw)
	}
	if _, ok := m["ep"]; ok {
		t.Fatalf("空 ep 应当被 omitempty 省掉: %s", raw)
	}
	// 卡里不许出现长期秘密（arch 打回条件 4）：封闭载荷里没有这些键的容身之处。
	for _, k := range []string{"pass", "password", "adminKey", "priv", "seed", "token"} {
		if _, ok := m[k]; ok {
			t.Fatalf("卡里出现了 %q（打回条件 4）", k)
		}
	}
	// 签名段是 86 字符（64 字节）base64url。
	if len(sigSeg) != sigSegChars {
		t.Fatalf("签名段 %d 字符，期望 %d", len(sigSeg), sigSegChars)
	}
	// 超长（把 ids 灌爆）必须报错，不许为压长度删字段。
	long := testPayload(priv)
	for i := 0; i < 60; i++ {
		long.IDs = append(long.IDs, strings.Repeat("i", 40))
	}
	long.Uses = len(long.IDs)
	long.Seats = len(long.IDs)
	if _, err := Issue(long, priv); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("超长机读块期望 ErrInvalidPayload，实际 %v", err)
	}
}

// TestIssueLayeredHumanBlock 钉住人读块的两条硬约束：≤500 字符 + 必须含短指纹。
func TestIssueLayeredHumanBlock(t *testing.T) {
	priv := testKey(6)
	p := testPayload(priv)
	fp := ShortRef(p.Ref)
	if len(fp) != 8 {
		t.Fatalf("ShortRef 长度 %d，期望 8（§10.1⑦）", len(fp))
	}
	card, err := IssueLayered(p, priv, []string{
		"周岭 的 agent 邀你参加「月付定价」· 3 人 · 今天 18:00 前可进 · 本机/内网",
		"核对短指纹：" + fp,
	})
	if err != nil {
		t.Fatalf("IssueLayered: %v", err)
	}
	if !strings.Contains(card, "[人读块 · 不是凭证，可改]") {
		t.Fatal("缺人读块标语")
	}
	if !strings.Contains(card, BeginMarker) || !strings.Contains(card, EndMarker) {
		t.Fatal("缺机读块包裹")
	}
	// 没有短指纹必须拒签。
	if _, err := IssueLayered(p, priv, []string{"周岭 的 agent 邀你参加「月付定价」"}); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("缺短指纹期望 ErrInvalidPayload，实际 %v", err)
	}
	// 人读块超 500 字符必须拒签。
	long := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		long = append(long, strings.Repeat("表", 30))
	}
	long = append(long, fp)
	if _, err := IssueLayered(p, priv, long); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("人读块超长期望 ErrInvalidPayload，实际 %v", err)
	}
	// 分层卡必须能解回来（人读块被忽略）。
	c, err := Parse(card)
	if err != nil {
		t.Fatalf("Parse(分层卡): %v", err)
	}
	if err := c.Verify(priv.Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("Verify(分层卡): %v", err)
	}
}

// TestParseFailClosed 钉住"未知字段 / null / 闭环格式"这几条 fail closed。
func TestParseFailClosed(t *testing.T) {
	priv := testKey(7)
	pub := priv.Public().(ed25519.PublicKey)
	base := testPayload(priv)

	raw, err := oplog.CanonicalJSON(base)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	signWith := func(payloadRaw []byte) string {
		sig := ed25519.Sign(priv, append([]byte(Domain), payloadRaw...))
		return BeginMarker + "\n" + MachinePrefix + base64.RawURLEncoding.EncodeToString(payloadRaw) + "." + base64.RawURLEncoding.EncodeToString(sig) + "\n" + EndMarker
	}

	// 未知字段（例如有人往卡里塞 pass）。
	withExtra := strings.TrimSuffix(string(raw), "}") + `,"pass":"hunter2"}`
	if _, err := Parse(signWith([]byte(withExtra))); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("未知字段期望 ErrBadSignature，实际 %v", err)
	}
	// null 字段。
	withNull := strings.TrimSuffix(string(raw), "}") + `,"hn":null}`
	if _, err := Parse(signWith([]byte(withNull))); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("null 字段期望 ErrBadSignature，实际 %v", err)
	}
	// JSON 不完整（能 base64 解出来但 JSON 断了）→ 结构性失败。
	broken := string(raw)[:len(raw)/2]
	if _, err := Parse(signWith([]byte(broken))); !errors.Is(err, ErrTruncated) {
		t.Fatalf("半截 JSON 期望 ErrTruncated，实际 %v", err)
	}
	// 空文本 / 只有人读块。
	if _, err := Parse("   "); !errors.Is(err, ErrTruncated) {
		t.Fatalf("空文本期望 ErrTruncated，实际 %v", err)
	}
	if _, err := Parse("周岭 的 agent 邀你参加「月付定价」"); !errors.Is(err, ErrTruncated) {
		t.Fatalf("纯人读块期望 ErrTruncated，实际 %v", err)
	}
	// 正常路径可控。
	c, err := Parse(signWith(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := c.Verify(pub); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// 验签用的是**收到的原始字节**：往 payload 里插空格后重新签名的卡仍能验过，
	// 但用"重新序列化"的思路会算出不同的字节 → 这就是 A-1 要求不用规范化的原因。
	spaced := strings.Replace(string(raw), `"v":1`, `"v" : 1`, 1)
	c2, err := Parse(signWith([]byte(spaced)))
	if err != nil {
		t.Fatalf("Parse(带空格): %v", err)
	}
	if err := c2.Verify(pub); err != nil {
		t.Fatalf("Verify(带空格): %v", err)
	}
	if string(c2.RawPayload) != spaced {
		t.Fatal("RawPayload 必须是收到的原始字节，不能被规范化")
	}
}

// TestVerifyGuards 覆盖 Verify / VerifyRoom 的边界。
func TestVerifyGuards(t *testing.T) {
	priv := testKey(8)
	pub := priv.Public().(ed25519.PublicKey)
	p := testPayload(priv)
	block, err := Issue(p, priv)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	c, err := Parse(block)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var nilCard *Card
	if err := nilCard.Verify(pub); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("nil 卡期望 ErrBadSignature，实际 %v", err)
	}
	if err := c.Verify(ed25519.PublicKey{1, 2, 3}); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("坏公钥期望 ErrBadSignature，实际 %v", err)
	}
	if err := c.Verify(testKey(10).Public().(ed25519.PublicKey)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("别的公钥期望 ErrBadSignature，实际 %v", err)
	}
	if err := VerifyRoom(c, p.Ref); err != nil {
		t.Fatalf("同 ref 的 VerifyRoom 不应失败: %v", err)
	}
	if err := VerifyRoom(c, "another-room-ref"); !errors.Is(err, ErrWrongRoom) {
		t.Fatalf("别的 ref 期望 ErrWrongRoom，实际 %v", err)
	}
	if err := VerifyRoom(c, ""); !errors.Is(err, ErrWrongRoom) {
		t.Fatalf("空 ref 期望 ErrWrongRoom，实际 %v", err)
	}
	// 签名段被截短 → 结构性失败（长度不足）。
	short := c
	short.Sig = c.Sig[:10]
	if err := short.Verify(pub); !errors.Is(err, ErrTruncated) {
		t.Fatalf("签名长度不足期望 ErrTruncated，实际 %v", err)
	}
	// 没有原始字节 → 结构性失败（不允许"重新序列化后再验"）。
	empty := c
	empty.RawPayload = nil
	if err := empty.Verify(pub); !errors.Is(err, ErrTruncated) {
		t.Fatalf("缺原始字节期望 ErrTruncated，实际 %v", err)
	}
}

// TestParseAtJoinBy 钉住"过期只由显式 now 判定"（arch A-17）。
func TestParseAtJoinBy(t *testing.T) {
	priv := testKey(11)
	p := testPayload(priv)
	p.Exp = 1765000000000
	p.IAT = 1764900000000
	block, err := Issue(p, priv)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	// Parse 不读时钟：过期卡也能解析（权威判定在宿主）。
	if _, err := Parse(block); err != nil {
		t.Fatalf("Parse 不该判过期: %v", err)
	}
	if _, err := ParseAt(block, p.Exp-1); err != nil {
		t.Fatalf("未到 joinBy 就判过期: %v", err)
	}
	if _, err := ParseAt(block, p.Exp); err != nil {
		t.Fatalf("恰好等于 joinBy 不该判过期: %v", err)
	}
	// 过期不再由 Parse 定性（A3：exp 没签名，必须等验签），改由 CheckJoinBy 判。
	c, err := ParseAt(block, p.Exp+1)
	if err != nil {
		t.Fatalf("过期卡仍应解析成功: %v", err)
	}
	if err := CheckJoinBy(c); !errors.Is(err, ErrExpired) {
		t.Fatalf("超过 joinBy 期望 CheckJoinBy 给 ErrExpired，实际 %v", err)
	}
	// now=0 表示不判。
	c0, err := ParseAt(block, 0)
	if err != nil {
		t.Fatalf("now=0 不该判过期: %v", err)
	}
	if err := CheckJoinBy(c0); err != nil {
		t.Fatalf("now=0 时 CheckJoinBy 不该报错: %v", err)
	}
}

// TestNewNonce 钉住 nonce 的形状（12B → 16 字符 base64url）。
func TestNewNonce(t *testing.T) {
	a, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	b, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	if len(a) != 16 {
		t.Fatalf("nonce 长度 %d，期望 16", len(a))
	}
	if a == b {
		t.Fatal("两次生成的 nonce 相同")
	}
	if _, err := decodeB64(a); err != nil {
		t.Fatalf("nonce 不是 base64url: %v", err)
	}
}
