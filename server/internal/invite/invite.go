// Package invite 是邀请卡（分层卡）的编解码/签名/校验，**无状态纯格式包**。
//
// 依据 docs/09-本地宿主规格-S1冻结.md §2，以及 arch 评审 A-1/A-3/A-7/A-8/A-10/A-18：
//
//   - 验签对**收到的原始字节**做，绝不"先 parse 再重新序列化再验签"（A-1）；
//   - 未知 v / api / caps 一律 fail closed（A-8）；
//   - ref = base64url_nopad(sha256(roomKeyPub)) 是规范身份（A-3）；
//   - 卡里禁止出现密码 / adminKey / roomKey 私钥（打回条件 4）——由封闭的
//     Payload 结构 + 严格反序列化（未知字段、null 一律拒）保证。
//
// 本包**不读时钟、不读吊销表、不碰 store/net/http**（arch §6.1：invite 必须是无状态纯格式包）。
// joinBy（能否进场）的权威判定在宿主；本包只提供显式传 now 的 ParseAt 供调用方做提示。
//
// 判定顺序（docs/09 §2.4 硬规则）：先结构（ErrTruncated）→ 再验签（ErrBadSignature）
// → 最后比对身份（ErrWrongRoom）。三句人话见 HumanError。
package invite

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"agchat/internal/oplog"
)

// 机器块与签名域常量。
const (
	// MachinePrefix 是机读块的一行前缀。
	MachinePrefix = "agchat-invite:v1:"
	// BeginMarker / EndMarker 包裹机读块。
	BeginMarker = "---agchat-invite-begin---"
	EndMarker   = "---agchat-invite-end---"
	// Domain 是卡签名域前缀（含 0x00 域分隔，防跨域复用）。
	Domain = "agchat-invite:v1\x00"

	// CardVersion 是当前唯一支持的卡格式版本。
	CardVersion = 1
	// APIVersion 是当前唯一支持的宿主接口版本。
	APIVersion = 1
	// RulesVersion 是当前唯一支持的结果/计票/hash 规则版本。
	RulesVersion = 1

	// MaxHumanBlockChars 是人读块硬上限（pm R6 / arch 9.4）。
	MaxHumanBlockChars = 500
	// MaxMachineLineChars 是机读块单行硬上限（arch B-18）。
	MaxMachineLineChars = 1024
	// NonceBytes 是卡 nonce 的字节数（base64url 后 16 字符）。
	NonceBytes = 12
)

// 闭枚能力位（arch A-8）：未知值一律拒整张卡。
const (
	CapJoin    = "join"
	CapRead    = "read"
	CapPost    = "post"
	CapPropose = "propose"
	CapVote    = "vote"
)

// §2.4 的三句人话（原样字符串，禁止改写）。
const (
	HumanTruncated     = "卡不完整：转发时被截断了。请让对方把 begin/end 之间的整段重发一次。"
	HumanMultiMachine  = "卡里有不止一段机读块：请让对方只发一张卡（人读块可以留着）。"
	HumanTopicMismatch = "卡里人读块写的议题和机读块对不上：人读块被改过，把整张卡发出来让对方核对。"
	HumanBadSignature  = "卡对不上：内容被改过，或不是这台主机的签发卡。不要用它。"
	HumanWrongRoom     = "这是另一场会的卡。"
)

// HumanExpired 是规格 §2.4 三句之外唯一新增的一句（joinBy 过期）。
// 它不属于 §2.4 的三类失败：结构/认证/身份都对，只是进场时间过了。
const HumanExpired = "这张卡已过进场时间（joinBy 截止）。请让对方发一张新卡。"

// HumanRulesMismatch 是第三类（身份/会话失败）的细分：规则版本不匹配（§10.1④）。
// 它**不是**"卡对不上"——签名可能是好的，只是这张卡属于另一版规则办的会。
const HumanRulesMismatch = "这是另一版规则办的会（规则版本不一致），不能用来进场。"

// HumanUnknown 是兜底人话（未知错误）。故意不泄露技术细节。
const HumanUnknown = "卡无法使用：无法确认这张卡。请让对方重新发一张卡。"

