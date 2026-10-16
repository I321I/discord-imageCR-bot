package commands

import (
	"context"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"discordbot/internal/imagemsg"
	"discordbot/internal/index"

	"github.com/bwmarrin/discordgo"
)

// 偽造 value：一律 ephemeral「找不到這張圖片」或「找不到符合的圖片」，不讀訊息、不下載。
func TestGK9_ForgedValuesRejected(t *testing.T) {
	cases := map[string]func(other, own string) string{
		"別人頻道":     func(other, own string) string { return srHidden + ":" + other + ":0" },
		"別人頻道負序號":  func(other, own string) string { return srHidden + ":" + other + ":-1" },
		"序號越界大數":   func(other, own string) string { return srChannel + ":" + own + ":99999999999999999999" },
		"序號越界":     func(other, own string) string { return srChannel + ":" + own + ":5" },
		"負序號":      func(other, own string) string { return srChannel + ":" + own + ":-1" },
		"頻道前導零":    func(other, own string) string { return "0" + srChannel + ":" + own + ":0" },
		"訊息前導零":    func(other, own string) string { return srChannel + ":0" + own + ":0" },
		"多一段":      func(other, own string) string { return srChannel + ":" + own + ":0:0" },
		"超長":       func(other, own string) string { return srChannel + ":" + own + ":0" + strings.Repeat("0", 200) },
		"怪字元":      func(other, own string) string { return srChannel + ":" + own + ":0\x00" },
		"全形數字":     func(other, own string) string { return "１００:" + own + ":0" },
		"頻道錯配訊息":   func(other, own string) string { return srHidden + ":" + own + ":0" },
		"訊息屬於別人頻道": func(other, own string) string { return srChannel + ":" + other + ":0" },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			e := newSREnv(t)
			h := e.image(srHidden, imagemsg.Fields{Work: "芙莉蓮"}, "x")
			o := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "x")
			v := mk(h.ID, o.ID)
			e.s.Submit(context.Background(), searchSubmit(srUser, v))
			if len(e.fake.Responses) != 1 {
				t.Fatalf("應有 1 個回應：%+v", e.fake.Responses)
			}
			r := e.fake.Responses[0].Response
			if r.Type == discordgo.InteractionResponseDeferredChannelMessageWithSource {
				t.Fatalf("偽造值 %q 不應 deferred 貼圖", v)
			}
			if r.Data == nil || r.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
				t.Fatalf("應為 ephemeral：%+v", r)
			}
			if n := e.fake.CallCount("ChannelMessage"); n != 0 || len(e.fetch.fetched()) != 0 {
				t.Errorf("不應讀訊息或下載：%d %v", n, e.fetch.fetched())
			}
		})
	}
}

// 其他使用者勾選的頻道：A 可用、B 不可用的頻道，B 偽造 A 的 key → 拒絕。
func TestGK9_OtherUsersChannelRejected(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "x")
	e.s.Submit(context.Background(), searchSubmit("u2", srChannel+":"+m.ID+":0"))
	immediate(t, e.fake, msgSearchImageGone)
	if e.fake.CallCount("ChannelMessage") != 0 {
		t.Error("不應讀訊息")
	}
	// 自己打的字也只在自己的 Selected 內搜。
	e.fake.Responses = nil
	e.s.Submit(context.Background(), searchSubmit("u2", "芙莉蓮"))
	immediate(t, e.fake, msgSearchNoMatch)
}

// 自己打的字符合 Key 格式（PM 決定：當 Key）：Key 不存在時不改用文字搜尋。
func TestGK9_KeyShapedTextIsKey(t *testing.T) {
	e := newSREnv(t)
	e.legacy(srChannel, "100:1:0 是說明文字", "A")
	e.s.Submit(context.Background(), searchSubmit(srUser, "100:1:0"))
	immediate(t, e.fake, msgSearchImageGone)
}

// 使用者在 Selected 範圍內但頻道屬於另一個伺服器（索引的 GuildID 與 Selected 不一致）。
func TestGK9_EmptySelected(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "x")
	e.access.refs[srUser] = nil
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	immediate(t, e.fake, msgSearchImageGone)
	e.fake.Responses = nil
	e.s.Autocomplete(context.Background(), autocomplete(srUser, ""))
	if got := choices(t, e.fake); len(got) != 0 {
		t.Errorf("沒有頻道時不應有提示：%+v", got)
	}
}

// 參數值型別被偽造成非字串（數字）時：視為空字串。
// （階段 9 收尾：原本記錄「貼出 Selected 內最新一張」；依使用者決定改為斷言
// ephemeral「請輸入關鍵字，或從提示中選擇一張圖片」、不貼圖。）
func TestGK9_NonStringValueBehaviour(t *testing.T) {
	e := newSREnv(t)
	e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "NEWEST")
	i := searchSubmit(srUser, "")
	data := i.Data.(discordgo.ApplicationCommandInteractionData)
	data.Options[0].Value = float64(1)
	i.Data = data
	e.s.Submit(context.Background(), i)
	immediate(t, e.fake, msgSearchEmptyQuery)
	if len(e.fetch.fetched()) > 0 || e.fake.CallCount("ChannelMessage") != 0 {
		t.Error("非字串值不應讀訊息或下載")
	}
}

