// Package oplog 是操作日志/哈希链/收据/证据包的公共基础，**纯 stdlib、零业务依赖**（arch A-25）。
//
// 它是三件东西的唯一实现，别处不许再造另一份规范编码（arch §6.2）：
//   - 规范编码 CanonicalJSON（键升序、无空格、无 null、无浮点）
//   - op 链 h = sha256("agchat-op:v1" + 0x00 + canonical{i,t,o,a,p,ph})
//   - 证据包 bundle.zip 的读写，以及 transcriptHash（C-1）
//
// 签名域一律带 0x00 分隔，且**不跨域复用**。本包只提供 op / ack 两个域的签名入口
// （SignOp / SignAck），邀请卡的域 `agchat-invite:v1` 只在 internal/invite 里出现——
// 用错域写不出来，是设计目标（§10.2② / arch 门禁 G2：不提供通用 Sign()）。
// `agchat-control:v1` 是**保留域，S1 未使用**（S1 控制面是 ~/.agchat/control.token
// 的 bearer token，不是签名）。
package oplog

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 签名域常量（含 0x00 域分隔，防跨域复用）。
const (
	OpDomain   = "agchat-op:v1\x00"
	AckDomain  = "agchat-ack:v1\x00"
	HashDomain = "agchat-op:v1"
	Protocol   = "agchat/1"

	// OpsVersion / BundleVersion 是日志与证据包的格式版本（§10.2④）。
	OpsVersion    = 1
	BundleVersion = 1
)

// ZeroHash 是首条 op 的 ph（64 个 0）。
const ZeroHash = "0000000000000000000000000000000000000000000000000000000000000000"

// op 种类（docs/09 §4）。**本包不强制闭枚**：R-4 的 commit 标记等宿主自有行
// 也要进 jsonl，校验职责在 verify，不在格式包。
const (
	OpCreate  = "create"
	OpInvite  = "invite"
	OpJoin    = "join"
	OpPost    = "post"
	OpPropose = "propose"
	OpVote    = "vote"
	OpSummary = "summary"
	OpAdvance = "advance"
	OpEnd     = "end"
	OpReject  = "reject"
)

// advance 的 reason code 闭枚（§10.2⑥）。
const (
	ReasonTurnTimeout       = "turn_timeout"
	ReasonRoundIdle         = "round_idle"
	ReasonRoundsDone        = "rounds_done"
	ReasonBudget            = "budget"
	ReasonTimeLimit         = "time_limit"
	ReasonVotesComplete     = "votes_complete"
	ReasonSummaryDone       = "summary_done"
	ReasonSummaryTimeout    = "summary_timeout"
	ReasonNoWinner          = "no_winner"
	ReasonAdminClose        = "admin_close"
	ReasonResumeClockAdjust = "resume_clock_adjust"
)

// Reasons 是 §10.2⑥ 冻结的 reason 全集（升序，供 verify 与测试共用）。
var Reasons = []string{
	ReasonAdminClose,
	ReasonBudget,
	ReasonNoWinner,
	ReasonResumeClockAdjust,
	ReasonRoundIdle,
	ReasonRoundsDone,
	ReasonSummaryDone,
	ReasonSummaryTimeout,
	ReasonTimeLimit,
	ReasonTurnTimeout,
	ReasonVotesComplete,
}

// KnownReason 报告 reason 是否在 §10.2⑥ 的闭枚集合内。
func KnownReason(reason string) bool {
	i := sort.SearchStrings(Reasons, reason)
	return i < len(Reasons) && Reasons[i] == reason
}

// 哨兵错误。
var (
	ErrInvalidHash = errors.New("oplog: hash 不合法")
	ErrInvalidSig  = errors.New("oplog: 签名不合法")
	ErrChainBroken = errors.New("oplog: 链断开")
	ErrBadJSON     = errors.New("oplog: JSON 不合法")
	ErrBadBundle   = errors.New("oplog: 证据包不合法")
)

// b64 是全程唯一的 base64url 无填充编码。
var b64 = base64.RawURLEncoding