// 三个哨兵错误，各对应 docs/09 §2.4 的一句人话。
var (
	// ErrTruncated：结构性失败（切不出两段 / base64url 解码失败 / 长度不足 / JSON 不完整）。
	ErrTruncated = errors.New("invite: 卡不完整（结构）")
	// ErrBadSignature：认证失败（能解码、字段齐全，但签名验不过，或字段值 fail closed）。
	ErrBadSignature = errors.New("invite: 卡签名/字段不匹配")
	// ErrWrongRoom：身份失败（验签通过，但 ref 与当前会话不符）。
	ErrWrongRoom = errors.New("invite: 不是本场的卡")
	// ErrRulesMismatch：第三类的细分——规则版本不一致（§10.1④）。
	// 它同时满足 errors.Is(err, ErrWrongRoom)，因为两者同属"这不是本场的会"。
	ErrRulesMismatch = fmt.Errorf("%w: 规则版本不一致", ErrWrongRoom)
	// ErrExpired：joinBy 已过（只由 ParseAt 判定；宿主权威）。
	ErrExpired = errors.New("invite: 卡已过 joinBy")
	// ErrMultiMachine：一张卡里塞了多段机读块（A1）。
	ErrMultiMachine = errors.New("invite: 卡里有多段机读块")
	// ErrHumanTopic：人读块里的议题与签进 payload 的 th 不符（A2）。
	ErrHumanTopic = errors.New("invite: 人读块的议题与机读块不符")
	// ErrInvalidPayload：签发侧入参不合法（不会出现在解析路径上）。
	ErrInvalidPayload = errors.New("invite: payload 不合法")
)

// b64 是全程唯一的 base64url 无填充编码。
var b64 = base64.RawURLEncoding

// 端点种类枚举（§10.1⑥）：k 只能是这三个之一。
const (
	EndpointLoopback = "loopback"
	EndpointLAN      = "lan"
	EndpointTunnel   = "tunnel"
)

// Endpoint 是卡里的直连端点（S1 直连档）。
//
// K 是闭合枚举 loopback|lan|tunnel；k=="lan" 必须带 INS:1（明文入站标记，§10.1⑥ / G11）。
type Endpoint struct {
	K   string `json:"k"`
	U   string `json:"u"`
	INS int    `json:"ins,omitempty"`
}

// Rules 是规则版本三元组（res=结果 schema，vote=计票，hash=hash 域）。
type Rules struct {
	Res  int `json:"res"`
	Vote int `json:"vote"`
	Hash int `json:"hash"`
}

// Payload 是卡机读块里的 payload（短键、omitempty、禁止 null）。
//
// 字段语义（docs/09 §2.3 + §10.1）：
//
//	ids   本卡覆盖的 inviteId 全集；join 时宿主校验 uses == Σ maxUses(ids)（§10.1②）
//	uses  本卡总座位数 = Σ maxUses(ids)
//	seats 全场座位数；join 时宿主校验 seats == create.seats（§10.1③）
//	exp   joinBy：毫秒；0 = 不过期（只影响能否进场，与会议时长无关）
//	th    topicHash = hex(sha256(normalize(topic)))[:16]，**只能用 TopicHash 算**
//	iat   签发时刻（毫秒，§10.1⑤）
//	rv    规则版本三元组；不匹配 → ErrRulesMismatch（第三类，不是"卡对不上"）
type Payload struct {
	V     int        `json:"v"`
	API   int        `json:"api"`
	Ref   string     `json:"ref"`
	Key   string     `json:"key"`
	IDs   []string   `json:"ids"`
	Uses  int        `json:"uses"`
	Seats int        `json:"seats"`
	Exp   int64      `json:"exp,omitempty"`
	IAT   int64      `json:"iat,omitempty"`
	TH    string     `json:"th"`
	HN    string     `json:"hn,omitempty"`
	ON    string     `json:"on,omitempty"`
	Caps  []string   `json:"caps"`
	RV    Rules      `json:"rv"`
	EP    []Endpoint `json:"ep,omitempty"`
	N     string     `json:"n"`
}

