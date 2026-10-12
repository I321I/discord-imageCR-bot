package imagemsg

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 把關者第二輪：截斷從最長欄位開始、只截需要的量、其他欄位不動；結果仍可解析。
func TestGate2_TruncateLongestFirstOnly(t *testing.T) {
	f := Fields{Work: strings.Repeat("作", 300), Text: strings.Repeat("字\n", 1000), Characters: "角", Keywords: strings.Repeat("關", 600)}
	out := Format(f, "123")
	if n := utf8.RuneCountInString(out); n != MaxLen {
		t.Errorf("長度 %d，預期剛好 %d（只截需要的量）", n, MaxLen)
	}
	g, _, ok := Parse(out)
	if !ok {
		t.Fatal("截斷後無法解析")
	}
	if g.Work != f.Work || g.Keywords != f.Keywords || g.Characters != f.Characters {
		t.Error("只需截最長的圖片文字，其他欄位不應被動")
	}
	if !strings.HasSuffix(g.Text, "…") {
		t.Error("被截欄位結尾應有「…」")
	}
}

// 兩個最長欄位等長且需要截兩個欄位時，結果仍 ≤ 2000 且可解析。
func TestGate2_TruncateMultipleFields(t *testing.T) {
	f := Fields{Work: strings.Repeat("a", 1500), Text: strings.Repeat("b", 1500), Characters: strings.Repeat("c", 1500), Keywords: strings.Repeat("d", 1500)}
	out := Format(f, "123")
	if n := utf8.RuneCountInString(out); n > MaxLen {
		t.Fatalf("長度 %d", n)
	}
	g, _, ok := Parse(out)
	if !ok {
		t.Fatal("無法解析")
	}
	t.Logf("作品 %d、圖片文字 %d、角色 %d、關鍵字 %d", utf8.RuneCountInString(g.Work), utf8.RuneCountInString(g.Text), utf8.RuneCountInString(g.Characters), utf8.RuneCountInString(g.Keywords))
}

// 單行欄位的 \r\n、只有換行、空白行的處理（實作者選擇不修剪、不去重）。
func TestGate2_SingleLineJoinEdgeCases(t *testing.T) {
	cases := map[string]string{
		"a\r\nb":  "a、b",
		"\n\n":    "",
		"\na\n":   "a",
		"a\n \nb": "a、 、b", // 不修剪：空白行保留成「 」
		"a\na":    "a、a",   // 不去重
	}
	for in, want := range cases {
		out := Format(Fields{Work: in, Text: "t"}, "1")
		g, _, ok := Parse(out)
		if !ok {
			t.Errorf("%q：無法解析：%q", in, out)
			continue
		}
		if g.Work != want {
			t.Errorf("%q → %q，預期 %q", in, g.Work, want)
		}
	}
}
