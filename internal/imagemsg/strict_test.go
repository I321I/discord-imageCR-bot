package imagemsg

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FormatStrict 不截斷：不超過 2000 字時與 Format 相同；超過時 ok=false（編輯表單送出太長要回錯誤、不修改）。
func TestFormatStrict(t *testing.T) {
	f := Fields{Work: "芙莉蓮", Text: "第一句\n第二句"}
	got, ok := FormatStrict(f, "123")
	if !ok || got != Format(f, "123") {
		t.Errorf("未超過上限應與 Format 相同：%q（%v）", got, ok)
	}

	base := utf8.RuneCountInString(Format(Fields{}, "123"))
	exact := Fields{Text: strings.Repeat("字", MaxLen-base)}
	if s, ok := FormatStrict(exact, "123"); !ok || utf8.RuneCountInString(s) != MaxLen {
		t.Errorf("剛好 2000 字應可：%d（%v）", utf8.RuneCountInString(s), ok)
	}
	over := Fields{Text: strings.Repeat("字", MaxLen-base+1)}
	if _, ok := FormatStrict(over, "123"); ok {
		t.Error("超過 2000 字應回 ok=false")
	}
}