// Op 是一条操作记录，JSON 键严格是 docs/09 §4 的 i,t,o,a,p,ph,h,s。
// `s` 不参与 hash（§10.2②）。
type Op struct {
	I  int             `json:"i"`
	T  int64           `json:"t"`
	O  string          `json:"o"`
	A  string          `json:"a"`
	P  json.RawMessage `json:"p"`
	PH string          `json:"ph"`
	H  string          `json:"h"`
	S  string          `json:"s"`
}

// Ack 是宿主对一条被接受的 op 的收据：**由 roomKey 签 {ref,i,h,t}**（§10.2③；S1 无 hostKey）。
type Ack struct {
	Ref string `json:"ref"`
	I   int    `json:"i"`
	H   string `json:"h"`
	T   int64  `json:"t"`
	S   string `json:"s"`
}

// Seat 是一个已被宿主接纳的座位。三方共用（host 产出 / verify 重建 / 客户端认回），
// **不许各自定义**（§10.2①）。
type Seat struct {
	Ref       string `json:"ref"`
	InviteID  string `json:"inviteId"`
	JoinKey   string `json:"joinKey"`
	AgentPub  string `json:"agentPub"`
	AgentID   string `json:"agentId"`
	TokenHash string `json:"tokenHash"`
	Alias     string `json:"alias"`
	Ord       int    `json:"ord"`
}

// SeatBinding 是 joinKey → 座位的幂等绑定（arch A-14）：重复 joinKey 不重复扣 uses、
// 返回同一座位。三方共用（§10.2①）。
type SeatBinding struct {
	Ref      string `json:"ref"`
	JoinKey  string `json:"joinKey"`
	InviteID string `json:"inviteId"`
	AgentPub string `json:"agentPub"`
	Ord      int    `json:"ord"`
}

// TranscriptMsg 是转写稿的一条记录（C-1 / §10.2⑤）。alias 与 seq 由调用方从 op 流里取，
// 本包不认识房间聚合（B-13：重放不许回查库或调 room）。
type TranscriptMsg struct {
	I       int
	Seq     int
	Alias   string
	Round   int
	Text    string
	ReplyTo *int
	Support string
}

// Expect 是**声明值**（§10.2④）：verify 不许信赖它，只用它做差异定位。
type Expect struct {
	DecisionHash   string `json:"decisionHash"`
	TranscriptHash string `json:"transcriptHash"`
}

// Manifest 钉住规则版本与硬上限（arch C-3），否则"重算不符"无法区分篡改与代码升级。
type Manifest struct {
	Ref           string   `json:"roomRef"`
	Protocol      string   `json:"protocol"`
	ResultSchema  string   `json:"resultSchema"`
	VoteRule      string   `json:"voteRule"`
	HashDomain    string   `json:"hashDomain"`
	Seats         int      `json:"seats"`
	InviteIDs     []string `json:"inviteIds"`
	MaxRounds     int      `json:"maxRounds"`
	MaxAgents     int      `json:"maxAgents"`
	MaxMinutes    int      `json:"maxMinutes"`
	MaxOutput     int      `json:"maxOutput"`
	Topic         string   `json:"topic"`
	OpsVersion    int      `json:"opsVersion"`
	BundleVersion int      `json:"bundleVersion"`
	Expect        Expect   `json:"expect"`
}

// Bundle 是证据包内容。Ops 必须是**已提交前缀**（R-4），筛选是宿主的责任。
type Bundle struct {
	Manifest       Manifest        `json:"manifest"`
	Ops            []Op            `json:"-"`
	Acks           []Ack           `json:"-"`
	Result         json.RawMessage `json:"-"`
	DecisionHash   string          `json:"decisionHash"`
	TranscriptHash string          `json:"transcriptHash"`
}

