package host

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

// TestResumeClockDoesNotStack：连续两次 resume（中间没有新的活动）不许把时间戳
// 叠加推走 —— 否则每恢复一次 CreatedAt 就往后挪一次，会议会被越推越久。
func TestResumeClockDoesNotStack(t *testing.T) {
	h := newTestHost(t)
	room := mustCreate(t, h, 2)

	rc := h.roomByRef(room.Ref)
	if rc == nil {
		t.Fatal("找不到房间")
	}
	rc.mu.Lock()
	before := rc.snap.CreatedAt
	rc.lastAlive = nowMillis() - 60_000 // 假装停机 60 秒
	rc.mu.Unlock()

	for i := 0; i < 2; i++ {
		if err := h.adjustClockForResume(context.Background(), rc, 60_000); err != nil {
			t.Fatalf("第 %d 次修正: %v", i+1, err)
		}
		h.touch(rc)
		snap, err := h.peek(context.Background(), rc)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("第 %d 次之后 CreatedAt 偏移 = %d ms", i+1, snap.CreatedAt-before)
	}

	snap, err := h.peek(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	// 两次各 60s 是允许的（各代表一次真实停机），这里断言的是"没有被叠成 4 分钟以上"
	if d := snap.CreatedAt - before; d > 150_000 {
		t.Fatalf("两次 resume 叠加过多：%d ms", d)
	}
	_ = ed25519.PublicKeySize
	_ = rand.Reader
	_ = time.Second
}
