package budget

import "testing"

func TestEstTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"要不要把定价从年付改月付", 12}, // 12 个汉字 = 12
		{"abc", 1},           // ceil(3/4)
		{"abcd", 1},          // 4/4
		{"abcde", 2},         // ceil(5/4)
		{"同意 agree 2024", 5}, // 2 汉字 + ceil(10/4)=3
		{"。", 1},             // 全角标点按汉字算
	}
	for _, c := range cases {
		if got := EstTokens(c.in); got != c.want {
			t.Errorf("EstTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestReserveLine 锁住三条红线的数值：改它们等于改产品承诺。
func TestReserveLine(t *testing.T) {
	if ReservePct != 5 {
		t.Fatalf("预留必须正好 5%%，当前 %d", ReservePct)
	}
	if WarnPct != 80 || HardPct != 95 {
		t.Fatalf("提醒线/强制收敛线被改动: %d / %d", WarnPct, HardPct)
	}
	// 95% 线与 5% 预留必须严丝合缝：辩论轮花到 95% 后，
	// 剩下的 5% 正好够每人交一条 finalStance。
	if HardPct+ReservePct != 100 {
		t.Fatalf("强制收敛线(%d) + 预留(%d) 必须等于 100", HardPct, ReservePct)
	}
	if got := HardCap(1000); got != 950 {
		t.Fatalf("HardCap(1000) = %d, want 950", got)
	}
}

func TestView(t *testing.T) {
	v := View(62000, 200000)
	if v.Used != 62000 || v.Total != 200000 || v.Left != 138000 || v.Pct != 31 {
		t.Fatalf("View 不符: %+v", v)
	}
	// 超支时 left 不出现负数
	if got := View(1100, 1000).Left; got != 0 {
		t.Fatalf("超支 left 应为 0，得到 %d", got)
	}
}
