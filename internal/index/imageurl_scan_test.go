package index

import (
	"context"
	"slices"
	"testing"

	"discordbot/internal/discordtest"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

const example2 = "https://i.imgur.com/avJMxmH.jpg\n我不一想說得太失禮 你還是放棄吧"

func addUserLink(f *discordtest.Fake, ch, id, content string) *discordgo.Message {
	return f.AddMessage(&discordgo.Message{ID: id, ChannelID: ch, Content: content, Author: &discordgo.User{ID: "u1"}, Embeds: []*discordgo.MessageEmbed{}})
}

func TestScan_LegacyLinkMessageIndexed(t *testing.T) {
	f := discordtest.NewFake()
	addUserLink(f, "c", "100", example1)
	addUserLink(f, "c", "101", "只有一般連結 https://web.com/page")
	x, bf := New(), &fakeBackfill{}
	if err := ScanChannel(context.Background(), f, x, bf, botID, "g", "c", "200", "300"); err != nil {
		t.Fatal(err)
	}
	es := x.Search("", allow("c"), 0)
	if len(es) != 1 || es[0].Key.MessageID != "100" || es[0].ImageURL != example1URL {
		t.Errorf("setAt 之前的網址訊息應收錄、一般連結不收錄：%+v", es)
	}
	if len(bf.got()) != 0 {
		t.Errorf("不應補處理：%v", bf.got())
	}
}

// U1 (b)：只含網址的訊息依 max(setAt, urlSince)；附件訊息只看 setAt。
func TestScan_NewLinkMessageBackfilled(t *testing.T) {
	f := discordtest.NewFake()
	addUserLink(f, "c", "250", example2) // setAt 之後、urlSince 之前 → 舊圖片訊息
	addUserImage(f, "c", "260", "附件")    // 附件訊息：setAt 之後 → 補處理
	m := addUserLink(f, "c", "270", "附件＋網址 https://a.com/x.png")
	m.Attachments = []*discordgo.MessageAttachment{{Filename: "a.png", ContentType: "image/png"}} // 混合 → 依附件規則補處理
	addUserLink(f, "c", "350", example2)                                                          // urlSince 之後 → 補處理
	failed := addUserLink(f, "c", "360", example2)
	failed.Reactions = failedReaction()                       // 帶 ❌ → 不補也不收
	addUserLink(f, "c", "370", "動圖 https://tenor.com/view/x") // gifv（新訊息不處理，U3）→ 不補也不收
	f.ReplaceMessage(&discordgo.Message{ID: "370", ChannelID: "c", Content: "動圖 https://tenor.com/view/x", Author: &discordgo.User{ID: "u1"},
		Embeds: []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeGifv, URL: "https://tenor.com/view/x"}}})
	addUserLink(f, "c", "380", "<https://example.com/abc>") // 不是圖片
	x, bf := New(), &fakeBackfill{}
	if err := ScanChannel(context.Background(), f, x, bf, botID, "g", "c", "200", "300"); err != nil {
		t.Fatal(err)
	}
	if got := bf.got(); !slices.Equal(got, []string{"260", "270", "350"}) {
		t.Errorf("補處理 = %v，預期 [260 270 350]", got)
	}
	es := x.Search("", allow("c"), 0)
	if len(es) != 1 || es[0].Key.MessageID != "250" {
		t.Errorf("只有 250 應收錄：%v", keys(es))
	}
}

// url_since 不存在（寫入失敗等）：只含網址的訊息一律當舊圖片訊息收錄、不補處理。
func TestScan_NoURLSince_LinkMessagesLegacy(t *testing.T) {
	f := discordtest.NewFake()
	addUserLink(f, "c", "350", example2)
	addUserImage(f, "c", "360", "附件")
	x, bf := New(), &fakeBackfill{}
	if err := ScanChannel(context.Background(), f, x, bf, botID, "g", "c", "200", ""); err != nil {
		t.Fatal(err)
	}
	if got := bf.got(); !slices.Equal(got, []string{"360"}) {
		t.Errorf("補處理 = %v，預期只有附件訊息", got)
	}
	if es := x.Search("", allow("c"), 0); len(es) != 1 || es[0].Key.MessageID != "350" {
		t.Errorf("網址訊息應收錄：%v", keys(es))
	}
}

// Hooks 掃描時使用設定中的 url_since。
func TestHooks_ScanUsesURLSince(t *testing.T) {
	f, st, x, bf, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "200")
	if err := st.Update(context.Background(), func(d *store.Data) error { d.URLSince = "300"; return nil }); err != nil {
		t.Fatal(err)
	}
	addUserLink(f, "c", "250", example2)
	addUserLink(f, "c", "350", example2)
	h.ScanAll(st.Snapshot())
	if got := bf.got(); !slices.Equal(got, []string{"350"}) {
		t.Errorf("補處理 = %v", got)
	}
	if es := x.Search("", allow("c"), 0); len(es) != 1 || es[0].Key.MessageID != "250" {
		t.Errorf("收錄 = %v", keys(es))
	}
}

