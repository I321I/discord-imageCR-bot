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

// 四個欄位等長且都要截時：平均截成等長，結果仍 ≤ 2000 且可解析。
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
	w, x, c, k := utf8.RuneCountInString(g.Work), utf8.RuneCountInString(g.Text), utf8.RuneCountInString(g.Characters), utf8.RuneCountInString(g.Keywords)
	if w != x || x != c || c != k {
		t.Errorf("四欄應截成等長：作品 %d、圖片文字 %d、角色 %d、關鍵字 %d", w, x, c, k)
	}
	if n := utf8.RuneCountInString(out); n < MaxLen-3 {
		t.Errorf("長度 %d，平均截後不應比上限少超過 3 字", n)
	}
}

// 單行欄位的 \r\n、只有換行、空白行的處理：與 NormalizeList 一致（修剪、去除空段與重複）。
func TestGate2_SingleLineJoinEdgeCases(t *testing.T) {
	cases := map[string]string{
		"a\r\nb":   "a、b",
		"\n\n":     "",
		"\na\n":    "a",
		"a\n \nb":  "a、b", // 空白行去除
		"a\na":     "a",   // 去重
		" a \n b ": "a、b", // 修剪
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
