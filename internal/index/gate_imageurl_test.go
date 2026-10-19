package index

import (
	"context"
	"errors"
	"testing"

	"discordbot/internal/discordtest"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// 把關重點 4：偽造的網址 Key。
func TestGate_ParseKey_ForgedLinkKeys(t *testing.T) {
	bad := []string{
		"1:2:u99999999999999999999", "1:2:u+1", "1:2:u-0", "1:2:u 1", "1:2:u1 ", "1:2:U1", "1:2:u1u",
		"1:2:u0x1", "1:2:", "1:2:3:u1", "a:2:u1", "1:b:u1", "1:2:u１",
	}
	for _, s := range bad {
		if _, err := ParseKey(s); !errors.Is(err, ErrBadKey) {
			t.Errorf("%q 應為 ErrBadKey：%v", s, err)
		}
	}
	// 網址 Key 與附件 Key 不可互相指到對方的項目。
	x := New()
	m := linkMsg("1", "2", example1)
	x.PutLegacy("g", m)
	if _, ok := x.Get(Key{ChannelID: "1", MessageID: "2", Attachment: 0}); ok {
		t.Error("附件 Key 不應取得網址圖項目")
	}
	if _, ok := x.Get(Key{ChannelID: "1", MessageID: "2", Attachment: 0, Link: true}); !ok {
		t.Error("網址 Key 應取得網址圖項目")
	}
	if _, ok := x.Get(Key{ChannelID: "1", MessageID: "2", Attachment: 5, Link: true}); ok {
		t.Error("越界的網址 Key 不應取得項目")
	}
}

// gatedAPI 讓 ChannelMessage 在取得結果後停住，模擬重讀與其他事件的競爭。
type gatedAPI struct {
	*discordtest.Fake
	fetched chan struct{}
	release chan struct{}
}

func (g *gatedAPI) ChannelMessage(channelID, messageID string, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	m, err := g.Fake.ChannelMessage(channelID, messageID, options...)
	g.fetched <- struct{}{}
	<-g.release
	return m, err
}

// 把關重點 6（記錄）：部分更新在 putMu 外重讀，重讀途中訊息被刪除（MessageDeleted 先完成）時，
// 重讀結果仍會寫回索引，留下已刪除訊息的項目。/search 選到時重讀 404 會移除（自我修復），所以只記錄結果。
func TestGate_MessageUpdated_RefetchRacesDelete(t *testing.T) {
	f := discordtest.NewFake()
	g := &gatedAPI{Fake: f, fetched: make(chan struct{}), release: make(chan struct{})}
	st := store.New(f, "storage")
	st.UseEmpty()
	x := New()
	h := NewHooks(context.Background(), g, x, &fakeBackfill{}, st)
	h.SetBotID(botID)
	addSource(t, st, "g", "c", "200")
	if _, err := st.EnsureURLSince(context.Background(), "300"); err != nil {
		t.Fatal(err)
	}
	x.PutLegacy("g", linkMsg("c", "100", example1))
	addUserLink(f, "c", "100", "新文字\n"+example1URL)

	done := make(chan struct{})
	go func() {
		h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: "新文字\n" + example1URL, Author: &discordgo.User{ID: "u1"}})
		close(done)
	}()
	<-g.fetched
	h.MessageDeleted("c", "100")
	close(g.release)
	<-done
	es := x.Search("", allow("c"), 0)
	t.Logf("重讀途中訊息被刪除後，索引剩 %d 筆（0 為無殘留）", len(es))
}
