package host

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"agchat/internal/invite"
)

// b64url 是全程唯一的 base64url 无填充编码（与 invite/oplog 同口径）。
var b64url = base64.RawURLEncoding

// roomSecret 是 rooms/<ref>/host.key 的形状（0600，永不外发）。
//
// ★ 这里存 roomKey 私钥与房间密码，都**只在本机**：
//   - 房间密码是 engine.Join 的唯一鉴权；卡里没有它也永远不会有（G7）；
//   - roomKey 私钥用于签 create/advance/end/reject 与 ack，也用于签发卡。
//   - adminKey 是 engine.Close 的入参（A-11：本地宿主不引入对外 adminKey，
//     这里只是把 Create 返回的内部值留在宿主侧）。
type roomSecret struct {
	V         int    `json:"v"`
	Ref       string `json:"ref"`
	RoomID    string `json:"roomId"`
	Password  string `json:"password"`
	AdminKey  string `json:"adminKey"`
	RoomPriv  string `json:"roomPriv"`
	RoomPub   string `json:"roomPub"`
	CreatedAt int64  `json:"createdAt"`
}

func newRoomKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return pub, priv, nil
}

// refOf 与 invite.RefOf 同口径（base64url_nopad(sha256(pub))）。
// 直接用 invite.RefOf，避免第二份实现（arch A-3 的规范身份只能有一处）。
func refOf(pub ed25519.PublicKey) string { return invite.RefOf(pub) }

// joinKey = sha256(agentPub ‖ inviteId ‖ nonce)（arch A-14）。
// 重复 joinKey 不重复扣 uses，返回同一座位。
func joinKey(agentPub []byte, inviteID, nonce string) string {
	h := sha256.New()
	h.Write(agentPub)
	h.Write([]byte{0})
	h.Write([]byte(inviteID))
	h.Write([]byte{0})
	h.Write([]byte(nonce))
	return hex.EncodeToString(h.Sum(nil))
}

func pubB64(pub ed25519.PublicKey) string { return b64url.EncodeToString(pub) }

func pubFromB64(s string) (ed25519.PublicKey, error) {
	b, err := b64url.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("公钥不是 %d 字节 base64url", ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

func privFromB64(s string) (ed25519.PrivateKey, error) {
	b, err := b64url.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("私钥不是 %d 字节 base64url", ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(b), nil
}

func privB64(priv ed25519.PrivateKey) string { return b64url.EncodeToString(priv) }

// uniqueAlias 强制 alias 唯一（arch A-18）：冲突就加后缀。
// alias 只用于展示，**永不作为授权依据** —— 授权只认 token/agentPub。
func uniqueAlias(want string, taken map[string]bool) string {
	name := strings.TrimSpace(want)
	if name == "" {
		name = "参会者"
	}
	if !taken[name] {
		return name
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s-%d", name, i)
		if !taken[cand] {
			return cand
		}
	}
	return name + "-x"
}

// sortedKeys 返回 map 的升序键（advance 报告的确定性）。
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