// Hash 计算 op 链头：sha256(OpDomain || canonical{i,t,o,a,p,ph})，hex（64 位小写）。
//
// payload 会先被规范编码再入域，所以调用方不必自己先规范化；但**存进 Op.P 的必须是
// 同一份规范字节**（用 CanonicalJSON 或 Seal/SealOp 生成即可保证）。
func Hash(prevHex string, i int, t int64, o, a string, payload []byte) (string, error) {
	ph, err := NormalizeHash(prevHex)
	if err != nil {
		return "", err
	}
	if i < 1 {
		return "", fmt.Errorf("%w: i=%d（链内序号从 1 开始）", ErrInvalidHash, i)
	}
	if t <= 0 {
		return "", fmt.Errorf("%w: t=%d（必须是毫秒时间戳）", ErrInvalidHash, t)
	}
	if strings.TrimSpace(o) == "" {
		return "", fmt.Errorf("%w: o 为空", ErrInvalidHash)
	}
	if strings.TrimSpace(a) == "" {
		return "", fmt.Errorf("%w: a 为空（作者座位，宿主用 \"host\"）", ErrInvalidHash)
	}
	if len(bytesTrim(payload)) == 0 {
		return "", fmt.Errorf("%w: op 载荷为空（无载荷请传 {}）", ErrBadJSON)
	}
	canon, err := canonicalize(payload)
	if err != nil {
		return "", err
	}
	rec := map[string]any{
		"i":  i,
		"t":  t,
		"o":  o,
		"a":  a,
		"p":  json.RawMessage(canon),
		"ph": ph,
	}
	enc, err := CanonicalJSON(rec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(OpDomain), enc...))
	return hex.EncodeToString(sum[:]), nil
}

// HashOp 是 Hash 的便利包装：Hash(op.PH, op.I, op.T, op.O, op.A, op.P)。
func HashOp(op Op) (string, error) {
	return Hash(op.PH, op.I, op.T, op.O, op.A, op.P)
}

// NormalizeHash 校验并规范化一个链头："" 视为 ZeroHash，其余必须 64 位 hex（大小写不敏感，
// 输出小写）。
func NormalizeHash(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return ZeroHash, nil
	}
	if len(h) != 64 {
		return "", fmt.Errorf("%w: %q 不是 64 位 hex", ErrInvalidHash, h)
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("%w: %q 不是 hex", ErrInvalidHash, h)
		}
	}
	return h, nil
}

// Seal 生成一条签名好的 op：规范化载荷 → 算 h → 签名 → 填 ph/h/s。
// prevHex 传上一条的 h（首条传 "" 或 oplog.ZeroHash）；i 传链内序号（首条为 1）。
func Seal(priv ed25519.PrivateKey, prevHex string, i int, t int64, o, a string, payload any) (Op, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return Op{}, fmt.Errorf("%w: 私钥长度 %d", ErrInvalidSig, len(priv))
	}
	raw, err := CanonicalJSON(payload)
	if err != nil {
		return Op{}, err
	}
	ph, err := NormalizeHash(prevHex)
	if err != nil {
		return Op{}, err
	}
	h, err := Hash(ph, i, t, o, a, raw)
	if err != nil {
		return Op{}, err
	}
	sig := SignOp(priv, h)
	if sig == "" {
		return Op{}, fmt.Errorf("%w: 签名失败", ErrInvalidSig)
	}
	return Op{I: i, T: t, O: o, A: a, P: raw, PH: ph, H: h, S: sig}, nil
}

// SealOp 用 op 里已填好的 i/t/o/a/p(/ph) 生成签名，返回填好 ph/h/s 的 op。
// 参与者侧「先签名再交给宿主」就用这个：i/t/ph 由作者填，宿主只验签不重签。
func SealOp(op Op, priv ed25519.PrivateKey) (Op, error) {
	return Seal(priv, op.PH, op.I, op.T, op.O, op.A, op.P)
}

// HeadOf 返回链头（末条 op 的 h；空日志返回 ZeroHash）。纯函数，不需要落盘。
func HeadOf(ops []Op) string {
	if len(ops) == 0 {
		return ZeroHash
	}
	h, err := NormalizeHash(ops[len(ops)-1].H)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(ops[len(ops)-1].H))
	}
	return h
}

// SignOp 对 op 链头签名：域 = OpDomain || hHex，返回 base64url_nopad(sig64)。
// 签名对象是 **h 的 hex 字符串的字节**（不含 s 字段）。私钥不合法返回 ""。
func SignOp(priv ed25519.PrivateKey, hHex string) string {
	if len(priv) != ed25519.PrivateKeySize {
		return ""
	}
	h, err := NormalizeHash(hHex)
	if err != nil {
		return ""
	}
	if h == ZeroHash {
		// ZeroHash 不是合法的 op 链头（没有 op 会以它作为自己的 h），拒绝签名。
		return ""
	}
	return b64.EncodeToString(ed25519.Sign(priv, append([]byte(OpDomain), h...)))
}

