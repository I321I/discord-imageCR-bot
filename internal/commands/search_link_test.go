package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/index"
	"discordbot/internal/pipeline"

	"github.com/bwmarrin/discordgo"
)

const (
	srExample1    = "mygo、我早就知道會這樣了\nhttps://i.imgur.com/jgoYzsS.jpg"
	srExample1URL = "https://i.imgur.com/jgoYzsS.jpg"
)

// fakeLinkCheck 是假的圖片存在檢查：errs[url] 為回傳的錯誤；block 不為 nil 時每次先等它。
type fakeLinkCheck struct {
	mu       sync.Mutex
	errs     map[string]error
	urls     []string
	inFlight int
	maxIn    int
	block    chan struct{}
	started  chan struct{}
}

func (c *fakeLinkCheck) Check(ctx context.Context, url string) error {
	c.mu.Lock()
	c.urls = append(c.urls, url)
	c.inFlight++
	c.maxIn = max(c.maxIn, c.inFlight)
	block, started := c.block, c.started
	c.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if block != nil {
		<-block
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight--
	return c.errs[url]
}

func (c *fakeLinkCheck) checked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.urls...)
}

func newLinkSREnv(t *testing.T) (*srEnv, *fakeLinkCheck) {
	t.Helper()
	e := newSREnv(t)
	lc := &fakeLinkCheck{errs: map[string]error{}}
	e.s.LinkCheck = lc
	return e, lc
}

// linkLegacy 在頻道 ch 放一則使用者的舊圖片網址訊息並收錄進索引。
func (e *srEnv) linkLegacy(ch, content string) *discordgo.Message {
	m := e.fake.AddMessage(&discordgo.Message{ChannelID: ch, Author: &discordgo.User{ID: "someone"}, Content: content})
	e.x.PutLegacy(srGuild, m)
	return m
}

func linkKey(m *discordgo.Message, n int) string {
	return fmt.Sprintf("%s:%s:u%d", m.ChannelID, m.ID, n)
}

func TestSearchSubmit_LinkEntry_PostsURL(t *testing.T) {
	e, lc := newLinkSREnv(t)
	m := e.linkLegacy(srChannel, srExample1)
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	ed := posted(t, e.fake)
	if ed.Edit.Content == nil || *ed.Edit.Content != srExample1URL {
		t.Fatalf("內容應為網址：%v", ed.Edit.Content)
	}
	if len(ed.Edit.Files) != 0 {
		t.Error("網址圖不應上傳檔案")
	}
	if am := ed.Edit.AllowedMentions; am == nil || am.Parse == nil || len(am.Parse) != 0 || len(am.Users) != 0 || len(am.Roles) != 0 {
		t.Errorf("mention 不應通知：%+v", am)
	}
	if len(e.fetch.fetched()) != 0 {
		t.Error("不應下載")
	}
	if got := lc.checked(); len(got) != 1 || got[0] != srExample1URL {
		t.Errorf("貼出前應確認圖片存在：%v", got)
	}
}

// 劇透：貼出 ||網址||。
func TestSearchSubmit_LinkEntry_Spoiler(t *testing.T) {
	e, _ := newLinkSREnv(t)
	m := e.linkLegacy(srChannel, "劇透 ||https://a.com/x.png||")
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	if ed := posted(t, e.fake); ed.Edit.Content == nil || *ed.Edit.Content != "||https://a.com/x.png||" {
		t.Errorf("應保留劇透：%v", ed.Edit.Content)
	}
}

