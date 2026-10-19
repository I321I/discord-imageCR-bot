package index

import (
	"errors"
	"slices"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	example1    = "mygo、我早就知道會這樣了\nhttps://i.imgur.com/jgoYzsS.jpg"
	example1URL = "https://i.imgur.com/jgoYzsS.jpg"
)

func linkMsg(ch, id, content string) *discordgo.Message {
	return &discordgo.Message{ID: id, ChannelID: ch, Content: content, Author: &discordgo.User{ID: "u1"}}
}

func TestParseKey_LinkAndLegacyFormats(t *testing.T) {
	got, err := ParseKey("1:2:0")
	if err != nil || got != (Key{ChannelID: "1", MessageID: "2", Attachment: 0}) {
		t.Errorf("舊格式：%+v %v", got, err)
	}
	got, err = ParseKey("1:2:u3")
	if err != nil || got != (Key{ChannelID: "1", MessageID: "2", Attachment: 3, Link: true}) {
		t.Errorf("網址格式：%+v %v", got, err)
	}
	if s := (Key{ChannelID: "1", MessageID: "2", Attachment: 3, Link: true}).String(); s != "1:2:u3" {
		t.Errorf("String = %q", s)
	}
	if s := (Key{ChannelID: "1", MessageID: "2", Attachment: 3}).String(); s != "1:2:3" {
		t.Errorf("附件 String = %q", s)
	}
	for _, bad := range []string{"1:2:u", "1:2:u-1", "1:2:x1", "1:2:uu1", "1:2:U1", "1:2:u+1"} {
		if _, err := ParseKey(bad); !errors.Is(err, ErrBadKey) {
			t.Errorf("ParseKey(%q) 應回 ErrBadKey：%v", bad, err)
		}
	}
}

func TestPutLegacy_LinkOnly(t *testing.T) {
	x := New()
	x.PutLegacy("g", linkMsg("c", "10", example1))
	es := x.Search("", allow("c"), 0)
	if len(es) != 1 {
		t.Fatalf("應收錄 1 筆：%+v", es)
	}
	e := es[0]
	if !e.Key.Link || e.Key.Attachment != 0 || e.ImageURL != example1URL || e.LegacyText != "mygo、我早就知道會這樣了" ||
		e.AttachmentCount != 1 || e.Number != 1 || e.AttachmentID != "" || e.Kind != KindLegacy {
		t.Errorf("項目不正確：%+v", e)
	}
	if got, ok := x.Get(e.Key); !ok || got.ImageURL != example1URL {
		t.Errorf("Get 應找到：%+v %v", got, ok)
	}
}

func TestPutLegacy_LinkOnlyNoText(t *testing.T) {
	x := New()
	x.PutLegacy("g", linkMsg("c", "10", example1URL))
	x.PutLegacy("g", linkMsg("c", "11", "只有一般連結 https://web.com/page"))
	if es := x.Search("", allow("c"), 0); len(es) != 0 {
		t.Errorf("只有網址、或沒有圖片的訊息不應收錄：%+v", es)
	}
}

// U8：附件在前、網址在後；張數與編號計附件與網址。
func TestPutLegacy_MixedNumbering(t *testing.T) {
	x := New()
	m := legacyMsg("c", "10", "文字 https://a.com/1.png 網頁 https://web.com/ 和 https://a.com/2.jpg", 1)
	x.PutLegacy("g", m)
	es := x.Search("", allow("c"), 0)
	if len(es) != 3 {
		t.Fatalf("應有 3 筆：%+v", es)
	}
	want := []Key{
		{ChannelID: "c", MessageID: "10", Attachment: 0},
		{ChannelID: "c", MessageID: "10", Attachment: 0, Link: true},
		{ChannelID: "c", MessageID: "10", Attachment: 2, Link: true},
	}
	for i, e := range es {
		if e.Key != want[i] || e.Number != i+1 || e.AttachmentCount != 3 {
			t.Errorf("第 %d 筆：%+v，預期 Key %+v Number %d", i, e, want[i], i+1)
		}
	}
	if es[1].ImageURL != "https://a.com/1.png" || es[2].ImageURL != "https://a.com/2.jpg" || es[0].ImageURL != "" {
		t.Errorf("ImageURL 不正確：%q %q %q", es[0].ImageURL, es[1].ImageURL, es[2].ImageURL)
	}
	if es[0].LegacyText != "文字 網頁 https://web.com/ 和" {
		t.Errorf("文字應去掉圖片網址、保留一般連結：%q", es[0].LegacyText)
	}
}

func TestSearch_DoesNotMatchURL(t *testing.T) {
	x := New()
	x.PutLegacy("g", linkMsg("c", "10", example1))
	for _, q := range []string{"imgur", "jgoYzsS", "https"} {
		if es := x.Search(q, allow("c"), 0); len(es) != 0 {
			t.Errorf("搜尋 %q 不應比對到網址：%+v", q, es)
		}
	}
	if es := x.Search("mygo", allow("c"), 0); len(es) != 1 {
		t.Errorf("搜尋 mygo 應找到：%+v", es)
	}
}

// 同一訊息多張依 Number 排（附件與網址的序號可能相同）。
func TestSearch_SameMessageOrderByNumber(t *testing.T) {
	x := New()
	m := legacyMsg("c", "10", "a https://a.com/1.png https://a.com/2.png", 2)
	x.PutLegacy("g", m)
	var nums []int
	for _, e := range x.Search("", allow("c"), 0) {
		nums = append(nums, e.Number)
	}
	if !slices.Equal(nums, []int{1, 2, 3, 4}) {
		t.Errorf("應依 Number 排序：%v", nums)
	}
	nums = nil
	for _, e := range x.Search("", allow("c"), 3) {
		nums = append(nums, e.Number)
	}
	if !slices.Equal(nums, []int{1, 2, 3}) {
		t.Errorf("限制筆數時也應依 Number 排序：%v", nums)
	}
}

// 移除一張網址圖不影響同序號的附件圖。
func TestRemoveAttachment_LinkKeyDistinct(t *testing.T) {
	x := New()
	x.PutLegacy("g", legacyMsg("c", "10", "a https://a.com/1.png", 1))
	x.RemoveAttachment(Key{ChannelID: "c", MessageID: "10", Attachment: 0, Link: true})
	es := x.Search("", allow("c"), 0)
	if len(es) != 1 || es[0].Key.Link {
		t.Errorf("應只剩附件圖：%+v", es)
	}
}
