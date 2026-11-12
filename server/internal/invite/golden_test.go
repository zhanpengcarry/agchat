package invite

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"agchat/internal/oplog"
)

// goldenCase 是一条黄金向量（testdata/*.json）。
//
// stage 决定这条向量在哪一步应当失败（判定顺序：结构 → 验签 → 身份）：
//
//	parse  ：Parse/ParseAt 就该失败（或成功）
//	verify ：Parse 成功，Verify(pub) 失败
//	room   ：Parse + Verify 成功，VerifyRoom(verifyRef) 失败
type goldenCase struct {
	Case      string `json:"case"`
	Note      string `json:"note,omitempty"`
	Expect    string `json:"expect"` // "pass" | "fail"
	Stage     string `json:"stage"`  // parse | verify | room
	Text      string `json:"text"`
	Pub       string `json:"pub,omitempty"`
	VerifyRef string `json:"verifyRef,omitempty"`
	Now       int64  `json:"now,omitempty"` // >0 时走 ParseAt
	ExpectErr string `json:"expectErr,omitempty"`
}

// goldenSentinel 把黄金向量里的名字映射到具体哨兵错误。
var goldenSentinel = map[string]error{
	"truncated":      ErrTruncated,
	"bad_signature":  ErrBadSignature,
	"wrong_room":     ErrWrongRoom,
	"rules_mismatch": ErrRulesMismatch,
	"expired":        ErrExpired,
}

// TestGolden 遍历 testdata/*.json 里的黄金向量。
//
// 负向断言必须命中**具体哨兵错误**（"不能只 err != nil"）：把"截断"误报成"被改过"
// 会造成最坏的用户体验（B-19），所以判定点必须被逐条钉死。
func TestGolden(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	if len(paths) < 8 {
		t.Fatalf("黄金向量只有 %d 个，规格要求 >= 8", len(paths))
	}
	sort.Strings(paths)
	for _, path := range paths {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读 %s: %v", path, err)
			}
			var gc goldenCase
			if err := json.Unmarshal(raw, &gc); err != nil {
				t.Fatalf("解析 %s: %v", path, err)
			}
			if gc.Case == "" {
				t.Fatalf("%s 缺 case 字段", path)
			}
			if gc.Expect != "pass" && gc.Expect != "fail" {
				t.Fatalf("%s: expect=%q，只能是 pass/fail", path, gc.Expect)
			}
			if gc.Expect == "fail" {
				if _, ok := goldenSentinel[gc.ExpectErr]; !ok {
					t.Fatalf("%s: expectErr=%q 不是已知哨兵", path, gc.ExpectErr)
				}
			}
			runGolden(t, gc)
		})
	}
}

// runGolden 按 stage 执行一条向量。
func runGolden(t *testing.T, gc goldenCase) {
	t.Helper()
	want := goldenSentinel[gc.ExpectErr]
	fail := func(step string, err error) {
		t.Helper()
		if gc.Expect == "pass" {
			t.Fatalf("%s 期望通过，实际 %s 失败: %v", gc.Case, step, err)
		}
		if !errors.Is(err, want) {
			t.Fatalf("%s 期望 %s=%v，实际 %v（哨兵未命中，错误分类不清）", gc.Case, gc.ExpectErr, want, err)
		}
	}

	stage := gc.Stage
	if stage == "" {
		stage = "parse"
	}
	var (
		card *Card
		err  error
	)
	if gc.Now > 0 {
		card, err = ParseAt(gc.Text, gc.Now)
	} else {
		card, err = Parse(gc.Text)
	}
	if err != nil {
		if stage != "parse" {
			t.Fatalf("%s 在 parse 阶段就失败了（stage=%s）: %v", gc.Case, stage, err)
		}
		fail("parse", err)
		return
	}
	if gc.Expect == "fail" && stage == "parse" {
		t.Fatalf("%s 期望 parse 失败，实际通过", gc.Case)
	}

	if stage == "verify" || gc.Pub != "" {
		pub, perr := PubFromKey(gc.Pub)
		if perr != nil {
			t.Fatalf("%s: 向量里的 pub 不合法: %v", gc.Case, perr)
		}
		if verr := card.Verify(pub); verr != nil {
			if stage != "verify" {
				t.Fatalf("%s 在 verify 阶段失败（stage=%s）: %v", gc.Case, stage, verr)
			}
			fail("verify", verr)
			return
		}
		if stage == "verify" {
			t.Fatalf("%s 期望 verify 失败，实际通过", gc.Case)
		}
	}

	if stage == "room" {
		if rerr := VerifyRoom(card, gc.VerifyRef); rerr != nil {
			fail("room", rerr)
			return
		}
		t.Fatalf("%s 期望 VerifyRoom 失败，实际通过", gc.Case)
	}

	// joinby 是验签之后的第四步：exp 没签名，只有签验过了它才可信（A3）。
	if stage == "joinby" {
		if jerr := CheckJoinBy(card); jerr != nil {
			fail("joinby", jerr)
			return
		}
		if gc.Expect == "fail" {
			t.Fatalf("%s 期望 CheckJoinBy 失败，实际通过", gc.Case)
		}
	}
}