// 預覽的圖片在別的網址（U4）：確認預覽的圖片，貼出原網址。
func TestSearchSubmit_LinkEntry_ChecksFetchURL(t *testing.T) {
	e, lc := newLinkSREnv(t)
	m := e.fake.AddMessage(&discordgo.Message{ChannelID: srChannel, Author: &discordgo.User{ID: "someone"}, Content: "圖 https://imgur.com/abc",
		Embeds: []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: "https://imgur.com/abc", Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://i.imgur.com/abc.jpg"}}}})
	e.x.PutLegacy(srGuild, m)
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	if ed := posted(t, e.fake); ed.Edit.Content == nil || *ed.Edit.Content != "https://imgur.com/abc" {
		t.Errorf("應貼出原網址：%v", ed.Edit.Content)
	}
	if got := lc.checked(); len(got) != 1 || got[0] != "https://i.imgur.com/abc.jpg" {
		t.Errorf("應確認預覽的圖片：%v", got)
	}
}

func TestSearchSubmit_LinkEntry_ImageGone(t *testing.T) {
	for _, err := range []error{pipeline.ErrImageGone, pipeline.ErrNotImage} {
		e, lc := newLinkSREnv(t)
		m := e.linkLegacy(srChannel, srExample1)
		lc.errs[srExample1URL] = fmt.Errorf("包裝：%w", err)
		e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
		failedAfterDefer(t, e.fake, msgSearchLinkGone)
		if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Link: true}); ok {
			t.Errorf("%v：已刪除的圖應從索引移除", err)
		}
	}
}

func TestSearchSubmit_LinkEntry_CannotVerify(t *testing.T) {
	for _, err := range []error{errors.New("timeout"), pipeline.ErrBlockedAddress, pipeline.ErrTooLarge} {
		e, lc := newLinkSREnv(t)
		m := e.linkLegacy(srChannel, srExample1)
		lc.errs[srExample1URL] = err
		e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
		failedAfterDefer(t, e.fake, msgSearchLinkUnverified)
		if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Link: true}); !ok {
			t.Errorf("%v：無法確認時不應移除索引", err)
		}
	}
	// 沒有設定檢查器：無法確認。
	e := newSREnv(t)
	m := e.linkLegacy(srChannel, srExample1)
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	failedAfterDefer(t, e.fake, msgSearchLinkUnverified)
}

// 把關重點 4：網址已不在該位置 → 找不到並移除該筆。
func TestSearchSubmit_LinkEntry_URLChanged_Gone(t *testing.T) {
	e, lc := newLinkSREnv(t)
	m := e.linkLegacy(srChannel, srExample1)
	m.Content = "mygo、我早就知道會這樣了\nhttps://i.imgur.com/other.jpg" // Fake 中的訊息與 m 共用
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	failedAfterDefer(t, e.fake, msgSearchImageGone)
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Link: true}); ok {
		t.Error("網址被改掉應移除該筆")
	}
	if len(lc.checked()) != 0 {
		t.Error("找不到時不應連外檢查")
	}
}

func TestSearchSubmit_LinkEntry_MessageDeleted(t *testing.T) {
	e, _ := newLinkSREnv(t)
	m := e.linkLegacy(srChannel, srExample1)
	if err := e.fake.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	failedAfterDefer(t, e.fake, msgSearchImageGone)
	if es := e.x.Search("", map[string]bool{srChannel: true}, 0); len(es) != 0 {
		t.Errorf("訊息已刪除應移除：%+v", es)
	}
}

// 偽造指向看不到的頻道的網址 key → 找不到、不重讀訊息、不連外。
func TestSearchSubmit_LinkKey_ForgedChannel(t *testing.T) {
	e, lc := newLinkSREnv(t)
	m := e.linkLegacy(srHidden, srExample1)
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	immediate(t, e.fake, msgSearchImageGone)
	if n := e.fake.CallCount("ChannelMessage"); n != 0 || len(lc.checked()) != 0 {
		t.Errorf("不應讀訊息或連外：%d %v", n, lc.checked())
	}
}

// 附件 key 與網址 key 不互通：舊格式 c:m:0 指附件。
func TestSearchSubmit_LegacyKeyIsAttachment(t *testing.T) {
	e, _ := newLinkSREnv(t)
	m := e.linkLegacy(srChannel, srExample1)
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	immediate(t, e.fake, msgSearchImageGone)
}

