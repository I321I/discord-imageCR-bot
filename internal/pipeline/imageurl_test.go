package pipeline

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/imagemsg"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

const (
	example1    = "mygo、我早就知道會這樣了\nhttps://i.imgur.com/jgoYzsS.jpg"
	example1URL = "https://i.imgur.com/jgoYzsS.jpg"
	example2    = "https://i.imgur.com/avJMxmH.jpg\n我不一想說得太失禮 你還是放棄吧"
	example2URL = "https://i.imgur.com/avJMxmH.jpg"
	noExtURL    = "https://example.com/abc"
)

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.Black)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type linkEnv struct {
	*env
	urls *fakeFetcher
}

// newLinkEnv 同 newEnv，另外設好圖片網址功能上線時間點（"1"）、網址下載器與短的 embed 等待時間。
func newLinkEnv(t *testing.T) *linkEnv {
	t.Helper()
	e := newEnv(t)
	if _, err := e.p.Store.EnsureURLSince(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	e.fake.Sent = nil
	e.fake.Calls = map[string]int{}
	urls := &fakeFetcher{data: map[string][]byte{}, errs: map[string]error{}}
	e.p.URLFetch = urls
	e.p.EmbedWait = 10 * time.Millisecond
	return &linkEnv{env: e, urls: urls}
}

// linkMessage 在來源頻道放一則使用者文字訊息（沒有附件）。
func (e *linkEnv) linkMessage(content string, embeds ...*discordgo.MessageEmbed) *discordgo.Message {
	return e.fake.AddMessage(&discordgo.Message{
		ChannelID: testSource, GuildID: testGuild, Author: &discordgo.User{ID: testUser},
		Content: content, Embeds: append([]*discordgo.MessageEmbed{}, embeds...), Attachments: []*discordgo.MessageAttachment{},
	})
}

func (e *linkEnv) noTrace(t *testing.T, m *discordgo.Message) {
	t.Helper()
	if len(e.fake.Sent) != 0 || len(e.fake.Deleted) != 0 {
		t.Errorf("不應處理：Sent %d Deleted %d", len(e.fake.Sent), len(e.fake.Deleted))
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); len(got) != 0 {
		t.Errorf("不應留下反應：%q", got)
	}
}

func TestProcess_LinkOnly_PostsAndDeletes(t *testing.T) {
	e := newLinkEnv(t)
	e.urls.data[example1URL] = jpegBytes(t, 64, 36)
	m := e.linkMessage(example1)
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 1 {
		t.Fatalf("應發出 1 則，得到 %d", len(e.fake.Sent))
	}
	s := e.fake.Sent[0]
	want := imagemsg.Format(imagemsg.Fields{Work: "葬送のフリーレン", Text: "這就是人類啊", Keywords: "mygo、我早就知道會這樣了"}, testUser)
	if s.Data.Content != want {
		t.Errorf("內容：\n%s\n預期：\n%s", s.Data.Content, want)
	}
	if f := s.Data.Files; len(f) != 1 || f[0].Name != "jgoYzsS.jpg" || f[0].ContentType != "image/jpeg" {
		t.Errorf("附件：%+v", s.Data.Files)
	}
	if !bytes.Equal(s.Files[0], e.urls.data[example1URL]) {
		t.Error("附件 bytes 應為下載的圖片")
	}
	if am := s.Data.AllowedMentions; am == nil || am.Parse == nil || len(am.Parse) != 0 {
		t.Errorf("mention 不應通知：%+v", am)
	}
	if e.originalExists(m) {
		t.Error("全部成功應刪除原訊息")
	}
	if got := e.urls.fetched(); !slices.Equal(got, []string{example1URL}) {
		t.Errorf("網址下載：%v", got)
	}
	if len(e.fetch.fetched()) != 0 {
		t.Error("網址圖不應用附件下載器")
	}
}

// U8：附件在前、網址在後。
func TestProcess_MixedSources_OrderAndCount(t *testing.T) {
	e := newLinkEnv(t)
	m := e.imageMessage(t, "a.png")
	m.Content = "文字 https://a.com/x.png"
	e.fake.ReplaceMessage(m)
	e.urls.data["https://a.com/x.png"] = pngBytes(t, 80, 40)
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 2 {
		t.Fatalf("應發出 2 則，得到 %d", len(e.fake.Sent))
	}
	if e.fake.Sent[0].Data.Files[0].Name != "a.png" || e.fake.Sent[1].Data.Files[0].Name != "x.png" {
		t.Errorf("順序應為附件、網址：%s %s", e.fake.Sent[0].Data.Files[0].Name, e.fake.Sent[1].Data.Files[0].Name)
	}
	for _, s := range e.fake.Sent {
		if !strings.Contains(s.Data.Content, "文字") || strings.Contains(s.Data.Content, "a.com") {
			t.Errorf("關鍵字應為去掉網址的文字：%s", s.Data.Content)
		}
	}
}

// 檔名：副檔名與實際格式不符時改為 image.<格式>；劇透加 SPOILER_；只留安全字元。
func TestProcess_LinkFilenameAndSpoiler(t *testing.T) {
	cases := []struct {
		content, url string
		data         func(t *testing.T) []byte
		name, ctype  string
	}{
		{"https://a.com/x.jpg", "https://a.com/x.jpg", func(t *testing.T) []byte { return pngBytes(t, 8, 8) }, "image.png", "image/png"},
		{"||https://a.com/s.png||", "https://a.com/s.png", func(t *testing.T) []byte { return pngBytes(t, 8, 8) }, "SPOILER_s.png", "image/png"},
		{"https://a.com/%E5%9C%96%20a.JPEG", "https://a.com/%E5%9C%96%20a.JPEG", func(t *testing.T) []byte { return jpegBytes(t, 8, 8) }, "a.JPEG", "image/jpeg"},
		{"https://a.com/..%2F..%2Fx.png", "https://a.com/..%2F..%2Fx.png", func(t *testing.T) []byte { return pngBytes(t, 8, 8) }, "x.png", "image/png"},
	}
	for _, c := range cases {
		e := newLinkEnv(t)
		e.urls.data[c.url] = c.data(t)
		m := e.linkMessage(c.content + " 文字")
		if err := e.p.Process(context.Background(), testGuild, m); err != nil {
			t.Fatalf("%s：%v", c.content, err)
		}
		if f := e.fake.Sent[0].Data.Files[0]; f.Name != c.name || f.ContentType != c.ctype {
			t.Errorf("%s：檔名 %q 類型 %q，預期 %q %q", c.content, f.Name, f.ContentType, c.name, c.ctype)
		}
	}
}

// 把關重點 2：任一網址失敗 → 一則都不發、不刪、加 ❌。
func TestProcess_URLFailure_SendsNothingKeepsOriginal(t *testing.T) {
	bmp := []byte("BM\x3a\x00\x00\x00\x00\x00\x00\x00\x36\x00\x00\x00\x28\x00\x00\x00\x01\x00\x00\x00\x01\x00\x00\x00\x01\x00\x18\x00")
	cases := []struct {
		name string
		set  func(t *testing.T, f *fakeFetcher, u string)
		want error
	}{
		{"下載錯誤", func(t *testing.T, f *fakeFetcher, u string) { f.errs[u] = discordtest.ErrInjected }, discordtest.ErrInjected},
		{"太大", func(t *testing.T, f *fakeFetcher, u string) { f.errs[u] = ErrTooLarge }, ErrTooLarge},
		{"不是圖片（Content-Type）", func(t *testing.T, f *fakeFetcher, u string) { f.errs[u] = ErrNotImage }, ErrNotImage},
		{"無法解碼", func(t *testing.T, f *fakeFetcher, u string) { f.data[u] = []byte("<html>not image</html>") }, ErrNotImage},
		{"不支援的格式（U5）", func(t *testing.T, f *fakeFetcher, u string) { f.data[u] = bmp }, ErrNotImage},
		{"已刪除", func(t *testing.T, f *fakeFetcher, u string) { f.errs[u] = ErrImageGone }, ErrImageGone},
	}
	for _, c := range cases {
		e := newLinkEnv(t)
		m := e.imageMessage(t, "a.png")
		m.Content = "https://a.com/ok.png https://a.com/bad.png"
		e.fake.ReplaceMessage(m)
		e.urls.data["https://a.com/ok.png"] = pngBytes(t, 8, 8)
		c.set(t, e.urls, "https://a.com/bad.png")
		err := e.p.Process(context.Background(), testGuild, m)
		if !errors.Is(err, c.want) {
			t.Errorf("%s：錯誤 %v，預期 %v", c.name, err, c.want)
		}
		if len(e.fake.Sent) != 0 || !e.originalExists(m) || len(e.fake.Deleted) != 0 {
			t.Errorf("%s：不應發出或刪除：Sent %d Deleted %d", c.name, len(e.fake.Sent), len(e.fake.Deleted))
		}
		if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) {
			t.Errorf("%s：應只有 ❌：%q", c.name, got)
		}
	}
}

