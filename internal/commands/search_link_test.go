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

// linkLegacyWithEmbeds 同 linkLegacy，訊息帶 embeds。
func (e *srEnv) linkLegacyWithEmbeds(ch, content string, embeds ...*discordgo.MessageEmbed) *discordgo.Message {
	m := e.fake.AddMessage(&discordgo.Message{ChannelID: ch, Author: &discordgo.User{ID: "someone"}, Content: content, Embeds: embeds})
	e.x.PutLegacy(srGuild, m)
	return m
}

// 使用者決定（2026-10-19 把關後）：只有確定已刪除或確定不是圖片才擋下並移除索引；
// 其他錯誤（403、被擋、逾時、太大等）只有在 Discord 對這個網址產生了圖片預覽時才照樣貼出，
// 只靠副檔名判定的回「目前無法確認圖片」、不移除索引（避免散播偽裝成 .jpg 的釣魚連結）。
func TestSearchSubmit_LinkEntry_UncertainStillPosts(t *testing.T) {
	errs := []error{errors.New("HTTP 403"), errors.New("timeout"), context.DeadlineExceeded, pipeline.ErrBlockedAddress, pipeline.ErrTooLarge, pipeline.ErrUnconfirmedImage}
	// (b) 有 image 預覽、Check 失敗 → 貼出。
	for _, err := range errs {
		e, lc := newLinkSREnv(t)
		m := e.linkLegacyWithEmbeds(srChannel, srExample1, &discordgo.MessageEmbed{Type: discordgo.EmbedTypeImage, URL: srExample1URL})
		lc.errs[srExample1URL] = err
		e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
		if ed := posted(t, e.fake); ed.Edit.Content == nil || *ed.Edit.Content != srExample1URL {
			t.Errorf("%v：有圖片預覽時應照樣貼出：%v", err, ed.Edit.Content)
		}
		if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Link: true}); !ok {
			t.Errorf("%v：不應移除索引", err)
		}
	}
	// image 型預覽的 thumbnail 就是這個網址也算圖片預覽（article 的縮圖不算，見 UncertainWithoutPreview）。
	{
		e, lc := newLinkSREnv(t)
		m := e.linkLegacyWithEmbeds(srChannel, srExample1, &discordgo.MessageEmbed{Type: discordgo.EmbedTypeImage, URL: "https://other.com", Thumbnail: &discordgo.MessageEmbedThumbnail{URL: srExample1URL}})
		lc.errs[srExample1URL] = errors.New("HTTP 403")
		e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
		if ed := posted(t, e.fake); ed.Edit.Content == nil || *ed.Edit.Content != srExample1URL {
			t.Errorf("thumbnail 對應此網址時應貼出：%v", ed.Edit.Content)
		}
	}
}

func TestSearchSubmit_LinkEntry_UncertainWithoutPreview(t *testing.T) {
	cases := []struct {
		name   string
		embeds []*discordgo.MessageEmbed
	}{
		{"(a) 只有副檔名、沒有預覽", nil},
		{"(c) 預覽是 article", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeArticle, URL: srExample1URL, Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://phish.example/logo.png"}}}},
		{"(c) 預覽是 link", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeLink, URL: srExample1URL}}},
		// PM 決定（第三輪把關後）：article 的縮圖（網頁 og:image）就是這個網址也不算。
		{"article 的縮圖是此網址", []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeArticle, URL: "https://phish.example/", Thumbnail: &discordgo.MessageEmbedThumbnail{URL: srExample1URL}}}},
	}
	for _, c := range cases {
		e, lc := newLinkSREnv(t)
		m := e.linkLegacyWithEmbeds(srChannel, srExample1, c.embeds...)
		lc.errs[srExample1URL] = errors.New("HTTP 403")
		e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
		failedAfterDefer(t, e.fake, msgSearchLinkUnverified)
		for _, ed := range e.fake.ResponseEdits {
			if ed.Edit.Content != nil && strings.Contains(*ed.Edit.Content, "imgur") {
				t.Errorf("%s：不應貼出網址", c.name)
			}
		}
		if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Link: true}); !ok {
			t.Errorf("%s：不應移除索引", c.name)
		}
	}
	// 沒有設定檢查器：等同無法確認，套用同一規則。
	e := newSREnv(t)
	m := e.linkLegacy(srChannel, srExample1)
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	failedAfterDefer(t, e.fake, msgSearchLinkUnverified)
	e = newSREnv(t)
	m = e.linkLegacyWithEmbeds(srChannel, srExample1, &discordgo.MessageEmbed{Type: discordgo.EmbedTypeImage, URL: srExample1URL})
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	if ed := posted(t, e.fake); ed.Edit.Content == nil || *ed.Edit.Content != srExample1URL {
		t.Errorf("沒有檢查器、有圖片預覽時應貼出：%v", ed.Edit.Content)
	}
}