func TestSearchSubmit_TypedQuery_LinkEntry(t *testing.T) {
	e, _ := newLinkSREnv(t)
	e.linkLegacy(srChannel, srExample1)
	e.s.Submit(context.Background(), searchSubmit(srUser, "mygo"))
	if ed := posted(t, e.fake); ed.Edit.Content == nil || *ed.Edit.Content != srExample1URL {
		t.Errorf("應貼出網址：%v", ed.Edit.Content)
	}
}

// 網址圖貼出失敗 → 貼圖失敗。
func TestSearchSubmit_LinkEntry_EditFails(t *testing.T) {
	e, _ := newLinkSREnv(t)
	m := e.linkLegacy(srChannel, srExample1)
	e.fake.FailOn["InteractionResponseEdit"] = discordtest.ErrInjected
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	if len(e.fake.Followups) != 1 || e.fake.Followups[0].Params.Content != msgSearchPostFailed {
		t.Errorf("應回貼圖失敗：%+v", e.fake.Followups)
	}
}

// 網址貼圖也排隊（同時 1 個）：檢查圖片時佔用貼圖位置。
func TestSearchSubmit_LinkEntry_Queued(t *testing.T) {
	e, lc := newLinkSREnv(t)
	m := e.linkLegacy(srChannel, srExample1)
	lc.block = make(chan struct{})
	lc.started = make(chan struct{}, 10)
	var wg sync.WaitGroup
	for n := 0; n < 3; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
		}()
	}
	<-lc.started
	time.Sleep(30 * time.Millisecond)
	if n := e.fake.CallCount("ChannelMessage"); n != searchPostSlots {
		t.Errorf("排隊中不應先讀訊息：%d", n)
	}
	close(lc.block)
	wg.Wait()
	if lc.maxIn != 1 {
		t.Errorf("同時檢查數 %d，預期 1", lc.maxIn)
	}
	if len(e.fake.ResponseEdits) != 3 {
		t.Errorf("三個都應貼出：%d", len(e.fake.ResponseEdits))
	}
}

func TestChoiceName_MixedNumbering(t *testing.T) {
	x := index.New()
	m := &discordgo.Message{ID: "1", ChannelID: "c", Content: "圖說 https://a.com/x.png", Author: &discordgo.User{ID: "u"},
		Attachments: []*discordgo.MessageAttachment{{ID: "a", Filename: "a.png", ContentType: "image/png"}}}
	x.PutLegacy("g", m)
	es := x.Search("", map[string]bool{"c": true}, 0)
	if len(es) != 2 {
		t.Fatalf("應有 2 筆：%+v", es)
	}
	if got := ChoiceName(es[0]); got != "圖說（第 1 張）" {
		t.Errorf("第 1 張：%q", got)
	}
	if got := ChoiceName(es[1]); got != "圖說（第 2 張）" || strings.Contains(got, "a.com") {
		t.Errorf("第 2 張：%q", got)
	}
}

func TestSearchLinkTexts(t *testing.T) {
	if msgSearchLinkGone != "圖床上的圖片已被刪除，無法貼出" || msgSearchLinkUnverified != "目前無法確認圖片，請稍後再試" {
		t.Errorf("文字：%q %q", msgSearchLinkGone, msgSearchLinkUnverified)
	}
}

// 「重新辨識」帶 ❌、只含圖片網址的原訊息：重新處理整則。
func TestRerecognize_FailedLinkOriginalReprocesses(t *testing.T) {
	e := newCMEnv(t)
	m := e.fake.AddMessage(&discordgo.Message{ChannelID: cmSource, GuildID: cmGuild, Author: &discordgo.User{ID: cmUser}, Content: srExample1,
		Reactions: []*discordgo.MessageReactions{{Count: 1, Me: true, Emoji: &discordgo.Emoji{Name: "❌"}}}})
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	if len(e.pipe.reproc) != 1 || e.pipe.reproc[0].ID != m.ID {
		t.Fatalf("應重新處理整則：%v", e.pipe.reproc)
	}
	deferred(t, e.fake, msgReprocessed)
}