// Card 是解析结果。RawPayload 是**收到的原始字节**，验签只用它（arch A-1）。
type Card struct {
	// 下面三个字段是"命中但还没裁决"的状态：它们说的都是**没签名**的字段，
	// 必须等 Verify 验签通过再定性。否则改一个字节就会被报成"另一版规则办的会"
	// 或"卡过期了"，而不是"卡对不上，内容被改过"（docs/09 §2.4 的顺序）。
	ruleMismatch bool
	joinBy       int64
	nowMillis    int64
	P            Payload
	RawPayload   []byte
	Sig          []byte
	Text         string
}

// payloadKeys 是 payload 的闭枚键集：出现任何别的键 → fail closed。
var payloadKeys = map[string]struct{}{
	"v": {}, "api": {}, "ref": {}, "key": {}, "ids": {}, "uses": {}, "seats": {},
	"exp": {}, "iat": {}, "th": {}, "hn": {}, "on": {}, "caps": {}, "rv": {}, "ep": {}, "n": {},
}

// caps 是闭枚能力位集合（arch A-8）。
var caps = map[string]struct{}{
	CapJoin: {}, CapRead: {}, CapPost: {}, CapPropose: {}, CapVote: {},
}

// KnownCap 报告 cap 是否在闭枚集合内。
func KnownCap(c string) bool { _, ok := caps[c]; return ok }

// Issue 签发机读块（含 begin/end 包裹）。返回值即 docs/09 §2.2 的包裹块。
//
// 签名域 = Domain || payloadRaw，其中 payloadRaw 是 CanonicalJSON 的输出；
// 验签方对收到的原始字节验证，不需要任何重新序列化。
func Issue(p Payload, priv ed25519.PrivateKey) (machineBlock string, err error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("%w: 私钥长度 %d", ErrInvalidPayload, len(priv))
	}
	// 签发侧依旧是权威校验：签出去的卡不许自带错的 rv。
	if err := checkPayload(p); err != nil {
		return "", fmt.Errorf("%w: %s", ErrInvalidPayload, err.Error())
	}
	if p.RV.Res != RulesVersion || p.RV.Vote != RulesVersion || p.RV.Hash != RulesVersion {
		return "", fmt.Errorf("%w: rv={res:%d,vote:%d,hash:%d}，本机支持 {res:%d,vote:%d,hash:%d}",
			ErrInvalidPayload, p.RV.Res, p.RV.Vote, p.RV.Hash, RulesVersion, RulesVersion, RulesVersion)
	}
	raw, err := oplog.CanonicalJSON(p)
	if err != nil {
		return "", fmt.Errorf("%w: 规范编码失败: %s", ErrInvalidPayload, err.Error())
	}
	sig := ed25519.Sign(priv, append([]byte(Domain), raw...))
	line := MachinePrefix + b64.EncodeToString(raw) + "." + b64.EncodeToString(sig)
	if len(line) > MaxMachineLineChars {
		return "", fmt.Errorf("%w: 机读块 %d 字符 > 上限 %d（不许为压长度删 ids/seats/rv，请减少端点或少发座位）",
			ErrInvalidPayload, len(line), MaxMachineLineChars)
	}
	return BeginMarker + "\n" + line + "\n" + EndMarker, nil
}

// IssueLayered 签发分层卡：人读块（≤500 字符）+ 机读块。
//
// humanLines 由宿主组织（谁开的 / 议什么 / 什么时候前可进 / 怎么连），
// 本函数只做拼接与长度约束，不改写文字；空行会被跳过。
func IssueLayered(p Payload, priv ed25519.PrivateKey, humanLines []string) (cardText string, err error) {
	machine, err := Issue(p, priv)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("[人读块 · 不是凭证，可改]\n")
	for _, ln := range humanLines {
		ln = strings.TrimRight(ln, " \t\r\n")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		b.WriteString(ln)
		b.WriteString("\n")
	}
	human := b.String()
	if n := utf8.RuneCountInString(human); n > MaxHumanBlockChars {
		return "", fmt.Errorf("%w: 人读块 %d 字符 > 上限 %d", ErrInvalidPayload, n, MaxHumanBlockChars)
	}
	// §10.1⑦ / arch A-7：人读块必须含 ref 的前 8 字符短指纹，否则 B 没有任何
	// 第二个通道可复核（"你念一下卡上那 8 个字符"）。缺了就不许签发。
	if err := CheckHumanBlock(p, human); err != nil {
		return "", err
	}
	return human + machine, nil
}

