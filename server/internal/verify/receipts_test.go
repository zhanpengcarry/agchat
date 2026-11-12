package verify

import (
	"testing"

	"agchat/internal/oplog"
)

// TestReceiptsCatchOmittedOp：宿主漏记一条发言（连它的 ack 一起删、重封链、
// 重算声明值），包内完全自洽 —— 只有参与者手上的收据能咬住它。
func TestReceiptsCatchOmittedOp(t *testing.T) {
	f := cleanFixture(t)
	fullAcks := append([]oplog.Ack{}, f.acks...)

	// 删掉一条参与者发言（找 m_1 的 post）并重封链 + 重算声明值
	idx := f.stepIdxOf("m_1", "post")
	f.steps = append(f.steps[:idx], f.steps[idx+1:]...)
	f.seal()
	f.captureDeclared()

	b := f.bundle()
	b.Acks = nil // 宿主把那条 ack 也删了
	for _, a := range f.acks {
		b.Acks = append(b.Acks, a)
	}

	rep, err := Run(b)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.State != StateVerified {
		t.Fatalf("没有收据时这份包应当自洽通过（否则测不到点子上），实际 %s/%s", rep.State, rep.Code)
	}

	rep2, err := RunWithReceipts(b, fullAcks)
	if err != nil {
		t.Fatalf("RunWithReceipts: %v", err)
	}
	if rep2.State != StateFailed {
		t.Fatalf("带收据时应判 failed，实际 %s（code=%s）", rep2.State, rep2.Code)
	}
	if rep2.Code != CodeAckOrphan {
		t.Fatalf("reason code 应为 %q，实际 %q", CodeAckOrphan, rep2.Code)
	}
	if rep2.FailAt == 0 {
		t.Fatal("FailAt 应指向缺失的那一条")
	}
}

// TestReceiptsCleanBundleStillVerified：干净包 + 收据不许误判。
func TestReceiptsCleanBundleStillVerified(t *testing.T) {
	f := cleanFixture(t)
	rep, err := RunWithReceipts(f.bundle(), f.acks)
	if err != nil {
		t.Fatalf("RunWithReceipts: %v", err)
	}
	if rep.State != StateVerified {
		t.Fatalf("干净包 + 自己的收据应 verified，实际 %s（code=%s reasons=%v）", rep.State, rep.Code, rep.Reasons)
	}
}