// Sign 已按 arch 门禁 G2 **物理删除**：本包只提供带域的签名入口（SignOp / SignAck），
// 没有通用 Sign()，用错域在编译期就写不出来。

// VerifySig 验证 op 签名：域 = OpDomain || hHex。
func VerifySig(hHex, sigB64 string, pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: 公钥长度 %d", ErrInvalidSig, len(pub))
	}
	h, err := NormalizeHash(hHex)
	if err != nil {
		return err
	}
	if h == ZeroHash {
		return fmt.Errorf("%w: 链头是 ZeroHash，不可能有 op 用它", ErrInvalidSig)
	}
	sig, err := decodeB64(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: 签名不是 %d 字节 base64url", ErrInvalidSig, ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, append([]byte(OpDomain), h...), sig) {
		return fmt.Errorf("%w: 验不过", ErrInvalidSig)
	}
	return nil
}

// SignAck 让宿主签一条收据：域 = AckDomain || canonical{ref,i,h,t}（§10.2③：用 roomKey）。
func SignAck(priv ed25519.PrivateKey, ref string, i int, hHex string, t int64) (Ack, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return Ack{}, fmt.Errorf("%w: 私钥长度 %d", ErrInvalidSig, len(priv))
	}
	if strings.TrimSpace(ref) == "" {
		return Ack{}, fmt.Errorf("%w: ack.ref 为空", ErrInvalidSig)
	}
	if i < 1 {
		return Ack{}, fmt.Errorf("%w: ack.i=%d", ErrInvalidSig, i)
	}
	if t <= 0 {
		return Ack{}, fmt.Errorf("%w: ack.t=%d", ErrInvalidSig, t)
	}
	h, err := NormalizeHash(hHex)
	if err != nil || h == ZeroHash {
		return Ack{}, fmt.Errorf("%w: ack.h 不合法", ErrInvalidSig)
	}
	body, err := ackBody(ref, i, h, t)
	if err != nil {
		return Ack{}, err
	}
	sig := ed25519.Sign(priv, append([]byte(AckDomain), body...))
	return Ack{Ref: ref, I: i, H: h, T: t, S: b64.EncodeToString(sig)}, nil
}

// VerifyAck 验证收据签名。
func VerifyAck(a Ack, pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("%w: 公钥长度 %d", ErrInvalidSig, len(pub))
	}
	if strings.TrimSpace(a.Ref) == "" {
		return fmt.Errorf("%w: ack.ref 为空", ErrInvalidSig)
	}
	if a.I < 1 {
		return fmt.Errorf("%w: ack.i=%d", ErrInvalidSig, a.I)
	}
	if a.T <= 0 {
		return fmt.Errorf("%w: ack.t=%d", ErrInvalidSig, a.T)
	}
	h, err := NormalizeHash(a.H)
	if err != nil || h == ZeroHash {
		return fmt.Errorf("%w: ack.h 不合法", ErrInvalidSig)
	}
	body, err := ackBody(a.Ref, a.I, h, a.T)
	if err != nil {
		return err
	}
	sig, err := decodeB64(a.S)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: ack.s 不是 %d 字节 base64url", ErrInvalidSig, ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, append([]byte(AckDomain), body...), sig) {
		return fmt.Errorf("%w: ack 验不过", ErrInvalidSig)
	}
	return nil
}

// ackBody 是 ack 的签名体：canonical{ref,i,h,t}。
func ackBody(ref string, i int, hHex string, t int64) ([]byte, error) {
	return CanonicalJSON(map[string]any{
		"ref": ref,
		"i":   i,
		"h":   hHex,
		"t":   t,
	})
}