// ───────────────────────── 黄金向量生成器 ─────────────────────────
//
// 只在 AGCHAT_WRITE_GOLDEN=1 时运行，用来**可复现地**重写 testdata/*.json：
//
//	cd server && $env:AGCHAT_WRITE_GOLDEN="1"; go test ./internal/invite -run TestWriteGoldens -count=1
//
// 密钥是种子确定的（不含任何真实秘密），所以同样的代码重跑得到同样的字节。

func goldenKey(seed byte) ed25519.PrivateKey {
	sum := sha256.Sum256([]byte{seed, 'a', 'g', 'c', 'h', 'a', 't', '-', 'g', 'o', 'l', 'd', 'e', 'n'})
	return ed25519.NewKeyFromSeed(sum[:])
}

func goldenPayload(priv ed25519.PrivateKey) Payload {
	pub := priv.Public().(ed25519.PublicKey)
	return Payload{
		V:     CardVersion,
		API:   APIVersion,
		Ref:   RefOf(pub),
		Key:   KeyOf(pub),
		IDs:   []string{"iv_7f3a"},
		Uses:  1,
		Seats: 3,
		Exp:   1765000000000,
		IAT:   1764900000000,
		TH:    TopicHash("月付定价"),
		HN:    "周岭的MacBook",
		ON:    "周岭",
		Caps:  []string{CapJoin, CapRead, CapPost, CapPropose, CapVote},
		RV:    Rules{Res: RulesVersion, Vote: RulesVersion, Hash: RulesVersion},
		EP:    []Endpoint{{K: EndpointLoopback, U: "http://127.0.0.1:54321"}},
		N:     "0123456789abcdef",
	}
}

// signRaw 绕过 Issue 的校验（用于造"未知 v / 未知 cap / rv 不符"这些
// 合法签发者不该产生、但接收方必须 fail closed 的卡）。
func signRaw(p Payload, priv ed25519.PrivateKey) (string, error) {
	raw, err := oplog.CanonicalJSON(p)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(priv, append([]byte(Domain), raw...))
	return BeginMarker + "\n" + MachinePrefix + b64.EncodeToString(raw) + "." + b64.EncodeToString(sig) + "\n" + EndMarker, nil
}

// machineLine 取出机读块那一行。
func machineLine(t *testing.T, block string) string {
	t.Helper()
	body, err := extractMachine(block)
	if err != nil {
		t.Fatalf("extractMachine: %v", err)
	}
	seg, err := machineSegments(body)
	if err != nil {
		t.Fatalf("machineSegments: %v", err)
	}
	return seg
}