func hooksWithURLSince(t *testing.T) (*discordtest.Fake, *Index, *Hooks) {
	t.Helper()
	f, st, x, _, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "200")
	if err := st.Update(context.Background(), func(d *store.Data) error { d.URLSince = "300"; return nil }); err != nil {
		t.Fatal(err)
	}
	return f, x, h
}

func TestMessageUpdated_LegacyPartial_RefetchesFull(t *testing.T) {
	f, x, h := hooksWithURLSince(t)
	x.PutLegacy("g", linkMsg("c", "100", example1))
	addUserLink(f, "c", "100", "新文字\n"+example1URL)
	h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: "新文字\n" + example1URL, Author: &discordgo.User{ID: "u1"}})
	es := x.Search("", allow("c"), 0)
	if len(es) != 1 || es[0].LegacyText != "新文字" || es[0].ImageURL != example1URL {
		t.Errorf("應以重讀的完整訊息更新：%+v", es)
	}
	if f.CallCount("ChannelMessage") != 1 {
		t.Errorf("部分更新應重讀一次，得到 %d", f.CallCount("ChannelMessage"))
	}
}

// 完整的更新（attachments、embeds 都有）不重讀。
func TestMessageUpdated_LegacyFull_NoRefetch(t *testing.T) {
	f, x, h := hooksWithURLSince(t)
	x.PutLegacy("g", linkMsg("c", "100", example1))
	m := linkMsg("c", "100", "改過\n"+example1URL)
	m.Attachments, m.Embeds = []*discordgo.MessageAttachment{}, []*discordgo.MessageEmbed{}
	h.MessageUpdated(m)
	if es := x.Search("", allow("c"), 0); len(es) != 1 || es[0].LegacyText != "改過" {
		t.Errorf("應更新：%+v", es)
	}
	if f.CallCount("ChannelMessage") != 0 {
		t.Error("完整更新不應重讀")
	}
}

func TestMessageUpdated_LegacyLinkRemoved(t *testing.T) {
	f, x, h := hooksWithURLSince(t)
	x.PutLegacy("g", linkMsg("c", "100", example1))
	addUserLink(f, "c", "100", "mygo、我早就知道會這樣了")
	h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: "mygo、我早就知道會這樣了", Author: &discordgo.User{ID: "u1"}})
	if es := x.Search("", allow("c"), 0); len(es) != 0 {
		t.Errorf("網址被編輯掉應移除：%+v", es)
	}
}

func TestMessageUpdated_LegacyRefetch404(t *testing.T) {
	_, x, h := hooksWithURLSince(t)
	x.PutLegacy("g", linkMsg("c", "100", example1))
	h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: example1, Author: &discordgo.User{ID: "u1"}})
	if es := x.Search("", allow("c"), 0); len(es) != 0 {
		t.Errorf("重讀 404 應移除：%+v", es)
	}
}

func TestMessageUpdated_LegacyRefetchError_Unchanged(t *testing.T) {
	f, x, h := hooksWithURLSince(t)
	x.PutLegacy("g", linkMsg("c", "100", example1))
	f.FailOn["ChannelMessage"] = discordtest.ErrInjected
	h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: "新文字", Author: &discordgo.User{ID: "u1"}})
	if es := x.Search("", allow("c"), 0); len(es) != 1 || es[0].LegacyText != "mygo、我早就知道會這樣了" {
		t.Errorf("重讀失敗時索引不應改變：%+v", es)
	}
}

// 新訊息（附件訊息在 setAt 之後；網址訊息在 max(setAt, urlSince) 之後）的更新不動索引（觸發處理在 pipeline）。
func TestMessageUpdated_NewMessage_IndexUntouched(t *testing.T) {
	f, x, h := hooksWithURLSince(t)
	full := func(m *discordgo.Message) *discordgo.Message {
		m.Embeds = []*discordgo.MessageEmbed{}
		if m.Attachments == nil {
			m.Attachments = []*discordgo.MessageAttachment{}
		}
		return m
	}
	h.MessageUpdated(full(linkMsg("c", "350", example2)))
	h.MessageUpdated(full(legacyMsg("c", "250", "附件訊息", 1)))
	// 網址訊息在 setAt 與 urlSince 之間：舊圖片訊息 → 收錄。
	h.MessageUpdated(full(linkMsg("c", "260", example2)))
	es := x.Search("", allow("c"), 0)
	if len(es) != 1 || es[0].Key.MessageID != "260" {
		t.Errorf("只有 260 應收錄：%v", keys(es))
	}
	// urlSince 之後的部分更新不需要重讀。
	h.MessageUpdated(&discordgo.Message{ID: "400", ChannelID: "c", Content: "x", Author: &discordgo.User{ID: "u1"}})
	if f.CallCount("ChannelMessage") != 0 {
		t.Errorf("新訊息不應重讀：%d", f.CallCount("ChannelMessage"))
	}
}