func TestProcess_NoURLFetcher_Fails(t *testing.T) {
	e := newLinkEnv(t)
	e.p.URLFetch = nil
	m := e.linkMessage(example1)
	if err := e.p.Process(context.Background(), testGuild, m); err == nil {
		t.Fatal("沒有網址下載器應失敗")
	}
	if !e.originalExists(m) || len(e.fake.Sent) != 0 {
		t.Error("失敗時不應發出或刪除")
	}
}

// 把關重點 2（潛在問題修正）：補處理重讀後已沒有圖片 → 不發、不刪、不加 ❌，⏳ 移除。
func TestProcess_NoSourcesAfterRefetch_DoesNotDelete(t *testing.T) {
	cases := []struct {
		name   string
		modify func(m *discordgo.Message)
	}{
		{"網址被編輯掉", func(m *discordgo.Message) { m.Content = "mygo、我早就知道會這樣了"; m.Attachments = nil }},
		{"附件被清空", func(m *discordgo.Message) { m.Content = "文字"; m.Attachments = []*discordgo.MessageAttachment{} }},
		{"只剩 gifv", func(m *discordgo.Message) {
			m.Content = "https://tenor.com/view/x"
			m.Attachments = nil
			m.Embeds = []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeGifv, URL: "https://tenor.com/view/x"}}
		}},
	}
	for _, c := range cases {
		e := newLinkEnv(t)
		var m *discordgo.Message
		if c.name == "附件被清空" {
			m = e.imageMessage(t, "a.png")
		} else {
			m = e.linkMessage(example1)
		}
		fresh := *m
		c.modify(&fresh)
		e.fake.ReplaceMessage(&fresh)
		if err := <-e.p.EnqueueBackfill(context.Background(), testGuild, m); err != nil {
			t.Errorf("%s：不應回錯誤：%v", c.name, err)
		}
		if len(e.fake.Sent) != 0 || len(e.fake.Deleted) != 0 || !e.originalExists(m) {
			t.Errorf("%s：不應發出或刪除：Sent %d Deleted %d", c.name, len(e.fake.Sent), len(e.fake.Deleted))
		}
		if got := e.fake.ReactionsOn(testSource, m.ID); len(got) != 0 {
			t.Errorf("%s：不應留下 ⏳ 或 ❌：%q", c.name, got)
		}
	}
	// Process（即時）收到沒有圖片的訊息也一樣。
	e := newLinkEnv(t)
	m := e.linkMessage("純文字")
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Errorf("沒有圖片不應回錯誤：%v", err)
	}
	if !e.originalExists(m) || len(e.fake.Deleted) != 0 {
		t.Error("沒有圖片不應刪除原訊息")
	}
}