// TranscriptHash 返回 sha256(canonical[{i,seq,alias,round,text,replyTo,support}])，
// 形状与 decisionHash 一致（"sha256:" + 64 位 hex，见 server/internal/room/result.go:1029）。
//
// 与 decisionHash 的分工：decisionHash 锚**结论**，transcriptHash 锚**过程**（C-1）。
// `replyTo` / `support` 为空时用 nil / ""，规范编码里这两个键 omitempty。
func TranscriptHash(msgs []TranscriptMsg) (string, error) {
	arr := make([]any, 0, len(msgs))
	for idx, m := range msgs {
		if m.I < 1 {
			return "", fmt.Errorf("%w: 第 %d 条消息 i=%d（必须 >= 1）", ErrBadJSON, idx+1, m.I)
		}
		if m.Seq < 0 {
			return "", fmt.Errorf("%w: 第 %d 条消息 seq=%d", ErrBadJSON, idx+1, m.Seq)
		}
		if strings.TrimSpace(m.Alias) == "" {
			return "", fmt.Errorf("%w: 第 %d 条消息 alias 为空（alias 必须来自 op 载荷）", ErrBadJSON, idx+1)
		}
		if m.Round < 0 {
			return "", fmt.Errorf("%w: 第 %d 条消息 round=%d", ErrBadJSON, idx+1, m.Round)
		}
		rec := map[string]any{
			"i":     m.I,
			"seq":   m.Seq,
			"alias": m.Alias,
			"round": m.Round,
			"text":  m.Text,
		}
		if m.ReplyTo != nil {
			rec["replyTo"] = *m.ReplyTo
		}
		if m.Support != "" {
			rec["support"] = m.Support
		}
		arr = append(arr, rec)
	}
	enc, err := CanonicalJSON(arr)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(enc)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VerifyChain 逐条校验 ph / h / 签名，返回首个失败位置（**1-based 序号**）与错误；
// 全部通过返回 (0, nil)。
//
// key 按 op 返回作者公钥（宿主 op 用 roomKey，参与者 op 用 agentKey）。
// 首条 op 的 ph 必须是 ZeroHash（允许留空，按 ZeroHash 处理）。
func VerifyChain(ops []Op, key func(Op) (ed25519.PublicKey, error)) (int, error) {
	if key == nil {
		return 0, fmt.Errorf("%w: key 回调为空", ErrChainBroken)
	}
	prev := ZeroHash
	for idx, op := range ops {
		pos := idx + 1
		ph, err := NormalizeHash(op.PH)
		if err != nil {
			return pos, fmt.Errorf("%w: 第 %d 条: %v", ErrChainBroken, pos, err)
		}
		if ph != prev {
			return pos, fmt.Errorf("%w: 第 %d 条 ph=%s，应为上一条 h=%s", ErrChainBroken, pos, ph, prev)
		}
		h, err := HashOp(op)
		if err != nil {
			return pos, fmt.Errorf("%w: 第 %d 条重算失败: %v", ErrChainBroken, pos, err)
		}
		if !strings.EqualFold(h, strings.TrimSpace(op.H)) {
			return pos, fmt.Errorf("%w: 第 %d 条 h 不匹配（重算 %s，记录 %q）", ErrChainBroken, pos, h, op.H)
		}
		pub, err := key(op)
		if err != nil {
			return pos, fmt.Errorf("%w: 第 %d 条取作者公钥: %v", ErrChainBroken, pos, err)
		}
		if err := VerifySig(h, op.S, pub); err != nil {
			return pos, fmt.Errorf("%w: 第 %d 条签名无效: %v", ErrChainBroken, pos, err)
		}
		prev = h
	}
	return 0, nil
}

// decodeB64 解码 base64url；容忍填充与首尾空白。
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	if s == "" {
		return nil, fmt.Errorf("空 base64url")
	}
	out, err := b64.DecodeString(s)
	if err != nil {
		if out2, err2 := base64.URLEncoding.DecodeString(strings.TrimRight(s, "=")); err2 == nil {
			return out2, nil
		}
		return nil, err
	}
	return out, nil
}

// bytesTrim 是小工具：去掉首尾空白（避免为了一个 TrimSpace 引入 bytes 依赖的歧义）。
func bytesTrim(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpaceByte(b[start]) {
		start++
	}
	for end > start && isSpaceByte(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