// 重讀在 putMu 之外：重讀期間頻道被移除，取得 putMu 後重新確認，不寫入。
func TestMessageUpdated_RefetchThenChannelRemoved(t *testing.T) {
	f, st, x, _, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "200")
	addUserLink(f, "c", "100", example1)
	api := &hookAPI{Fake: f, onFetch: func() {
		removeSource(t, st, "g", "c")
	}}
	h.api = api
	h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: example1, Author: &discordgo.User{ID: "u1"}})
	if es := x.Search("", allow("c"), 0); len(es) != 0 {
		t.Errorf("頻道已移除不應寫入：%+v", es)
	}
}

type hookAPI struct {
	*discordtest.Fake
	onFetch func()
}

func (a *hookAPI) ChannelMessage(channelID, messageID string, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	m, err := a.Fake.ChannelMessage(channelID, messageID, options...)
	if a.onFetch != nil {
		a.onFetch()
	}
	return m, err
}

func removeSource(t *testing.T, st *store.Store, guildID, channelID string) {
	t.Helper()
	if err := st.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel(guildID, channelID); return nil }); err != nil {
		t.Fatal(err)
	}
}

// 把關後決定 (c)：批次刪除也算；刪除的是別則訊息時照常寫回。
func TestMessageUpdated_RefetchRacesBulkDelete(t *testing.T) {
	for _, c := range []struct {
		name   string
		del    func(h *Hooks)
		remain int
	}{
		{"批次刪除同一則", func(h *Hooks) { h.MessagesDeleted("c", []string{"99", "100"}) }, 0},
		{"刪除別則", func(h *Hooks) { h.MessageDeleted("c", "101") }, 1},
	} {
		f := discordtest.NewFake()
		g := &gatedAPI{Fake: f, fetched: make(chan struct{}), release: make(chan struct{})}
		st := store.New(f, "storage")
		st.UseEmpty()
		x := New()
		h := NewHooks(context.Background(), g, x, &fakeBackfill{}, st)
		addSource(t, st, "g", "c", "200")
		addUserLink(f, "c", "100", "新文字\n"+example1URL)
		done := make(chan struct{})
		go func() {
			h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: "新文字\n" + example1URL, Author: &discordgo.User{ID: "u1"}})
			close(done)
		}()
		<-g.fetched
		c.del(h)
		close(g.release)
		<-done
		if es := x.Search("", allow("c"), 0); len(es) != c.remain {
			t.Errorf("%s：剩 %d 筆，預期 %d", c.name, len(es), c.remain)
		}
		// 之後同一則訊息的更新不受先前刪除記錄影響（不殘留記錄）。
		h.refetchMu.Lock()
		n := len(h.refetching)
		h.refetchMu.Unlock()
		if n != 0 {
			t.Errorf("%s：重讀記錄應清除：%d", c.name, n)
		}
	}
}

// PM 決定（第二輪把關後）：完整的更新（不需重讀）處理途中收到刪除事件 → 不寫回。
func TestMessageUpdated_FullUpdateRacesDelete(t *testing.T) {
	_, st, x, _, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "200")
	h.putMu.Lock() // 讓 MessageUpdated 停在寫入前
	done := make(chan struct{})
	m := linkMsg("c", "100", example1)
	m.Attachments, m.Embeds = []*discordgo.MessageAttachment{}, []*discordgo.MessageEmbed{}
	go func() {
		h.MessageUpdated(m)
		close(done)
	}()
	waitCond(t, "登記處理中", func() bool {
		h.refetchMu.Lock()
		defer h.refetchMu.Unlock()
		return len(h.refetching) == 1
	})
	h.MessageDeleted("c", "100")
	h.putMu.Unlock()
	<-done
	if es := x.Search("", allow("c"), 0); len(es) != 0 {
		t.Errorf("處理途中被刪除不應寫回：%+v", es)
	}
	h.refetchMu.Lock()
	defer h.refetchMu.Unlock()
	if len(h.refetching) != 0 {
		t.Errorf("登記應清除：%d", len(h.refetching))
	}
}