// CheckHumanBlock 校验人读块的两条硬约束（≤500 字符 + 必须含 ref 前 8 字符短指纹）。
//
// 宿主要自己拼人读块时可以单独调它；用 IssueLayered 的话它已经被调用过了。
// 注意：**不一样**人读块里的议题与 payload 的 th 一致——那需要从自由文本里
// 抽出议题，属宿主的组织责任（docs/09 §2.1 的交叉校验）。
func CheckHumanBlock(p Payload, humanBlock string) error {
	if n := utf8.RuneCountInString(humanBlock); n > MaxHumanBlockChars {
		return fmt.Errorf("%w: 人读块 %d 字符 > 上限 %d", ErrInvalidPayload, n, MaxHumanBlockChars)
	}
	fp := ShortRef(p.Ref)
	if fp == "" {
		return fmt.Errorf("%w: payload.ref 为空，算不出短指纹", ErrInvalidPayload)
	}
	if !strings.Contains(humanBlock, fp) {
		return fmt.Errorf("%w: 人读块必须含 ref 短指纹 %q（§10.1⑦ / A-7）", ErrInvalidPayload, fp)
	}
	return nil
}

// Parse 解析卡文本：剥非字母表字符 → 结构性检查 → 保留验签所需原始字节 → json.Unmarshal。
// 它**不验签**（没有公钥）、**不判过期**（不读时钟，arch A-17）。
func Parse(text string) (*Card, error) { return parseWith(text, 0) }

// ParseAt 在 Parse 之上追加 joinBy 判定：nowMillis > 0 且 payload.Exp > 0 且
// nowMillis > Exp → ErrExpired。权威判定仍在宿主（arch A-17），这里只给调用方一个
// 显式、可测、不读全局时钟的入口。
func ParseAt(text string, nowMillis int64) (*Card, error) { return parseWith(text, nowMillis) }

func parseWith(text string, nowMillis int64) (*Card, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: 空文本", ErrTruncated)
	}
	machine, err := extractMachine(text)
	if err != nil {
		return nil, err
	}
	payloadSeg, sigSeg, err := splitMachine(machine)
	if err != nil {
		return nil, err
	}
	if len(payloadSeg) > 4096 {
		return nil, fmt.Errorf("%w: payload 段 %d 字符（上限 4096）", ErrTruncated, len(payloadSeg))
	}
	raw, err := decodeB64(payloadSeg)
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("%w: payload 段解不出来（%d 字符）", ErrTruncated, len(payloadSeg))
	}
	sig, err := decodeB64(trimSigJunk(sigSeg))
	if err != nil {
		return nil, fmt.Errorf("%w: 签名段 base64url 解不出来", ErrTruncated)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: 签名 %d 字节（需要 %d）", ErrTruncated, len(sig), ed25519.SignatureSize)
	}
	p, err := decodePayload(raw)
	if err != nil {
		return nil, err
	}
	// 结构/字段自洽（rv 与 joinBy 除外：它们没有签名，留给验签之后再定性，A3）。
	if err := checkPayload(p); err != nil {
		return nil, err
	}
	// 人读块里的议题必须与 payload 的 th 对得上（§2.1 / A2）：机读块可以原样转发，
	// 只改人读块里的议题，B 看到的就是另一件事。注：人读块没签名，所以这条只是
	// "一眼能看出来的不一致"，真正的结论仍以验签为准。
	if topic, ok := humanTopic(text); ok && TopicHash(topic) != p.TH {
		return nil, fmt.Errorf("%w: 人读块说「%s」，与机读块的 th 对不上", ErrHumanTopic, topic)
	}
	c := &Card{P: p, RawPayload: raw, Sig: sig, Text: text, joinBy: p.Exp, nowMillis: nowMillis}
	c.ruleMismatch = p.RV.Res != RulesVersion || p.RV.Vote != RulesVersion || p.RV.Hash != RulesVersion
	return c, nil
}