// 把關後決定 (d)：確認圖片使用 15 秒逾時；逾時視為無法確認，照樣貼出。
func TestSearchSubmit_LinkCheckTimeout(t *testing.T) {
	if defaultLinkCheckTimeout != 15*time.Second {
		t.Errorf("預設逾時 %v，預期 15 秒", defaultLinkCheckTimeout)
	}
	e := newSREnv(t)
	dc := &deadlineCheck{}
	e.s.LinkCheck = dc
	start := time.Now()
	m := e.linkLegacy(srChannel, srExample1)
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	if dl, ok := dc.deadline(); !ok || dl.Sub(start) > 15*time.Second+time.Second || dl.Sub(start) < 14*time.Second {
		t.Errorf("Check 應有約 15 秒的期限：%v %v", dl.Sub(start), ok)
	}
	// 實際逾時（縮短期限）：有圖片預覽時照樣貼出。
	e = newSREnv(t)
	e.s.LinkCheck = &deadlineCheck{wait: true}
	e.s.LinkCheckTimeout = 20 * time.Millisecond
	m = e.linkLegacyWithEmbeds(srChannel, srExample1, &discordgo.MessageEmbed{Type: discordgo.EmbedTypeImage, URL: srExample1URL})
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	if ed := posted(t, e.fake); ed.Edit.Content == nil || *ed.Edit.Content != srExample1URL {
		t.Errorf("逾時後應照樣貼出：%v", ed.Edit.Content)
	}
}

// deadlineCheck 記錄 Check 收到的 ctx 期限；wait 時等到 ctx 結束才回傳。
type deadlineCheck struct {
	mu   sync.Mutex
	dl   time.Time
	ok   bool
	wait bool
}

func (c *deadlineCheck) Check(ctx context.Context, url string) error {
	c.mu.Lock()
	c.dl, c.ok = ctx.Deadline()
	c.mu.Unlock()
	if c.wait {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (c *deadlineCheck) deadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dl, c.ok
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

// 使用者決定（2026-10-19 把關後）：帶 ❌ 但只剩 gifv 的訊息「重新辨識」→「這則訊息沒有可處理的圖片」。
func TestRerecognize_FailedOriginalNoImages(t *testing.T) {
	e := newCMEnv(t)
	m := failedOriginal(e, cmUser, true)
	e.pipe.reprocErr = fmt.Errorf("包裝：%w", pipeline.ErrNoImages)
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	deferred(t, e.fake, msgNoImages)
	if msgNoImages != "這則訊息沒有可處理的圖片" {
		t.Errorf("文字：%q", msgNoImages)
	}
}

// 第二輪把關後：Check 回「無法確認是點陣圖」（例如 SVG）且沒有 Discord 圖片預覽 → 無法確認、不移除索引。
func TestSearchSubmit_LinkEntry_UnconfirmedRasterNoPreview(t *testing.T) {
	e, lc := newLinkSREnv(t)
	m := e.linkLegacy(srChannel, srExample1)
	lc.errs[srExample1URL] = fmt.Errorf("包裝：%w", pipeline.ErrUnconfirmedImage)
	e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
	failedAfterDefer(t, e.fake, msgSearchLinkUnverified)
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Link: true}); !ok {
		t.Error("不應移除索引")
	}
}

// PM 決定（第二輪把關後）：重讀到的訊息作者是 bot 或 webhook → 視為找不到、不連外、不貼出。
func TestSearchSubmit_LinkEntry_BotOrWebhookAuthor(t *testing.T) {
	for _, mod := range []func(m *discordgo.Message){
		func(m *discordgo.Message) { m.Author = &discordgo.User{ID: "otherbot", Bot: true} },
		func(m *discordgo.Message) { m.WebhookID = "wh" },
		func(m *discordgo.Message) { m.Author = nil },
	} {
		e, lc := newLinkSREnv(t)
		m := e.linkLegacy(srChannel, srExample1)
		mod(m) // Fake 中的訊息與 m 共用
		e.s.Submit(context.Background(), searchSubmit(srUser, linkKey(m, 0)))
		failedAfterDefer(t, e.fake, msgSearchImageGone)
		if len(lc.checked()) != 0 {
			t.Error("不應連外檢查")
		}
		if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Link: true}); ok {
			t.Error("應移除該筆")
		}
	}
}
