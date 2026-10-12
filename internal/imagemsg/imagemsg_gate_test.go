package imagemsg

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

// 把關者補充：隨機欄位（單行欄位不含換行）Format 後必須能 Parse 回原值。
func TestGate_FormatParseRoundTrip_Random(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	pieces := []string{"", "作品：", "圖片文字：", "角色：", "關鍵字：", "上傳者：<@9>", "芙莉蓮", " ", "、", FailedMarker, "<@1>", "：", "a"}
	gen := func(multiline bool) string {
		if r.Intn(4) == 0 {
			return ""
		}
		var sb strings.Builder
		n := 1 + r.Intn(5)
		for i := 0; i < n; i++ {
			if multiline && r.Intn(3) == 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(pieces[r.Intn(len(pieces))])
		}
		return sb.String()
	}
	for i := 0; i < 20000; i++ {
		in := Fields{Work: gen(false), Text: gen(true), Characters: gen(false), Keywords: gen(false)}
		content := Format(in, "42")
		got, uid, ok := Parse(content)
		if !ok || uid != "42" || got != in {
			t.Fatalf("round-trip 失敗\n輸入 %#v\n內容 %q\n得到 %#v uid=%q ok=%v", in, content, got, uid, ok)
		}
	}
}

// 把關者補充：長內容截斷後仍 ≤ 2000 字且可被 Parse。
func TestGate_FormatTruncatedStillParses(t *testing.T) {
	long := strings.Repeat("字幕\n", 1500)
	for _, in := range []Fields{
		{Work: "w", Text: long},
		{Work: "w", Text: long, Keywords: strings.Repeat("梗、", 800)},
		{Work: strings.Repeat("作", 3000), Characters: strings.Repeat("角", 3000)},
	} {
		c := Format(in, "1")
		if n := utf8.RuneCountInString(c); n > MaxLen {
			t.Errorf("長度 %d > %d", n, MaxLen)
		}
		if _, _, ok := Parse(c); !ok {
			t.Errorf("截斷後無法 Parse：%q", c[:80])
		}
	}
}

// 把關者補充：單行欄位若含換行，Format 會把換行轉成「、」，產生的內容可被 Parse。
func TestGate_SingleLineFieldWithNewline(t *testing.T) {
	in := Fields{Work: "第一行\n第二行", Text: "t"}
	got, _, ok := Parse(Format(in, "1"))
	if !ok || got.Work != "第一行、第二行" || got.Text != "t" {
		t.Errorf("單行欄位含換行時應轉成「、」並可解析：%+v %v", got, ok)
	}
}
