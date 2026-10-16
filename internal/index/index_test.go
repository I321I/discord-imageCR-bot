package index

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"discordbot/internal/imagemsg"

	"github.com/bwmarrin/discordgo"
)

func imageMsg(ch, id string, f imagemsg.Fields, uploader string) *discordgo.Message {
	return &discordgo.Message{
		ID: id, ChannelID: ch, Content: imagemsg.Format(f, uploader),
		Author:      &discordgo.User{ID: "bot", Bot: true},
		Attachments: []*discordgo.MessageAttachment{{Filename: "a.png", ContentType: "image/png"}},
	}
}

func legacyMsg(ch, id, content string, n int) *discordgo.Message {
	m := &discordgo.Message{ID: id, ChannelID: ch, Content: content, Author: &discordgo.User{ID: "u1"}}
	for i := 0; i < n; i++ {
		m.Attachments = append(m.Attachments, &discordgo.MessageAttachment{Filename: fmt.Sprintf("%d.png", i), ContentType: "image/png"})
	}
	return m
}

func allow(ids ...string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func keys(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Key.String())
	}
	return out
}

func TestKey_RoundTrip(t *testing.T) {
	k := Key{ChannelID: "123", MessageID: "456", Attachment: 2}
	if k.String() != "123:456:2" {
		t.Errorf("String %q", k.String())
	}
	got, err := ParseKey("123:456:2")
	if err != nil || got != k {
		t.Errorf("ParseKey 得到 %+v %v", got, err)
	}
	for _, bad := range []string{"", "1:2", "1:2:x", "1:2:-1", ":2:0", "1::0", "1:2:3:4", strings.Repeat("1", 99) + ":2:0", "隨便打的字"} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) 應回錯誤", bad)
		}
	}
}

func TestPut_ParsesImageMessage(t *testing.T) {
	x := New()
	f := imagemsg.Fields{Work: "葬送のフリーレン", Text: "這就是人類啊", Characters: "芙莉蓮"}
	x.PutImageMessage("g1", imageMsg("c1", "10", f, "u9"))
	e, ok := x.Get(Key{ChannelID: "c1", MessageID: "10"})
	if !ok || e.Kind != KindImageMessage || e.Fields != f || e.GuildID != "g1" || e.AttachmentCount != 1 {
		t.Errorf("得到 %+v %v", e, ok)
	}
	// 解析失敗的 bot 訊息忽略。
	x.PutImageMessage("g1", &discordgo.Message{ID: "11", ChannelID: "c1", Content: "不是圖片訊息"})
	if _, ok := x.Get(Key{ChannelID: "c1", MessageID: "11"}); ok {
		t.Error("解析失敗的訊息不應收錄")
	}
}

func TestPut_ReplacesSameMessage(t *testing.T) {
	x := New()
	x.PutImageMessage("g1", imageMsg("c1", "10", imagemsg.Fields{Work: "舊"}, "u"))
	x.PutImageMessage("g1", imageMsg("c1", "10", imagemsg.Fields{Work: "新"}, "u"))
	if got := x.Search("舊", allow("c1"), 25); len(got) != 0 {
		t.Errorf("編輯後舊內容不應搜到：%v", keys(got))
	}
	if got := x.Search("新", allow("c1"), 25); len(got) != 1 {
		t.Errorf("應搜到新內容一筆：%v", keys(got))
	}
}

func TestPutLegacy_OneEntryPerImage(t *testing.T) {
	x := New()
	x.PutLegacy("g1", legacyMsg("c1", "5", "芙莉蓮 好可愛", 3))
	for i := 0; i < 3; i++ {
		e, ok := x.Get(Key{ChannelID: "c1", MessageID: "5", Attachment: i})
		if !ok || e.Kind != KindLegacy || e.LegacyText != "芙莉蓮 好可愛" || e.AttachmentCount != 3 {
			t.Errorf("第 %d 張：%+v %v", i, e, ok)
		}
	}
	if got := x.Search("芙莉蓮", allow("c1"), 25); len(got) != 3 {
		t.Errorf("應有 3 筆：%v", keys(got))
	}
}

func TestPutLegacy_NoTextSkipped(t *testing.T) {
	x := New()
	x.PutLegacy("g1", legacyMsg("c1", "5", "  \n ", 2))
	if got := x.Search("", allow("c1"), 25); len(got) != 0 {
		t.Errorf("無文字的舊訊息不應收錄：%v", keys(got))
	}
}

func TestSearch_MatchesFourFieldsNotUploader(t *testing.T) {
	x := New()
	x.PutImageMessage("g", imageMsg("c", "1", imagemsg.Fields{Work: "作品甲"}, "777"))
	x.PutImageMessage("g", imageMsg("c", "2", imagemsg.Fields{Text: "字幕乙"}, "777"))
	x.PutImageMessage("g", imageMsg("c", "3", imagemsg.Fields{Characters: "角色丙"}, "777"))
	x.PutImageMessage("g", imageMsg("c", "4", imagemsg.Fields{Keywords: "關鍵字丁"}, "777"))
	for q, want := range map[string]string{"作品甲": "c:1:0", "字幕乙": "c:2:0", "角色丙": "c:3:0", "關鍵字丁": "c:4:0"} {
		if got := keys(x.Search(q, allow("c"), 25)); len(got) != 1 || got[0] != want {
			t.Errorf("%s：得到 %v", q, got)
		}
	}
	for _, q := range []string{"777", "上傳者", "<@777>"} {
		if got := x.Search(q, allow("c"), 25); len(got) != 0 {
			t.Errorf("以 %q 不應搜到：%v", q, keys(got))
		}
	}
}