// 只有空白的輸入（Discord 可能送出）。
// （階段 9 收尾：原本只記錄行為（貼出最新的圖）；依使用者決定改為斷言 ephemeral「請輸入關鍵字…」、不貼圖。）
func TestGK9_WhitespaceOnlyFreeText(t *testing.T) {
	e := newSREnv(t)
	e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "NEWEST")
	e.s.Submit(context.Background(), searchSubmit(srUser, "   "))
	immediate(t, e.fake, msgSearchEmptyQuery)
	if len(e.fetch.fetched()) > 0 {
		t.Error("空白輸入不應下載")
	}
}

// 附件序號在索引中存在、但重新抓到的訊息已少了該附件。
// （階段 9 收尾：原本只 t.Log 建議；依 PM 決定 1 改為斷言只移除該筆附件，同訊息其他圖保留。）
func TestGK9_AttachmentOutOfRangeRemovedFromIndex(t *testing.T) {
	e := newSREnv(t)
	m := e.legacy(srChannel, "舊的", "A", "B")
	m.Attachments = m.Attachments[:1]
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":1"))
	failedAfterDefer(t, e.fake, msgSearchImageGone)
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Attachment: 1}); ok {
		t.Error("附件序號越界時應從索引移除該筆")
	}
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Attachment: 0}); !ok {
		t.Error("同訊息的其他附件應保留")
	}
}

// 重新抓到的附件已不是圖片（索引過時）。
// （階段 9 收尾：原本只記錄「仍會重新上傳」；依 PM 決定 2 改為斷言「找不到」、不上傳、移除該筆。）
func TestGK9_AttachmentNoLongerImage(t *testing.T) {
	e := newSREnv(t)
	m := e.legacy(srChannel, "舊的", "A")
	m.Attachments[0].ContentType = "application/zip"
	m.Attachments[0].Filename = "a.zip"
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	failedAfterDefer(t, e.fake, msgSearchImageGone)
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID}); ok {
		t.Error("已不是圖片的附件應從索引移除")
	}
}

// ChoiceName 對任何輸入：1..100 rune、UTF-8 合法、非空。
func TestGK9_ChoiceNameBounds(t *testing.T) {
	long := strings.Repeat("😀", 300)
	cases := []index.Entry{
		{Kind: index.KindImageMessage, AttachmentCount: 1},
		{Kind: index.KindImageMessage, AttachmentCount: 10, Key: index.Key{Attachment: 9}, Fields: imagemsg.Fields{Work: long, Text: long}},
		{Kind: index.KindImageMessage, AttachmentCount: 1, Fields: imagemsg.Fields{Work: imagemsg.FailedMarker, Text: imagemsg.FailedMarker}},
		{Kind: index.KindImageMessage, AttachmentCount: 1, Fields: imagemsg.Fields{Work: " \n\t "}},
		{Kind: index.KindLegacy, AttachmentCount: 2, Key: index.Key{Attachment: 1}, LegacyText: strings.Repeat("a\n", 500)},
		{Kind: index.KindLegacy, AttachmentCount: 1, LegacyText: ""},
		{Kind: index.KindImageMessage, AttachmentCount: 1 << 20, Key: index.Key{Attachment: 1<<20 - 1}, Fields: imagemsg.Fields{Work: long}},
	}
	for n, c := range cases {
		got := ChoiceName(c)
		r := utf8.RuneCountInString(got)
		if r < 1 || r > 100 || !utf8.ValidString(got) {
			t.Errorf("#%d：長度 %d、合法 %v：%q", n, r, utf8.ValidString(got), got)
		}
		// UTF-16 code unit 數（若 Discord 以 UTF-16 計算時的風險）。
		u16 := 0
		for _, ch := range got {
			if ch > 0xFFFF {
				u16 += 2
			} else {
				u16++
			}
		}
		// （階段 9 收尾：原本只 t.Logf；依 PM 決定 6 改為斷言 UTF-16 長度 ≤ 100。）
		if u16 > 100 {
			t.Errorf("#%d：UTF-16 長度 %d（> 100）", n, u16)
		}
	}
	// Value（Key 字串）上限 100。
	k := index.Key{ChannelID: "18446744073709551615", MessageID: "18446744073709551615", Attachment: 1 << 30}
	if len(k.String()) > 100 {
		t.Errorf("Key 字串過長：%d", len(k.String()))
	}
}

// 同時多個 autocomplete 與送出、索引寫入（-race）。
func TestGK9_ConcurrentAutocompleteAndSubmit(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "PNG")
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(3)
		go func() { defer wg.Done(); e.s.Autocomplete(context.Background(), autocomplete(srUser, "芙")) }()
		go func() {
			defer wg.Done()
			e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
		}()
		go func() {
			defer wg.Done()
			e.x.PutImageMessage(srGuild, m)
		}()
	}
	wg.Wait()
	if got := len(e.fake.ResponseEdits); got != 20 {
		t.Errorf("應貼出 20 次，得到 %d", got)
	}
}
