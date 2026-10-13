package imagemsg

import (
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

func rc(s string) int { return utf8.RuneCountInString(s) }

// 總長剛好 2000 不截；多 1 字時只截最長欄位 1 字（含「…」），總長剛好 2000。
func TestGate3_TruncateBoundary(t *testing.T) {
	base := Format(Fields{Work: "w", Text: "t"}, "123")
	pad := MaxLen - rc(base) + 1 // Text 長度讓總長剛好 2000
	f := Fields{Work: "w", Text: strings.Repeat("字", pad)}
	out := Format(f, "123")
	if rc(out) != MaxLen || strings.Contains(out, "…") {
		t.Fatalf("剛好 2000 不應截：長 %d", rc(out))
	}
	f.Text += "字"
	out = Format(f, "123")
	if rc(out) != MaxLen {
		t.Fatalf("差 1 字時截完應剛好 2000，得到 %d", rc(out))
	}
	g, _, ok := Parse(out)
	if !ok || g.Work != "w" || !strings.HasSuffix(g.Text, "…") || rc(g.Text) != pad {
		t.Errorf("只應截圖片文字 1 字：%+v ok=%v", g, ok)
	}
}

// 多位元組（CJK、emoji、組合字）以 rune 計數，結果不超過 2000 且是合法 UTF-8。
func TestGate3_TruncateMultibyte(t *testing.T) {
	f := Fields{Work: strings.Repeat("😀", 1500), Text: strings.Repeat("é", 1500), Characters: strings.Repeat("角", 800)}
	out := Format(f, "123456789012345678")
	if !utf8.ValidString(out) || rc(out) > MaxLen {
		t.Fatalf("長 %d valid=%v", rc(out), utf8.ValidString(out))
	}
	if _, _, ok := Parse(out); !ok {
		t.Error("無法解析")
	}
}

// 隨機輸入：結果 ≤ 2000、可解析、未截的欄位不變、被截的欄位等長、不會截過頭超過欄位數（餘數）。
func TestGate3_TruncateRandom(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	alphabet := []rune("ab字😀 ")
	gen := func() string {
		n := r.Intn(3000)
		if r.Intn(4) == 0 {
			n = 0
		}
		b := make([]rune, n)
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		return strings.TrimSpace(string(b))
	}
	for i := 0; i < 500; i++ {
		f := Fields{Work: gen(), Text: gen(), Characters: gen(), Keywords: gen()}
		if f.Text == "" {
			f.Text = "x"
		}
		out := Format(f, "123")
		if rc(out) > MaxLen {
			t.Fatalf("case %d 長 %d", i, rc(out))
		}
		g, _, ok := Parse(out)
		if !ok {
			t.Fatalf("case %d 無法解析", i)
		}
		cut := -1
		truncated := 0
		for _, fl := range allFields {
			if g.Get(fl) != f.Get(fl) {
				truncated++
				if cut >= 0 && rc(g.Get(fl)) != cut {
					t.Fatalf("case %d 被截欄位不等長", i)
				}
				cut = rc(g.Get(fl))
			}
		}
		if truncated > 0 && cut > 1 && MaxLen-rc(out) >= truncated {
			t.Fatalf("case %d 截過頭：長 %d、截了 %d 欄", i, rc(out), truncated)
		}
	}
}