// Verify 用**收到的原始字节**验签，并检查卡自洽（ref == RefOf(pub)、key == pub）。
//
// 判定顺序：长度（ErrTruncated）→ 签名（ErrBadSignature）→ 身份（ErrWrongRoom）。
func (c *Card) Verify(pub ed25519.PublicKey) error {
	if c == nil {
		return fmt.Errorf("%w: 空卡", ErrBadSignature)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: 公钥长度 %d", ErrBadSignature, len(pub))
	}
	if len(c.RawPayload) == 0 {
		return fmt.Errorf("%w: 没有原始 payload 字节", ErrTruncated)
	}
	if len(c.Sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: 签名 %d 字节（需要 %d）", ErrTruncated, len(c.Sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, append([]byte(Domain), c.RawPayload...), c.Sig) {
		return fmt.Errorf("%w: 签名验不过", ErrBadSignature)
	}
	if c.P.Key != KeyOf(pub) {
		return fmt.Errorf("%w: 卡的 key 不是这把公钥", ErrBadSignature)
	}
	if want := RefOf(pub); c.P.Ref != want {
		return fmt.Errorf("%w: 卡的 ref=%s，本主机 ref=%s", ErrWrongRoom, c.P.Ref, want)
	}
	// 规则版本与 joinBy 都写在**没签名**的字段里，所以只能等验签过了再定性（A3）。
	// 顺序仍然是：先"卡对不上"，再"另一版规则"，最后"过期"。
	if c.ruleMismatch {
		return fmt.Errorf("%w: 卡 rv=%d/%d/%d 本实现=%d", ErrRulesMismatch,
			c.P.RV.Res, c.P.RV.Vote, c.P.RV.Hash, RulesVersion)
	}
	return nil
}

// CheckJoinBy 判 joinBy 是否已过（调用方须先 Verify 过：过了验签，时间才可信）。
//
// 只在 ParseAt 给过 now 时有意义；Parse 出来的卡（now=0）这里一律放行。
func CheckJoinBy(c *Card) error {
	if c == nil || c.nowMillis == 0 || c.joinBy == 0 {
		return nil
	}
	if c.nowMillis > c.joinBy {
		return fmt.Errorf("%w: exp=%d now=%d", ErrExpired, c.joinBy, c.nowMillis)
	}
	return nil
}

// VerifyRoom 在验签通过后比对"当前会话"的 ref（docs/09 §2.4 第三句）。
func VerifyRoom(c *Card, wantRef string) error {
	if c == nil {
		return fmt.Errorf("%w: 空卡", ErrWrongRoom)
	}
	if strings.TrimSpace(wantRef) == "" {
		return fmt.Errorf("%w: 缺当前会话 ref", ErrWrongRoom)
	}
	if c.P.Ref != wantRef {
		return fmt.Errorf("%w: 卡的 ref=%s，当前会=%s", ErrWrongRoom, c.P.Ref, wantRef)
	}
	return nil
}

// extractMachine 取出机读块。两个 marker 任一缺失都算结构性失败（被截断）。
func extractMachine(text string) (string, error) {
	// marker 必须**恰好各一次**：否则"伪卡拼在真卡前面"会让整段真卡被静默丢掉，
	// 而验签、ref 比对全程都能过（A1）。
	nb := strings.Count(text, BeginMarker)
	ne := strings.Count(text, EndMarker)
	switch {
	case nb == 0 && ne == 0:
		// 没有 marker：整段当机读块（用户只粘了一行 v1:... 的情况）。
		if n := strings.Count(text, MachinePrefix); n > 1 {
			return "", fmt.Errorf("%w: 里面有多段机读块（%d 段），不知道用哪段", ErrMultiMachine, n)
		}
		return text, nil
	case nb != ne:
		return "", fmt.Errorf("%w: begin/end 不成对（begin=%d end=%d）", ErrTruncated, nb, ne)
	case nb > 1:
		return "", fmt.Errorf("%w: 里面有 %d 段机读块，不知道用哪段", ErrMultiMachine, nb)
	}
	i := strings.Index(text, BeginMarker)
	j := strings.Index(text, EndMarker)
	if j < i {
		return "", fmt.Errorf("%w: end 出现在 begin 之前", ErrTruncated)
	}
	return text[i+len(BeginMarker) : j], nil
}