func TestHandleMessage_ExtensionLink_ProcessesImmediately(t *testing.T) {
	e := newLinkEnv(t)
	e.p.EmbedWait = time.Hour
	e.urls.data[example2URL] = jpegBytes(t, 8, 8)
	m := e.linkMessage(example2)
	e.p.HandleMessage(context.Background(), m)
	if n := e.fake.CallCount("ChannelMessage"); n != 0 {
		t.Errorf("不需等待重讀：ChannelMessage %d 次", n)
	}
	if len(e.fake.Sent) != 1 || e.originalExists(m) {
		t.Errorf("副檔名圖片網址應立即處理：Sent %d", len(e.fake.Sent))
	}
}

func TestHandleMessage_UnresolvedLinkWaitsThenRefetches(t *testing.T) {
	for _, c := range []struct {
		name    string
		embed   *discordgo.MessageEmbed
		process bool
	}{
		{"補上 image 預覽", &discordgo.MessageEmbed{Type: discordgo.EmbedTypeImage, URL: noExtURL}, true},
		{"補上 article 預覽", &discordgo.MessageEmbed{Type: discordgo.EmbedTypeArticle, URL: noExtURL}, false},
		{"一直沒有預覽", nil, false},
	} {
		e := newLinkEnv(t)
		e.p.EmbedWait = 50 * time.Millisecond
		e.urls.data[noExtURL] = pngBytes(t, 8, 8)
		m := e.linkMessage("文字 " + noExtURL)
		done := make(chan struct{})
		go func() {
			e.p.HandleMessage(context.Background(), m)
			close(done)
		}()
		time.Sleep(10 * time.Millisecond)
		if len(e.fake.ReactionCalls) != 0 {
			t.Errorf("%s：等待期間不應加 ⏳", c.name)
		}
		if c.embed != nil {
			fresh := *m
			fresh.Embeds = []*discordgo.MessageEmbed{c.embed}
			e.fake.ReplaceMessage(&fresh)
		}
		<-done
		if c.process {
			if len(e.fake.Sent) != 1 || e.originalExists(m) {
				t.Errorf("%s：應處理一次：Sent %d", c.name, len(e.fake.Sent))
			}
		} else {
			e.noTrace(t, m)
		}
	}
}

