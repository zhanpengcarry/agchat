package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"agchat/internal/oplog"
)

// TestRejectedOpKeyCanRetry：被拒的 op 用同一个 opKey 重发，必须还能真正生效。
//
// 之前幂等表不分成功失败：第一次被拒也登记了 opKey，重放时返回 200 duplicate:true
// 且 result 为空 —— 客户端看到 200 就当成功、不再重发，发言就静默丢了。
func TestRejectedOpKeyCanRetry(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 2) // 2 席 2 人：进场即开局，免得卡在 briefing
	card := mustIssue(t, h, room.Ref, 2)

	join := func(nonce string) (ed25519.PrivateKey, string, string) {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		res, err := h.Join(context.Background(), JoinReq{Card: card.Card, AgentPub: pubB64(pub), Nonce: nonce, Proof: proofFor(priv, card.Card, nonce), Alias: nonce})
		if err != nil {
			t.Fatalf("join %s: %v", nonce, err)
		}
		return priv, res.AgentID, res.Token
	}
	privA, idA, tokA := join("甲")
	privB, idB, tokB := join("乙")
	rc := h.roomByRef(room.Ref)

	mk := func(priv ed25519.PrivateKey, agentID string) oplog.Op {
		op, err := oplog.Seal(priv, rc.log.head(), rc.log.nextIndex(), nowMillis(), oplog.OpPost, agentID,
			map[string]any{"text": "一句话", "seq": 1, "round": 1, "agentId": agentID, "alias": agentID})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}

	// 乙此时不该发言（第 1 轮按入场顺序，主席先）
	_, _, err := h.applyAgentOp(context.Background(), oplog.OpPost, tokB, AgentOpReq{Op: mk(privB, idB), OpKey: "retry-1"})
	if err == nil {
		t.Fatal("第一轮就该轮到主席，乙发言竟然成功")
	}

	// 甲发言把话筒交出去
	if _, _, err := h.applyAgentOp(context.Background(), oplog.OpPost, tokA, AgentOpReq{Op: mk(privA, idA), OpKey: "a-1"}); err != nil {
		if _, _, err2 := h.applyAgentOp(context.Background(), oplog.OpPost, tokA, AgentOpReq{Op: mk(privA, idA), OpKey: "a-1b"}); err2 != nil {
			t.Fatalf("甲的发言也失败: %v / %v", err, err2)
		}
	}

	// 同一个 opKey 重发：必须真的生效（而不是 200 + nil）
	res, ack, err := h.applyAgentOp(context.Background(), oplog.OpPost, tokB, AgentOpReq{Op: mk(privB, idB), OpKey: "retry-1"})
	if err != nil {
		t.Fatalf("换到乙的轮次后，同 opKey 重发应成功，实际 %v", err)
	}
	if res == nil {
		t.Fatal("成功了却没有任何结果")
	}
	if ack == nil {
		t.Fatal("成功了却没有 ack（说明走了 duplicate 分支）")
	}

	// 真重复：同一个 key 再发一次，这次才是 duplicate
	_, ack2, err := h.applyAgentOp(context.Background(), oplog.OpPost, tokB, AgentOpReq{Op: mk(privB, idB), OpKey: "retry-1"})
	if err != nil {
		t.Fatalf("重复重放不该报错: %v", err)
	}
	if ack2 != nil {
		t.Fatal("第二次重放应命中幂等（ack 为 nil）")
	}
}
