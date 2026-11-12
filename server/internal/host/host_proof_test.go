package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"agchat/internal/invite"
)

// TestJoinRequiresKeyProof：join 必须证明"私钥在我手上"。
//
// 没有这条，agentPub 是公开的（日志、收据里都有），谁都能拿别人的公钥来 join，
// 把对方 seat 上的 token 顶掉。
func TestJoinRequiresKeyProof(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 2)
	card := mustIssue(t, h, room.Ref, 2)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	nonce := "proof-nonce"
	msg := JoinProofMessage(card.Card, nonce)

	// 不带 proof → 400 proof_required
	_, err = h.Join(context.Background(), JoinReq{Card: card.Card, AgentPub: pubB64(pub), Nonce: nonce, Alias: "甲"})
	if oe, ok := err.(*opError); !ok || oe.Code != "proof_required" {
		t.Fatalf("缺 proof 应 400 proof_required，实际 %v", err)
	}

	// 用**别人的**私钥签 → 401 bad_proof
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	badProof := pubB64Of(ed25519.Sign(otherPriv, msg))
	_, err = h.Join(context.Background(), JoinReq{Card: card.Card, AgentPub: pubB64(pub), Nonce: nonce, Proof: badProof, Alias: "甲"})
	if oe, ok := err.(*opError); !ok || oe.Code != "bad_proof" {
		t.Fatalf("错误私钥应 401 bad_proof，实际 %v", err)
	}

	// 正确签名 → 通过
	good := pubB64Of(ed25519.Sign(priv, msg))
	res, err := h.Join(context.Background(), JoinReq{Card: card.Card, AgentPub: pubB64(pub), Nonce: nonce, Proof: good, Alias: "甲"})
	if err != nil {
		t.Fatalf("正确 proof 应通过，实际 %v", err)
	}
	if !res.Joined || res.AgentID == "" {
		t.Fatalf("没真的进场: %+v", res)
	}
}

// 顺手钉住签名域的形状：卡与 ref 两种载体要签出不同的字节，避免串场。
func TestJoinProofDomainDistinct(t *testing.T) {
	a := string(JoinProofMessage("card-text", "n1"))
	b := string(JoinProofMessage("ref-text", "n1"))
	if a == b {
		t.Fatal("卡与 ref 的 join 证明域相同，会串场")
	}
	if a == string(JoinProofMessage("card-text", "n2")) {
		t.Fatal("非ce 没有参与签名域")
	}
	_ = invite.CapJoin
}
