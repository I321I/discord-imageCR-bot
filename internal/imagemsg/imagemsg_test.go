package imagemsg

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFormat_SpecExample(t *testing.T) {
	got := Format(Fields{Work: "葬送的芙莉蓮", Text: "這就是人類啊"}, "123")
	want := "作品：葬送的芙莉蓮\n圖片文字：這就是人類啊\n角色：\n關鍵字：\n上傳者：<@123>"
	if got != want {
		t.Errorf("得到 %q\n預期 %q", got, want)
	}
}

func TestFormat_EmptyFieldsLast(t *testing.T) {
	got := Format(Fields{Keywords: "梗"}, "1")
	want := "關鍵字：梗\n作品：\n圖片文字：\n角色：\n上傳者：<@1>"
	if got != want {
		t.Errorf("得到 %q\n預期 %q", got, want)
	}
	got = Format(Fields{Characters: "芙莉蓮", Work: "w"}, "1")
	want = "作品：w\n角色：芙莉蓮\n圖片文字：\n關鍵字：\n上傳者：<@1>"
	if got != want {
		t.Errorf("得到 %q\n預期 %q", got, want)
	}
}

func TestFormat_MultilineText(t *testing.T) {
	got := Format(Fields{Work: "w", Text: "第一句\n第二句"}, "1")
	want := "作品：w\n圖片文字：第一句\n第二句\n角色：\n關鍵字：\n上傳者：<@1>"
	if got != want {
		t.Errorf("得到 %q\n預期 %q", got, want)
	}
}

func TestFormat_SingleLineFieldNewlinesBecomeSeparator(t *testing.T) {
	in := Fields{Work: "第一行\n第二行", Text: "多\n行", Characters: "芙莉蓮\r\n費倫\n", Keywords: "\n梗\r"}
	got, _, ok := Parse(Format(in, "1"))
	want := Fields{Work: "第一行、第二行", Text: "多\n行", Characters: "芙莉蓮、費倫", Keywords: "梗"}
	if !ok || got != want {
		t.Errorf("得到 %+v %v，預期 %+v", got, ok, want)
	}
	if got := Format(Fields{Work: "\n"}, "1"); got != Format(Fields{}, "1") {
		t.Errorf("只有換行的單行欄位應視為空：%q", got)
	}
}

func TestParse_RoundTrip(t *testing.T) {
	cases := []Fields{
		{Work: "葬送的芙莉蓮", Text: "這就是人類啊"},
		{Work: "w", Text: "第一句\n第二句\n第三句", Characters: "芙莉蓮、費倫", Keywords: "k"},
		{Work: FailedMarker, Text: FailedMarker},
		{Keywords: "只有關鍵字"},
		{},
		{Text: "只有\n文字"},
		{Text: "\n開頭是空行"},
		{Characters: "角", Text: "文字"},
	}
	for _, f := range cases {
		content := Format(f, "456")
		got, uploader, ok := Parse(content)
		if !ok {
			t.Errorf("無法解析 %q", content)
			continue
		}
		if got != f || uploader != "456" {
			t.Errorf("round trip 不符：%q\n得到 %+v %q\n預期 %+v", content, got, uploader, f)
		}
	}
}

func TestParse_TextContainingLabelLikeLine(t *testing.T) {
	cases := []Fields{
		{Text: "第一句\n角色：不是欄位"},
		{Work: "w", Text: "第一句\n作品：不是欄位"},
		{Work: "w", Text: "a\n圖片文字：不是欄位\n關鍵字：也不是"},
		{Text: "a\n作品：假\n上傳者：<@999>", Characters: "c"},
	}
	for _, f := range cases {
		content := Format(f, "1")
		got, uploader, ok := Parse(content)
		if !ok || got != f || uploader != "1" {
			t.Errorf("解析 %q\n得到 %+v %q %v\n預期 %+v", content, got, uploader, ok, f)
		}
	}
}

func TestParse_RejectsOtherMessages(t *testing.T) {
	cases := map[string]string{
		"純文字":     "hello",
		"空":       "",
		"缺上傳者":    "作品：\n圖片文字：\n角色：\n關鍵字：",
		"上傳者格式錯":  "作品：\n圖片文字：\n角色：\n關鍵字：\n上傳者：某人",
		"上傳者不在最後": "上傳者：<@1>\n作品：\n圖片文字：\n角色：\n關鍵字：",
		"缺角色":     "作品：\n圖片文字：\n關鍵字：\n上傳者：<@1>",
		"缺圖片文字":   "作品：\n角色：\n關鍵字：\n上傳者：<@1>",
		"作品重複":    "作品：a\n作品：b\n圖片文字：\n角色：\n關鍵字：\n上傳者：<@1>",
		"角色重複":    "作品：\n圖片文字：\n角色：\n角色：\n關鍵字：\n上傳者：<@1>",
		"多餘的行":    "多餘\n作品：\n圖片文字：\n角色：\n關鍵字：\n上傳者：<@1>",
		"欄位後多餘的行": "作品：\n圖片文字：\n角色：a\n多餘\n關鍵字：\n上傳者：<@1>",
	}
	for name, c := range cases {
		if _, _, ok := Parse(c); ok {
			t.Errorf("%s：不應解析成功：%q", name, c)
		}
	}
}

