package snowflake

import "testing"

func TestCompare_Numeric(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"9", "10", -1},
		{"10", "9", 1},
		{"123", "123", 0},
		{"99999999999999999", "100000000000000000", -1},
		{"b", "a", 1}, // 無法解析時退回字串比較
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d，預期 %d", c.a, c.b, got, c.want)
		}
	}
	if !Less("9", "10") || Less("10", "9") {
		t.Error("Less 應以數值比較")
	}
}
