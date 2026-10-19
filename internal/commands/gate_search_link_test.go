package commands

import (
	"context"
	"testing"

	"discordbot/internal/index"
)

// 把關重點 4：網址圖的位置以 recognize.Links 計（一般網頁連結也佔位置）；u1 指向第二個連結。
func TestGate_SearchSubmit_LinkIndexAfterWebLink(t *testing.T) {
	e, lc := newLinkSREnv(t)
	m := e.linkLegacy(srChannel, "看 https://web.com/page 跟 https://a.com/x.png 這張")
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Attachment: 1, Link: true}); !ok {
		t.Fatal("圖片網址應收錄在位置 1")
	}
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 1)))
	ed := posted(t, e.fake)
	if ed.Edit.Content == nil || *ed.Edit.Content != "https://a.com/x.png" {
		t.Fatalf("應貼出第二個連結：%v", ed.Edit.Content)
	}
	if got := lc.checked(); len(got) != 1 || got[0] != "https://a.com/x.png" {
		t.Errorf("確認的網址：%v", got)
	}
}

// 把關（mention）：網址中含 @everyone 字樣也不可通知。
func TestGate_SearchSubmit_LinkWithEveryone(t *testing.T) {
	e, _ := newLinkSREnv(t)
	u := "https://a.com/@everyone/x.png"
	m := e.linkLegacy(srChannel, "大家 "+u)
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	ed := posted(t, e.fake)
	if ed.Edit.Content == nil || *ed.Edit.Content != u {
		t.Fatalf("內容：%v", ed.Edit.Content)
	}
	if am := ed.Edit.AllowedMentions; am == nil || am.Parse == nil || len(am.Parse) != 0 {
		t.Errorf("mention 不應通知：%+v", am)
	}
}