// keepMachineRune 决定剥离后保留哪些字符：只保留 base64url 字母表 + 分隔符 '.' + 前缀里的 ':'。
// 于是 \r\n\t '>' 全角空格 零宽字符 XSS 包裹符号等全部被剥掉，折行不影响内容（docs/09 §2.2）。
func keepMachineRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '-', r == '_', r == '.', r == ':':
		return true
	}
	return false
}

// splitMachine 剥字符后要求"前缀 + 恰好两段 base64url"。
//
// 剥离后前缀**允许不在第一个字符上**：IM 经常在 begin marker 与机读行之间插
// "&nbsp;"、零宽字符之类的装饰，剥完会剩下 nb sp 这样的字母。真正的机读行从
// MachinePrefix 开始，所以这里按第一个前缀定位。
func splitMachine(machine string) (payloadSeg, sigSeg string, err error) {
	var b strings.Builder
	b.Grow(len(machine))
	for _, r := range machine {
		if keepMachineRune(r) {
			b.WriteRune(r)
		}
	}
	s := b.String()
	at := strings.Index(s, MachinePrefix)
	if at < 0 {
		return "", "", fmt.Errorf("%w: 剥字符后找不到 %q 前缀", ErrTruncated, MachinePrefix)
	}
	body := s[at+len(MachinePrefix):]
	parts := strings.Split(body, ".")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("%w: 剥字符后按 '.' 切出 %d 段（需要 2 段）", ErrTruncated, len(parts))
	}
	if parts[0] == "" {
		return "", "", fmt.Errorf("%w: payload 段为空", ErrTruncated)
	}
	if parts[1] == "" {
		return "", "", fmt.Errorf("%w: 签名段为空", ErrTruncated)
	}
	return parts[0], parts[1], nil
}

// sigSegChars 是 64 字节 ed25519 签名的 base64url 无填充长度（86）。
const sigSegChars = 86

// trimSigJunk 砍掉签名段尾部粘上的 IM 装饰（如 "&nbsp;" 剥完剩下的 nb sp）。
//
// 只砍**尾部多余**字符，且只在长度超过 86 时砍；签名本身仍要验，砍掉的字节
// 不可能改变验签结果（ed25519 只吃前 64 字节）。
func trimSigJunk(sigSeg string) string {
	if len(sigSeg) > sigSegChars {
		return sigSeg[:sigSegChars]
	}
	return sigSeg
}

// decodeB64 解码 base64url；容忍填充与首尾空白（IM 可能加 '='，我们从不产生填充）。
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "=")
	if s == "" {
		return nil, fmt.Errorf("空 base64url")
	}
	out, err := b64.DecodeString(s)
	if err != nil {
		// 再试一次允许填充的形式，兼容第三方产生器。
		if out2, err2 := base64.URLEncoding.DecodeString(strings.TrimSpace(s)); err2 == nil {
			return out2, nil
		}
		return nil, err
	}
	return out, nil
}

// decodePayload 把原始字节反序列化成 Payload；未知字段 / null 一律 fail closed。
func decodePayload(raw []byte) (Payload, error) {
	var p Payload
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&top); err != nil {
		// JSON 都不完整 → 结构性失败（被截断的典型表现）。
		return p, fmt.Errorf("%w: payload 不是合法 JSON 对象: %s", ErrTruncated, err.Error())
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return p, fmt.Errorf("%w: payload 尾部有多余内容", ErrTruncated)
	}
	if top == nil {
		return p, fmt.Errorf("%w: payload 是 null", ErrBadSignature)
	}
	for k, v := range top {
		if _, ok := payloadKeys[k]; !ok {
			return p, fmt.Errorf("%w: 未知字段 %q（卡格式是闭枚的）", ErrBadSignature, k)
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return p, fmt.Errorf("%w: 字段 %q 是 null（规格禁止 null）", ErrBadSignature, k)
		}
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&p); err != nil {
		return p, fmt.Errorf("%w: 字段类型不符: %s", ErrBadSignature, err.Error())
	}
	return p, nil
}