func TestSearch_RespectsAllowed(t *testing.T) {
	x := New()
	x.PutImageMessage("g", imageMsg("c1", "1", imagemsg.Fields{Work: "甲"}, "u"))
	x.PutImageMessage("g", imageMsg("c2", "2", imagemsg.Fields{Work: "甲"}, "u"))
	if got := keys(x.Search("甲", allow("c2"), 25)); len(got) != 1 || got[0] != "c2:2:0" {
		t.Errorf("只應有 c2：%v", got)
	}
	if got := x.Search("甲", nil, 25); len(got) != 0 {
		t.Errorf("allowed 為空時不應有結果：%v", keys(got))
	}
}

func TestSearch_Limit25(t *testing.T) {
	x := New()
	for i := 0; i < 40; i++ {
		x.PutImageMessage("g", imageMsg("c", fmt.Sprint(100+i), imagemsg.Fields{Work: "甲"}, "u"))
	}
	if got := x.Search("甲", allow("c"), 25); len(got) != 25 {
		t.Errorf("應最多 25 筆，得到 %d", len(got))
	}
}

// Q36：不分大小寫、全形半形視為相同、多詞全部符合。
func TestSearch_CaseInsensitive(t *testing.T) {
	x := New()
	x.PutImageMessage("g", imageMsg("c", "1", imagemsg.Fields{Work: "Frieren", Keywords: "ＡＢＣ１２３"}, "u"))
	for _, q := range []string{"frieren", "FRIEREN", "ｆｒｉｅｒｅｎ", "abc123", "ABC", "frieren　abc", "abc frieren"} {
		if got := x.Search(q, allow("c"), 25); len(got) != 1 {
			t.Errorf("%q 應搜到", q)
		}
	}
	if got := x.Search("frieren 沒有這個", allow("c"), 25); len(got) != 0 {
		t.Errorf("多詞要全部符合：%v", keys(got))
	}
}

// Q37：作品／角色／關鍵字 > 圖片文字 > 舊訊息文字；同級由新到舊（snowflake 數值）。
func TestSearch_OrderByFieldThenNewest(t *testing.T) {
	x := New()
	x.PutLegacy("g", legacyMsg("c", "1000", "芙莉蓮", 1))
	x.PutImageMessage("g", imageMsg("c", "999", imagemsg.Fields{Text: "芙莉蓮說"}, "u"))
	x.PutImageMessage("g", imageMsg("c", "9", imagemsg.Fields{Characters: "芙莉蓮"}, "u"))
	x.PutImageMessage("g", imageMsg("c", "10", imagemsg.Fields{Work: "葬送的芙莉蓮"}, "u"))
	x.PutImageMessage("g", imageMsg("c", "11", imagemsg.Fields{Keywords: "芙莉蓮"}, "u"))
	want := []string{"c:11:0", "c:10:0", "c:9:0", "c:999:0", "c:1000:0"}
	if got := keys(x.Search("芙莉蓮", allow("c"), 25)); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("得到 %v，預期 %v", got, want)
	}
}

// Q33：空查詢顯示最新的圖（不分等級，由新到舊；同訊息依附件順序）。
func TestSearch_EmptyQueryNewestFirst(t *testing.T) {
	x := New()
	x.PutLegacy("g", legacyMsg("c", "20", "文字", 2))
	x.PutImageMessage("g", imageMsg("c", "9", imagemsg.Fields{Work: "甲"}, "u"))
	x.PutImageMessage("g", imageMsg("c", "100", imagemsg.Fields{Work: "乙"}, "u"))
	want := []string{"c:100:0", "c:20:0", "c:20:1", "c:9:0"}
	if got := keys(x.Search("  ", allow("c"), 25)); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("得到 %v，預期 %v", got, want)
	}
}

// 失敗標記不參與比對（暫定）。
func TestSearch_FailedMarkerNotMatched(t *testing.T) {
	x := New()
	x.PutImageMessage("g", imageMsg("c", "1", imagemsg.Fields{Work: imagemsg.FailedMarker, Text: imagemsg.FailedMarker}, "u"))
	if got := x.Search("辨識失敗", allow("c"), 25); len(got) != 0 {
		t.Errorf("失敗標記不應被搜到：%v", keys(got))
	}
}

func TestRemoveMessage(t *testing.T) {
	x := New()
	x.PutLegacy("g", legacyMsg("c", "5", "甲", 2))
	x.PutImageMessage("g", imageMsg("c", "6", imagemsg.Fields{Work: "甲"}, "u"))
	x.RemoveMessage("c", "5")
	if got := keys(x.Search("甲", allow("c"), 25)); len(got) != 1 || got[0] != "c:6:0" {
		t.Errorf("得到 %v", got)
	}
}

func TestRemoveChannel(t *testing.T) {
	x := New()
	x.PutImageMessage("g", imageMsg("c1", "1", imagemsg.Fields{Work: "甲"}, "u"))
	x.PutImageMessage("g", imageMsg("c2", "2", imagemsg.Fields{Work: "甲"}, "u"))
	x.RemoveChannel("c1")
	if got := keys(x.Search("甲", allow("c1", "c2"), 25)); len(got) != 1 || got[0] != "c2:2:0" {
		t.Errorf("得到 %v", got)
	}
}

func TestIndex_ConcurrentPutSearch(t *testing.T) {
	x := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			x.PutImageMessage("g", imageMsg("c", fmt.Sprint(i+1), imagemsg.Fields{Work: "甲"}, "u"))
		}()
		go func() {
			defer wg.Done()
			x.Search("甲", allow("c"), 25)
		}()
		go func() {
			defer wg.Done()
			x.RemoveMessage("c", fmt.Sprint(i))
		}()
	}
	wg.Wait()
}