// 一個副檔名網址＋一個尚無預覽的網址：等待後兩張都是圖片則發 2 則（不先處理副檔名那張）。
func TestHandleMessage_ExtensionPlusUnresolved_Waits(t *testing.T) {
	e := newLinkEnv(t)
	e.p.EmbedWait = 50 * time.Millisecond
	e.urls.data["https://a.com/x.png"] = pngBytes(t, 8, 8)
	e.urls.data[noExtURL] = pngBytes(t, 9, 9)
	m := e.linkMessage("https://a.com/x.png " + noExtURL + " 文字")
	done := make(chan struct{})
	go func() {
		e.p.HandleMessage(context.Background(), m)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	if len(e.fake.Sent) != 0 || len(e.urls.fetched()) != 0 {
		t.Error("等待期間不應處理")
	}
	fresh := *m
	fresh.Embeds = []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: noExtURL}}
	e.fake.ReplaceMessage(&fresh)
	<-done
	if len(e.fake.Sent) != 2 || e.originalExists(m) {
		t.Errorf("應發出 2 則並刪原訊息：Sent %d", len(e.fake.Sent))
	}
}

// 等待後仍沒有預覽：未判定的連結當一般連結（留在關鍵字中），其他圖照常處理。
func TestHandleMessage_UnresolvedTreatedAsPlainLinkAfterWait(t *testing.T) {
	e := newLinkEnv(t)
	e.urls.data["https://a.com/x.png"] = pngBytes(t, 8, 8)
	m := e.linkMessage("https://a.com/x.png " + noExtURL)
	e.p.HandleMessage(context.Background(), m)
	if len(e.fake.Sent) != 1 || !strings.Contains(e.fake.Sent[0].Data.Content, noExtURL) {
		t.Errorf("應發出 1 則且關鍵字保留一般連結：%+v", e.fake.Sent)
	}
}