func TestNormalizeList(t *testing.T) {
	cases := map[string]string{
		"芙莉蓮,費倫，修塔爾克":   "芙莉蓮、費倫、修塔爾克",
		"  芙莉蓮 , 費倫  ":  "芙莉蓮、費倫",
		"a,,，、b":        "a、b",
		"a；b;c\nd\r\ne": "a、b、c、d、e",
		"":              "",
		" , ，":          "",
		"芙莉蓮、費倫、芙莉蓮":    "芙莉蓮、費倫",
		"Spy x Family":  "Spy x Family",
		"　全形空白　、x":      "全形空白、x",
	}
	for in, want := range cases {
		if got := NormalizeList(in); got != want {
			t.Errorf("NormalizeList(%q) = %q，預期 %q", in, got, want)
		}
	}
}

func TestFailed(t *testing.T) {
	f := Fields{Work: FailedMarker, Text: "ok", Keywords: FailedMarker}
	if got := f.Failed(); !slices.Equal(got, []Field{FieldWork, FieldKeywords}) {
		t.Errorf("得到 %v", got)
	}
	if got := (Fields{Work: "x"}).Failed(); len(got) != 0 {
		t.Errorf("沒有失敗欄位應回空：%v", got)
	}
	if got := (Fields{Text: FailedMarker + " "}).Failed(); len(got) != 0 {
		t.Errorf("只有完全等於標記才算失敗：%v", got)
	}
}

func TestGetSetLabel(t *testing.T) {
	var f Fields
	labels := map[Field]string{FieldWork: "作品", FieldText: "圖片文字", FieldCharacters: "角色", FieldKeywords: "關鍵字"}
	for field, label := range labels {
		f.Set(field, label+"值")
		if f.Get(field) != label+"值" {
			t.Errorf("Get/Set %v 不一致", field)
		}
		if Label(field) != label {
			t.Errorf("Label(%v) = %q，預期 %q", field, Label(field), label)
		}
	}
	if f != (Fields{Work: "作品值", Text: "圖片文字值", Characters: "角色值", Keywords: "關鍵字值"}) {
		t.Errorf("Set 寫到錯的欄位：%+v", f)
	}
}

func TestFormat_Max2000(t *testing.T) {
	long := strings.Repeat("字", 3000)

	// 只有圖片文字過長：截圖片文字，結尾加「…」，其他欄位不動。
	out := Format(Fields{Work: "w", Text: long, Keywords: "k"}, "123")
	if n := utf8.RuneCountInString(out); n != 2000 {
		t.Errorf("長度應為 2000，得到 %d", n)
	}
	f, _, ok := Parse(out)
	if !ok || !strings.HasSuffix(f.Text, "…") || f.Work != "w" || f.Keywords != "k" {
		t.Errorf("截斷結果不符：%+v %v", f, ok)
	}

	// 關鍵字最長：只截關鍵字，較短的圖片文字不動。
	out = Format(Fields{Work: "w", Text: "短字幕", Keywords: long}, "123")
	if n := utf8.RuneCountInString(out); n != 2000 {
		t.Errorf("長度應為 2000，得到 %d", n)
	}
	f, _, ok = Parse(out)
	if !ok || f.Text != "短字幕" || !strings.HasSuffix(f.Keywords, "…") || f.Work != "w" {
		t.Errorf("截斷結果不符：%+v %v", f, ok)
	}

	// 從最長的欄位開始截：作品最長時先截作品，截到只剩「…」仍超過才截次長的角色；圖片文字較短不動。
	out = Format(Fields{Work: strings.Repeat("作", 2500), Text: strings.Repeat("字", 100), Characters: strings.Repeat("角", 1990)}, "123")
	if n := utf8.RuneCountInString(out); n != 2000 {
		t.Errorf("長度應為 2000，得到 %d", n)
	}
	f, _, ok = Parse(out)
	if !ok || f.Work != "…" || !strings.HasSuffix(f.Characters, "…") || f.Text != strings.Repeat("字", 100) {
		t.Errorf("截斷結果不符：work=%q text 長 %d chars 長 %d ok=%v", f.Work, utf8.RuneCountInString(f.Text), utf8.RuneCountInString(f.Characters), ok)
	}

	// 只需截一個欄位時，只截最長的那個。
	out = Format(Fields{Work: strings.Repeat("作", 1500), Text: strings.Repeat("字", 1000)}, "123")
	f, _, ok = Parse(out)
	if !ok || f.Text != strings.Repeat("字", 1000) || !strings.HasSuffix(f.Work, "…") {
		t.Errorf("應只截最長的作品：work 長 %d text 長 %d ok=%v", utf8.RuneCountInString(f.Work), utf8.RuneCountInString(f.Text), ok)
	}

	// 不超過時不截斷。
	exact := Fields{Text: strings.Repeat("字", 1000)}
	if out := Format(exact, "1"); strings.Contains(out, "…") {
		t.Error("未超過 2000 字不應截斷")
	}

	// 任何組合都不超過 2000 字。
	out = Format(Fields{Work: long, Text: long, Characters: long, Keywords: long}, "123")
	if n := utf8.RuneCountInString(out); n > 2000 {
		t.Errorf("長度 %d 超過 2000", n)
	}
	if _, _, ok := Parse(out); !ok {
		t.Error("截斷後仍應可解析")
	}
}
