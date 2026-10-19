package snowflake

import (
	"strconv"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestCompare_Numeric(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"9", "10", -1},
		{"10", "9", 1},
		{"123", "123", 0},
		{"99999999999999999", "100000000000000000", -1},
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

// 階段 6 後決定 7：不合法的值（空字串、非數字）不退回字串比較：一律排在所有合法 ID 之後，
// 彼此視為相等。所以合法 ID 一定「早於」不合法的 setAt（安全側：當舊圖片訊息，不補處理）。
func TestCompare_InvalidNotStringCompared(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"123", "", -1},
		{"", "123", 1},
		{"123", "abc", -1},
		{"999", "1000x", -1}, // 字串比較會得到 1
		{"b", "a", 0},
		{"", "", 0},
		{"-1", "5", 1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d，預期 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestValid(t *testing.T) {
	for _, s := range []string{"1", "1234567890123456789"} {
		if !Valid(s) {
			t.Errorf("%q 應合法", s)
		}
	}
	for _, s := range []string{"", "abc", "12a", "-1", " 1", "99999999999999999999"} {
		if Valid(s) {
			t.Errorf("%q 不應合法", s)
		}
	}
}

func TestFromTime(t *testing.T) {
	at := time.UnixMilli(1420070400000 + 1000).UTC()
	got := FromTime(at)
	if got != strconv.FormatUint(1000<<22, 10) {
		t.Errorf("FromTime = %s", got)
	}
	ts, err := discordgo.SnowflakeTimestamp(got)
	if err != nil || !ts.Equal(at) {
		t.Errorf("轉回時間不一致：%v %v", ts, err)
	}
	// 比之後的時間產生的 snowflake 早
	if !Less(got, FromTime(at.Add(time.Millisecond))) {
		t.Error("較早的時間應產生較小的 snowflake")
	}
	if got := FromTime(time.UnixMilli(0)); got != "0" {
		t.Errorf("Discord epoch 之前應為 0：%s", got)
	}
}