func TestHandleMessage_ShutdownDuringWait(t *testing.T) {
	e := newLinkEnv(t)
	e.p.EmbedWait = time.Hour
	m := e.linkMessage(noExtURL)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.p.HandleMessage(ctx, m)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("關機時應立即結束等待")
	}
	if n := e.fake.CallCount("ChannelMessage"); n != 0 {
		t.Errorf("關機時不應重讀：%d", n)
	}
	e.noTrace(t, m)
}

// U1 (b)：只含網址的訊息早於 url_since（或沒有 url_since）不處理；附件訊息照舊。
func TestHandleMessage_LinkOnlyNeedsURLSince(t *testing.T) {
	e := newEnv(t) // 沒有 url_since
	urls := &fakeFetcher{data: map[string][]byte{example2URL: jpegBytes(t, 8, 8)}, errs: map[string]error{}}
	e.p.URLFetch = urls
	lm := e.fake.AddMessage(&discordgo.Message{ChannelID: testSource, Author: &discordgo.User{ID: testUser}, Content: example2})
	e.p.HandleMessage(context.Background(), lm)
	if len(e.fake.Sent) != 0 || !e.originalExists(lm) || len(e.fake.ReactionCalls) != 0 {
		t.Errorf("沒有 url_since 時網址訊息不應處理：Sent %d", len(e.fake.Sent))
	}
	am := e.imageMessage(t, "a.png")
	e.p.HandleMessage(context.Background(), am)
	if len(e.fake.Sent) != 1 {
		t.Errorf("附件訊息照舊處理：Sent %d", len(e.fake.Sent))
	}
}

// U3：只有 gifv 的新訊息不處理、不加 ⏳／❌。
func TestHandleMessage_GIFVOnlyIgnored(t *testing.T) {
	e := newLinkEnv(t)
	const g = "https://tenor.com/view/x"
	m := e.linkMessage("動圖 "+g, &discordgo.MessageEmbed{Type: discordgo.EmbedTypeGifv, URL: g})
	e.p.HandleMessage(context.Background(), m)
	e.noTrace(t, m)
}

// 把關重點 3：create（未判定）與 update（預覽補上）幾乎同時 → 只處理一輪。
func TestHandleMessageUpdate_EmbedArrives_ProcessesOnce(t *testing.T) {
	e := newLinkEnv(t)
	e.p.EmbedWait = 30 * time.Millisecond
	e.urls.data[noExtURL] = pngBytes(t, 8, 8)
	m := e.linkMessage("文字 " + noExtURL)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.p.HandleMessage(context.Background(), m)
	}()
	time.Sleep(5 * time.Millisecond)
	fresh := *m
	fresh.Embeds = []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: noExtURL}}
	e.fake.ReplaceMessage(&fresh)
	// 預覽補上的更新事件是部分的：沒有作者與內容。
	e.p.HandleMessageUpdate(context.Background(), &discordgo.Message{ID: m.ID, ChannelID: m.ChannelID, Embeds: fresh.Embeds})
	wg.Wait()
	if len(e.fake.Sent) != 1 || len(e.fake.Deleted) != 1 {
		t.Errorf("應只處理一輪：Sent %d Deleted %d", len(e.fake.Sent), len(e.fake.Deleted))
	}
}

// U2：設定時間點之後的文字訊息被編輯加上圖片網址 → 處理。
func TestHandleMessageUpdate_UserEditAddsLink(t *testing.T) {
	e := newLinkEnv(t)
	e.urls.data[example2URL] = jpegBytes(t, 8, 8)
	m := e.linkMessage("文字")
	fresh := *m
	fresh.Content = example2
	e.fake.ReplaceMessage(&fresh)
	e.p.HandleMessageUpdate(context.Background(), &fresh)
	waitUntil(t, "處理完成", func() bool { return e.fake.CallCount("ChannelMessageDelete") == 1 })
	if len(e.fake.Sent) != 1 {
		t.Errorf("編輯加入網址應處理：Sent %d", len(e.fake.Sent))
	}
}