// checkPayload 做闭枚/自洽校验。返回的错误已包好 ErrBadSignature。
func checkPayload(p Payload) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrBadSignature, fmt.Sprintf(format, args...))
	}
	if p.V != CardVersion {
		return bad("未知卡格式版本 v=%d（只支持 %d）", p.V, CardVersion)
	}
	if p.API != APIVersion {
		return bad("未知宿主接口版本 api=%d（只支持 %d）", p.API, APIVersion)
	}
	keyBytes, err := decodeB64(p.Key)
	if err != nil || len(keyBytes) != ed25519.PublicKeySize {
		return bad("key 不是 %d 字节 base64url", ed25519.PublicKeySize)
	}
	if want := RefOf(ed25519.PublicKey(keyBytes)); p.Ref != want {
		return bad("ref=%q 与 sha256(key)=%q 不一致", p.Ref, want)
	}
	if len(p.IDs) == 0 {
		return bad("ids 为空（genesis 必须列全 inviteId）")
	}
	seenID := make(map[string]struct{}, len(p.IDs))
	for _, id := range p.IDs {
		if strings.TrimSpace(id) == "" {
			return bad("ids 里有空串")
		}
		if _, dup := seenID[id]; dup {
			return bad("ids 里 %q 重复", id)
		}
		seenID[id] = struct{}{}
	}
	if p.Uses < 1 {
		return bad("uses=%d（必须 >= 1）", p.Uses)
	}
	if p.Seats < 1 {
		return bad("seats=%d（必须 >= 1）", p.Seats)
	}
	if p.Uses > p.Seats {
		return bad("uses=%d > seats=%d", p.Uses, p.Seats)
	}
	if p.Exp < 0 {
		return bad("exp=%d（负值非法）", p.Exp)
	}
	if p.IAT <= 0 {
		return bad("iat=%d（签发时刻必须给出，毫秒，§10.1⑤）", p.IAT)
	}
	if p.Exp > 0 && p.IAT > p.Exp {
		return bad("iat=%d 晚于 exp=%d（签发时卡已过期）", p.IAT, p.Exp)
	}
	if !isHex16(p.TH) {
		return bad("th=%q 不是 16 位小写 hex", p.TH)
	}
	if len(p.Caps) == 0 {
		return bad("caps 为空")
	}
	seenCap := make(map[string]struct{}, len(p.Caps))
	for _, c := range p.Caps {
		if !KnownCap(c) {
			return bad("未知能力位 %q（闭合枚举）", c)
		}
		if _, dup := seenCap[c]; dup {
			return bad("caps 里 %q 重复", c)
		}
		seenCap[c] = struct{}{}
	}
	// rv 不在这里判：它是没签名的字段，只有验签通过后才能定性（见 Card.Verify / A3）。
	// 数量与长度上限（A7）：签发侧自觉不够，接收侧也得挡，否则一张卡能塞几百 KB。
	if len(p.IDs) > 64 {
		return bad("ids %d 个（上限 64）", len(p.IDs))
	}
	if len(p.EP) > 8 {
		return bad("ep %d 个（上限 8）", len(p.EP))
	}
	if len(p.Caps) > 8 {
		return bad("caps %d 个（上限 8）", len(p.Caps))
	}
	if len(p.HN) > 120 || len(p.ON) > 120 {
		return bad("hn/on 太长")
	}
	if strings.TrimSpace(p.N) == "" {
		return bad("n（卡 nonce）为空")
	}
	for i, ep := range p.EP {
		switch ep.K {
		case EndpointLoopback, EndpointLAN, EndpointTunnel:
		default:
			return bad("ep[%d].k=%q 不在闭枚集合 {loopback,lan,tunnel} 内（§10.1⑥）", i, ep.K)
		}
		if strings.TrimSpace(ep.U) == "" {
			return bad("ep[%d].u 为空", i)
		}
		if ep.K == EndpointLAN && ep.INS != 1 {
			return bad("ep[%d] 是 lan 端点但没有 ins:1（明文入站必须显式标记，§10.1⑥ / G11）", i)
		}
	}
	return nil
}

