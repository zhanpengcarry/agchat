package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"testing"

	"agchat/internal/oplog"
)

// benchRoom 造一场 N 人会、全员入场、已开局，用于跑 op 吞吐。
func benchRoom(b *testing.B, seats int) (*Host, *roomCtx, []*seat) {
	b.Helper()
	h := newTestHostB(b)
	room := mustCreateB(b, h, seats)
	card := mustIssueB(b, h, room.Ref, seats)
	for i := 0; i < seats; i++ {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			b.Fatal(err)
		}
		nonce := fmt.Sprintf("bn-%d", i)
		if _, err := h.Join(context.Background(), JoinReq{
			Card: card.Card, AgentPub: pubB64(pub), Nonce: nonce,
			Proof: proofFor(priv, card.Card, nonce), Alias: nonce,
		}); err != nil {
			b.Fatalf("join: %v", err)
		}
		_ = priv
	}
	rc := h.roomByRef(room.Ref)
	out := make([]*seat, 0, len(rc.seats))
	for _, s := range rc.seats {
		out = append(out, s)
	}
	return h, rc, out
}

// BenchmarkStatusRead 量 Status（控制面只读，最频繁的一类读取）。
func BenchmarkStatusRead(b *testing.B) {
	h, rc, _ := benchRoom(b, 6)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.Status(ctx, rc.ref); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPeek 量一次"只读载入聚合"的成本（每次请求都会做）。
func BenchmarkPeek(b *testing.B) {
	h, rc, _ := benchRoom(b, 6)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := h.peek(ctx, rc); err != nil {
			b.Fatal(err)
		}
	}
}

// ---- b 版本（*testing.B）的建会/发卡辅助 ----

func newTestHostB(b *testing.B) *Host {
	b.Helper()
	home := b.TempDir()
	cfg := DefaultConfig()
	cfg.Home = home
	cfg.DBPath = home + "/agchat.db"
	cfg.Addr = "127.0.0.1:0"
	cfg.DisplayName = "bench"
	cfg.OwnerName = "房主"
	h, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(h.Close)
	return h
}

func mustCreateB(b *testing.B, h *Host, seats int) *CreateResp {
	b.Helper()
	res, err := h.Create(context.Background(), CreateReq{Topic: "bench", Seats: seats, Rounds: 6, Minutes: 10})
	if err != nil {
		b.Fatal(err)
	}
	return res
}

func mustIssueB(b *testing.B, h *Host, ref string, uses int) *IssueResp {
	b.Helper()
	res, err := h.Issue(context.Background(), IssueReq{Ref: ref, Uses: uses})
	if err != nil {
		b.Fatal(err)
	}
	return res
}

var _ = oplog.OpPost