func TestHandleMessageUpdate_SkipsFailedAndLegacy(t *testing.T) {
	cases := []struct {
		name   string
		modify func(m *discordgo.Message)
	}{
		{"帶 ❌", func(m *discordgo.Message) { m.Reactions = failedReaction() }},
		{"bot", func(m *discordgo.Message) { m.Author = &discordgo.User{ID: "otherbot", Bot: true} }},
		{"webhook", func(m *discordgo.Message) { m.WebhookID = "wh" }},
		{"非來源頻道", func(m *discordgo.Message) { m.ChannelID = "other" }},
		{"仍有未判定的連結", func(m *discordgo.Message) { m.Content += " " + noExtURL }},
		{"一般連結", func(m *discordgo.Message) { m.Content = "https://web.com/page" }},
	}
	for _, c := range cases {
		e := newLinkEnv(t)
		e.urls.data[example2URL] = jpegBytes(t, 8, 8)
		m := e.linkMessage(example2)
		fresh := *m
		c.modify(&fresh)
		e.fake.ReplaceMessage(&fresh)
		e.p.HandleMessageUpdate(context.Background(), &fresh)
		time.Sleep(20 * time.Millisecond)
		if len(e.fake.Sent) != 0 || len(e.fake.Deleted) != 0 {
			t.Errorf("%s：不應處理：Sent %d", c.name, len(e.fake.Sent))
		}
	}
	// 設定時間點（url_since）之前的網址訊息（舊圖片訊息）。
	e := newLinkEnv(t)
	if err := e.p.Store.Update(context.Background(), func(d *store.Data) error { d.URLSince = "99999999999999999"; return nil }); err != nil {
		t.Fatal(err)
	}
	e.fake.Sent = nil
	e.urls.data[example2URL] = jpegBytes(t, 8, 8)
	m := e.linkMessage(example2)
	e.p.HandleMessageUpdate(context.Background(), m)
	time.Sleep(20 * time.Millisecond)
	if len(e.fake.Sent) != 0 {
		t.Errorf("舊圖片訊息不應處理：Sent %d", len(e.fake.Sent))
	}
	if n := e.fake.CallCount("ChannelMessage"); n != 0 {
		t.Errorf("確定是舊訊息時不需重讀：%d", n)
	}
	// 重讀 404：放棄。
	e2 := newLinkEnv(t)
	e2.p.HandleMessageUpdate(context.Background(), &discordgo.Message{ID: "999", ChannelID: testSource, Author: &discordgo.User{ID: testUser}, Content: example2})
	if len(e2.fake.Sent) != 0 || len(e2.fake.ReactionCalls) != 0 {
		t.Error("訊息已刪除時不應處理")
	}
}

func TestReprocess_LinkMessage(t *testing.T) {
	e := newLinkEnv(t)
	e.urls.data[example2URL] = jpegBytes(t, 8, 8)
	m := e.linkMessage(example2)
	m.Reactions = failedReaction()
	if err := <-e.p.Reprocess(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 1 || e.originalExists(m) {
		t.Errorf("應發出並刪除：Sent %d", len(e.fake.Sent))
	}

	e = newLinkEnv(t)
	e.urls.errs[example2URL] = ErrImageGone
	m = e.linkMessage(example2)
	m.Reactions = failedReaction()
	if err := <-e.p.Reprocess(context.Background(), testGuild, m); err == nil {
		t.Fatal("下載失敗應回錯誤")
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) || !e.originalExists(m) {
		t.Errorf("失敗應保留原訊息並重新加 ❌：%q", got)
	}
}