// checkJoinBy 只在调用方显式给 now 时才判过期（arch A-17：权威判定在宿主）。
func checkJoinBy(p Payload, nowMillis int64) error {
	if nowMillis > 0 && p.Exp > 0 && nowMillis > p.Exp {
		return fmt.Errorf("%w: exp=%d now=%d", ErrExpired, p.Exp, nowMillis)
	}
	return nil
}

// RefOf 返回 base64url_nopad(sha256(pub))，43 字符（arch A-3）。
func RefOf(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return b64.EncodeToString(sum[:])
}

// KeyOf 返回 base64url_nopad(pub)（卡里的 "key" 字段格式）。
func KeyOf(pub ed25519.PublicKey) string { return b64.EncodeToString(pub) }

// PubFromKey 把卡里的 "key" 字段解析成公钥。
func PubFromKey(s string) (ed25519.PublicKey, error) {
	b, err := decodeB64(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: key 不是 %d 字节 base64url", ErrBadSignature, ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// NewNonce 生成卡 nonce（12B → base64url 16 字符，docs/09 §2.3 的 "n"）。
func NewNonce() (string, error) {
	buf := make([]byte, NonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("invite: 生成 nonce: %w", err)
	}
	return b64.EncodeToString(buf), nil
}

// TopicHash 返回 hex(sha256(normalize(topic)))[:16]（docs/09 §2.3 的 th）。
//
// normalizeTopic：把 Unicode 空白与零宽字符折叠成单个空格，并去掉首尾空白。
// **全队只能用这个函数算 th，不要自己写一份归一化**（否则人读块交叉校验会误判）。
func TopicHash(topic string) string {
	sum := sha256.Sum256([]byte(normalizeTopic(topic)))
	return hex.EncodeToString(sum[:])[:16]
}

// normalizeTopic 见 TopicHash 的注释。零宽字符按空白处理。
func normalizeTopic(topic string) string {
	var b strings.Builder
	b.Grow(len(topic))
	space := false
	for _, r := range topic {
		if isSpaceLike(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// isSpaceLike 判定"看起来是空白"的码点：Unicode 空白 + 零宽/方向控制符。
func isSpaceLike(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	switch r {
	case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff', '\u00a0':
		return true
	}
	return false
}

// ShortRef 给人类比对用的短指纹（arch A-7 / §10.1⑦）：ref 前 8 字符。
// 宿主必须把它打进人读块（IssueLayered 会强制检查），人可以用第二个通道核对。
func ShortRef(ref string) string {
	const n = 8
	if len(ref) <= n {
		return ref
	}
	return ref[:n]
}

// HumanError 把哨兵错误翻成 docs/09 §2.4 的人话（原样字符串）。
func HumanError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrHumanTopic):
		return HumanTopicMismatch
	case errors.Is(err, ErrMultiMachine):
		return HumanMultiMachine
	case errors.Is(err, ErrTruncated):
		return HumanTruncated
	case errors.Is(err, ErrBadSignature):
		return HumanBadSignature
	case errors.Is(err, ErrRulesMismatch):
		return HumanRulesMismatch
	case errors.Is(err, ErrWrongRoom):
		return HumanWrongRoom
	case errors.Is(err, ErrExpired):
		return HumanExpired
	}
	return HumanUnknown
}

// isHex16 判定 16 位小写 hex。
func isHex16(s string) bool {
	if len(s) != 16 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// humanTopic 从人读块里抠出「议题」原文（"邀你参加「X」"）。抠不到就当没有，
// 不因为措辞不同就拒卡。
func humanTopic(text string) (string, bool) {
	if i := strings.Index(text, BeginMarker); i >= 0 {
		text = text[:i]
	}
	l := strings.Index(text, "「")
	if l < 0 {
		return "", false
	}
	r := strings.Index(text[l:], "」")
	if r <= 0 {
		return "", false
	}
	topic := strings.TrimSpace(text[l+len("「") : l+r])
	if topic == "" {
		return "", false
	}
	return topic, true
}