// machineSegments 返回剥字符后的整行（前缀 + payload.sig）。
func machineSegments(body string) (string, error) {
	var b strings.Builder
	for _, r := range body {
		if keepMachineRune(r) {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if !strings.HasPrefix(s, MachinePrefix) {
		return "", fmt.Errorf("缺前缀")
	}
	return s, nil
}

// splitSegments 把整行拆成 payload / sig 两段。
func splitSegments(t *testing.T, line string) (string, string) {
	t.Helper()
	body := strings.TrimPrefix(line, MachinePrefix)
	i := strings.Index(body, ".")
	if i < 0 {
		t.Fatalf("机读行里没有 '.' 分隔符: %q", line)
	}
	return body[:i], body[i+1:]
}

// wrap 把一行机读行包回 begin/end。
func wrap(line string) string { return BeginMarker + "\n" + line + "\n" + EndMarker }

// TestWriteGoldens 生成 testdata/*.json（默认跳过）。
func TestWriteGoldens(t *testing.T) {
	if os.Getenv("AGCHAT_WRITE_GOLDEN") != "1" {
		t.Skip("设 AGCHAT_WRITE_GOLDEN=1 才重写黄金向量")
	}
	privA := goldenKey(1)
	pubA := privA.Public().(ed25519.PublicKey)
	privB := goldenKey(2)
	pubB := privB.Public().(ed25519.PublicKey)

	p := goldenPayload(privA)
	valid, err := Issue(p, privA)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	validLine := machineLine(t, valid)
	payloadSeg, sigSeg := splitSegments(t, validLine)

	// 第二张卡：同 key、不同 nonce（用来做"内容被换、签名不配套"）。
	p2 := p
	p2.N = "fedcba9876543210"
	other, err := Issue(p2, privA)
	if err != nil {
		t.Fatalf("Issue(other): %v", err)
	}
	otherPayload, _ := splitSegments(t, machineLine(t, other))

	// 未知 v=2：签名是真的，但版本 fail closed。
	pV2 := p
	pV2.V = 2
	v2, err := signRaw(pV2, privA)
	if err != nil {
		t.Fatalf("signRaw(v2): %v", err)
	}

	// 未知 cap。
	pCap := p
	pCap.Caps = append(append([]string{}, p.Caps...), "admin")
	badCap, err := signRaw(pCap, privA)
	if err != nil {
		t.Fatalf("signRaw(cap): %v", err)
	}

	// rv 不匹配（第三类：这是另一版规则办的会）。
	pRV := p
	pRV.RV = Rules{Res: RulesVersion, Vote: RulesVersion, Hash: 2}
	badRV, err := signRaw(pRV, privA)
	if err != nil {
		t.Fatalf("signRaw(rv): %v", err)
	}

	// ref 与 key 不自洽（把 ref 换成另一个房间的）。
	pRef := p
	pRef.Ref = RefOf(pubB)
	badRef, err := signRaw(pRef, privA)
	if err != nil {
		t.Fatalf("signRaw(ref): %v", err)
	}

	// 缺 iat。
	pNoIAT := p
	pNoIAT.IAT = 0
	noIAT, err := signRaw(pNoIAT, privA)
	if err != nil {
		t.Fatalf("signRaw(no iat): %v", err)
	}

	// lan 端点缺 ins:1。
	pLAN := p
	pLAN.EP = []Endpoint{{K: EndpointLAN, U: "http://192.168.1.20:54321"}}
	badLAN, err := signRaw(pLAN, privA)
	if err != nil {
		t.Fatalf("signRaw(lan): %v", err)
	}

	// 改一个字符（签名段第 12 个字符）。
	tamperedSig := []byte(sigSeg)
	if tamperedSig[12] == 'A' {
		tamperedSig[12] = 'B'
	} else {
		tamperedSig[12] = 'A'
	}
	tamperedLine := MachinePrefix + payloadSeg + "." + string(tamperedSig)

	// 截断：去掉 end marker 与后半段。
	truncatedNoEnd := BeginMarker + "\n" + validLine[:len(validLine)-40]
	// 截断：payload 段被砍掉 7 个字符（base64 解出来是半截 JSON）。
	truncatedPayload := wrap(MachinePrefix + payloadSeg[:len(payloadSeg)-7] + "." + sigSeg)
	// 截断：连 begin 都没有，只剩前半段。
	truncatedTail := validLine[:len(validLine)/2]

	// IM 加工：每行加 "> "，每 40 字符折行，结尾加零宽与 &nbsp;。
	var mangled strings.Builder
	mangled.WriteString("[人读块 · 不是凭证，可改]\n")
	mangled.WriteString("> 周岭 的 agent 邀你参加「月付定价」· 3 人 · 今天 18:00 前可进 · 本机/内网\n")
	mangled.WriteString("> " + ShortRef(p.Ref) + "（短指纹，可让对方念）\n")
	for _, ln := range []string{BeginMarker, validLine, EndMarker} {
		mangled.WriteString("> ")
		for i := 0; i < len(ln); i += 40 {
			end := i + 40
			if end > len(ln) {
				end = len(ln)
			}
			if i > 0 {
				mangled.WriteString("\n> ")
			}
			mangled.WriteString(ln[i:end])
		}
		mangled.WriteString("\u200b\u3000&nbsp;\n")
	}

	refB := RefOf(pubB)

	cases := []goldenCase{
		{
			Case: "valid_min", Note: "合法：字段齐全、ref/key 自洽、签名有效",
			Expect: "pass", Stage: "parse", Text: valid, Pub: KeyOf(pubA),
		},
		{
			Case: "valid_im_mangled", Note: "IM 加工：加 '> '、折行、零宽字符、&nbsp; 后仍可解（§2.2 剥字符规则）",
			Expect: "pass", Stage: "parse", Text: mangled.String(), Pub: KeyOf(pubA),
		},
		{
			Case: "truncated_no_end", Note: "截断：只有 begin 没有 end",
			Expect: "fail", Stage: "parse", Text: truncatedNoEnd, ExpectErr: "truncated",
		},
		{
			Case: "truncated_payload_segment", Note: "截断：payload 段少一截（base64/JSON 不完整）",
			Expect: "fail", Stage: "parse", Text: truncatedPayload, ExpectErr: "truncated",
		},
		{
			Case: "truncated_no_markers", Note: "截断：连前缀都只剩一半",
			Expect: "fail", Stage: "parse", Text: truncatedTail, ExpectErr: "truncated",
		},
		{
			Case: "tampered_one_char", Note: "篡改：签名段改一个字符",
			Expect: "fail", Stage: "verify", Text: wrap(tamperedLine), Pub: KeyOf(pubA), ExpectErr: "bad_signature",
		},
		{
			Case: "tampered_payload_from_other_card", Note: "篡改：payload 换成另一张卡（签名不配套）",
			Expect: "fail", Stage: "verify", Text: wrap(MachinePrefix + otherPayload + "." + sigSeg), Pub: KeyOf(pubA), ExpectErr: "bad_signature",
		},
		{
			Case: "unknown_v", Note: "未知卡格式版本 v=2（签名是真的，仍 fail closed）",
			Expect: "fail", Stage: "parse", Text: v2, ExpectErr: "bad_signature",
		},
		{
			Case: "unknown_cap", Note: "未知能力位 caps+[admin]（闭枚枚举，fail closed）",
			Expect: "fail", Stage: "parse", Text: badCap, ExpectErr: "bad_signature",
		},
		{
			Case: "rv_mismatch", Note: "规则版本不符 → 第三类（另一版规则办的会），不是'卡对不上'",
			Expect: "fail", Stage: "parse", Text: badRV, ExpectErr: "rules_mismatch",
		},
		{
			Case: "ref_not_sha256_key", Note: "ref 与 key 不自洽（换成另一个房间的 ref）",
			Expect: "fail", Stage: "parse", Text: badRef, ExpectErr: "bad_signature",
		},
		{
			Case: "missing_iat", Note: "缺签发时刻 iat（§10.1⑤）",
			Expect: "fail", Stage: "parse", Text: noIAT, ExpectErr: "bad_signature",
		},
		{
			Case: "lan_endpoint_without_ins", Note: "ep.k=lan 但没有 ins:1（G11 明文入站必须显式标记）",
			Expect: "fail", Stage: "parse", Text: badLAN, ExpectErr: "bad_signature",
		},
		{
			Case: "expired_join_by", Note: "过期：joinBy 已过（宿主权威判定，ParseAt 显式给 now）",
			Expect: "fail", Stage: "parse", Text: valid, Pub: KeyOf(pubA), Now: p.Exp + 1, ExpectErr: "expired",
		},
		{
			Case: "fresh_join_by", Note: "未过期：now 早于 joinBy，同一张卡应当通过",
			Expect: "pass", Stage: "parse", Text: valid, Pub: KeyOf(pubA), Now: p.Exp - 1,
		},
		{
			Case: "wrong_room", Note: "换 ref：签名有效但是另一场会的卡（§2.4 第三句）",
			Expect: "fail", Stage: "room", Text: valid, Pub: KeyOf(pubA), VerifyRef: refB, ExpectErr: "wrong_room",
		},
		{
			Case: "foreign_host", Note: "换 ref：用另一台主机的公钥验签 → 认证失败（不是身份失败）",
			Expect: "fail", Stage: "verify", Text: valid, Pub: KeyOf(pubB), ExpectErr: "bad_signature",
		},
	}

	dir := "testdata"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	for i, gc := range cases {
		raw, err := json.MarshalIndent(gc, "", "  ")
		if err != nil {
			t.Fatalf("marshal %s: %v", gc.Case, err)
		}
		raw = append(raw, '\n')
		name := fmt.Sprintf("%02d-%s.json", i+1, gc.Case)
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	t.Logf("写出 %d 个黄金向量到 %s/", len(cases), dir)
}
