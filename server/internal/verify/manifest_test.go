package verify

import (
	"testing"

	"agchat/internal/oplog"
)

// TestVerifyManifestTamperFails：manifest 是打包者写的文件，改了就必受影响。
//
// 用来指认"这是哪一场会"的 ref/topic/seats 都在 create 载荷里签过名，
// manifest 里那份必须与它一致；否则一份通过的包可以改个 topics 冒充另一场会，
// 而两个结论指纹一个字都不用动 —— 那种 verified 是指认不了场次的 verified。
func TestVerifyManifestTamperFails(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*testing.T, *fixture, *oplog.Bundle)
	}{
		{"改 topic", func(t *testing.T, f *fixture, b *oplog.Bundle) {

			b.Manifest.Topic = "完全无关的另一件事"
		}},
		{"改 seats", func(t *testing.T, f *fixture, b *oplog.Bundle) {

			b.Manifest.Seats = 99
		}},
		{"塞一个没签过的 inviteId", func(t *testing.T, f *fixture, b *oplog.Bundle) {

			b.Manifest.InviteIDs = []string{"iv_1", "iv_fake"}
		}},
		{"换 ref", func(t *testing.T, f *fixture, b *oplog.Bundle) {

			b.Manifest.Ref = "OTHERREF"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := cleanFixture(t)
			b := f.bundle()
			c.apply(t, f, b)
			rep, err := Run(b)
			if err != nil {
				t.Fatalf("Run 返回 error：%v", err)
			}
			if rep.State != StateFailed {
				t.Fatalf("manifest 被改应判 failed，实际 %s（code=%s reasons=%v）", rep.State, rep.Code, rep.Reasons)
			}
			if rep.Code != CodeManifestMismatch {
				t.Fatalf("code 应为 %q，实际 %q", CodeManifestMismatch, rep.Code)
			}
		})
	}
}

// TestVerifyCleanManifestPasses 反向确认：干净包的 manifest 没被误判。
func TestVerifyCleanManifestPasses(t *testing.T) {
	f := cleanFixture(t)
	rep, err := Run(f.bundle())
	if err != nil {
		t.Fatalf("Run 返回 error：%v", err)
	}
	if rep.State != StateVerified {
		t.Fatalf("干净包应 verified，实际 %s（code=%s reasons=%v）", rep.State, rep.Code, rep.Reasons)
	}
}
