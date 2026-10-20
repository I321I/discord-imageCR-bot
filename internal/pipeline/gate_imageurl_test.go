package pipeline

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"discordbot/internal/recognize"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// blockingFetcher 在 Fetch 時通知 started 並等到 ctx 結束。
type blockingFetcher struct {
	started chan struct{}
}

func (f *blockingFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	f.started <- struct{}{}
	<-ctx.Done()
	return nil, ctx.Err()
}

// 把關重點 2：下載網址圖途中關機 → 不發、不刪、不加 ❌（留給下次補處理）。
func TestGate_Process_ShutdownDuringLinkFetch(t *testing.T) {
	e := newLinkEnv(t)
	bf := &blockingFetcher{started: make(chan struct{}, 1)}
	e.p.URLFetch = bf
	m := e.imageMessage(t, "a.png")
	m.Content = "文字 https://a.com/x.png"
	e.fake.ReplaceMessage(m)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.p.Process(ctx, testGuild, m) }()
	select {
	case <-bf.started:
	case <-time.After(5 * time.Second):
		t.Fatal("網址下載沒有開始")
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("關機中斷應回錯誤")
	}
	if len(e.fake.Sent) != 0 || len(e.fake.Deleted) != 0 || !e.originalExists(m) {
		t.Errorf("關機中斷不應發出或刪除：Sent %d Deleted %d", len(e.fake.Sent), len(e.fake.Deleted))
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); len(got) != 0 {
		t.Errorf("關機中斷不應留下反應：%q", got)
	}
}

// 把關重點 2：多個網址中第一個失敗 → 第二個不必下載，什麼都不發。
func TestGate_Process_FirstLinkFailsNothingSent(t *testing.T) {
	e := newLinkEnv(t)
	e.urls.errs["https://a.com/1.png"] = ErrImageGone
	e.urls.data["https://a.com/2.png"] = pngBytes(t, 8, 8)
	m := e.linkMessage("文字 https://a.com/1.png https://a.com/2.png")
	if err := e.p.Process(context.Background(), testGuild, m); !errors.Is(err, ErrImageGone) {
		t.Fatalf("錯誤：%v", err)
	}
	if len(e.fake.Sent) != 0 || !e.originalExists(m) {
		t.Error("任一張失敗不應發出或刪除")
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) {
		t.Errorf("應只有 ❌：%q", got)
	}
}

// 把關重點 3：建立事件（尚無預覽）與多個並行的更新事件同時進來 → 只處理一輪、原訊息只刪一次。
func TestGate_HandleMessage_CreateAndManyUpdates_ProcessOnce(t *testing.T) {
	e := newLinkEnv(t)
	e.p.EmbedWait = 20 * time.Millisecond
	e.urls.data[noExtURL] = pngBytes(t, 8, 8)
	m := e.linkMessage("文字 " + noExtURL)
	fresh := *m
	fresh.Embeds = []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: noExtURL}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.p.HandleMessage(context.Background(), m)
	}()
	time.Sleep(2 * time.Millisecond)
	e.fake.ReplaceMessage(&fresh)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.p.HandleMessageUpdate(context.Background(), &discordgo.Message{ID: m.ID, ChannelID: m.ChannelID, Embeds: fresh.Embeds})
		}()
	}
	wg.Wait()
	// 晚到的更新（原訊息已刪除）也不可再處理。
	e.p.HandleMessageUpdate(context.Background(), &discordgo.Message{ID: m.ID, ChannelID: m.ChannelID, Embeds: fresh.Embeds})
	if len(e.fake.Sent) != 1 || e.fake.CallCount("ChannelMessageDelete") != 1 {
		t.Errorf("應只處理一輪：Sent %d Delete %d", len(e.fake.Sent), e.fake.CallCount("ChannelMessageDelete"))
	}
}

// 把關重點 3／5：部分更新（沒有作者、附件資訊）的只含網址訊息，若早於 url_since → 重讀後依網址規則判定為舊訊息，不處理。
func TestGate_HandleMessageUpdate_PartialLinkOnlyBeforeURLSince(t *testing.T) {
	e := newLinkEnv(t)
	e.urls.data[example2URL] = jpegBytes(t, 8, 8)
	m := e.linkMessage(example2)
	if err := e.p.Store.Update(context.Background(), func(d *store.Data) error { d.URLSince = "99999999999999999"; return nil }); err != nil {
		t.Fatal(err)
	}
	e.fake.Sent = nil
	e.p.HandleMessageUpdate(context.Background(), &discordgo.Message{ID: m.ID, ChannelID: m.ChannelID})
	time.Sleep(20 * time.Millisecond)
	if len(e.fake.Sent) != 0 || len(e.fake.Deleted) != 0 {
		t.Errorf("早於 url_since 的網址訊息不應處理：Sent %d Deleted %d", len(e.fake.Sent), len(e.fake.Deleted))
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); len(got) != 0 {
		t.Errorf("不應留下反應：%q", got)
	}
}

// 把關重點 3：等待預覽期間使用者把網址編輯掉 → 不處理、不留反應。
func TestGate_HandleMessage_LinkEditedAwayDuringWait(t *testing.T) {
	e := newLinkEnv(t)
	e.p.EmbedWait = 20 * time.Millisecond
	m := e.linkMessage("文字 " + noExtURL)
	done := make(chan struct{})
	go func() {
		e.p.HandleMessage(context.Background(), m)
		close(done)
	}()
	fresh := *m
	fresh.Content = "文字"
	e.fake.ReplaceMessage(&fresh)
	<-done
	e.noTrace(t, m)
}

// 把關重點 3：等待預覽期間原訊息被刪除 → 不處理。
func TestGate_HandleMessage_DeletedDuringWait(t *testing.T) {
	e := newLinkEnv(t)
	e.p.EmbedWait = 20 * time.Millisecond
	m := e.linkMessage("文字 " + noExtURL)
	done := make(chan struct{})
	go func() {
		e.p.HandleMessage(context.Background(), m)
		close(done)
	}()
	if err := e.fake.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
		t.Fatal(err)
	}
	e.fake.Deleted = nil
	<-done
	if len(e.fake.Sent) != 0 {
		t.Errorf("已刪除的訊息不應處理：Sent %d", len(e.fake.Sent))
	}
}

// 把關（記錄）：新訊息「附件＋gifv 網址」→ 處理附件、刪原訊息；gifv 網址留在關鍵字中（實作者暫定，PM 已接受）。
func TestGate_Process_AttachmentPlusGIFV(t *testing.T) {
	e := newLinkEnv(t)
	gifv := "https://tenor.com/view/abc"
	m := e.imageMessage(t, "a.png")
	m.Content = "文字 " + gifv
	m.Embeds = []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeGifv, URL: gifv, Thumbnail: &discordgo.MessageEmbedThumbnail{URL: "https://media.tenor.com/x.png"}}}
	e.fake.ReplaceMessage(m)
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 1 || e.originalExists(m) {
		t.Fatalf("應只發附件並刪原訊息：Sent %d", len(e.fake.Sent))
	}
	if !strings.Contains(e.fake.Sent[0].Data.Content, gifv) {
		t.Errorf("gifv 網址應留在關鍵字：%s", e.fake.Sent[0].Data.Content)
	}
	if len(e.urls.fetched()) != 0 {
		t.Error("gifv 不應下載")
	}
}

// 把關：上傳檔名只留安全字元，不可帶路徑。
func TestGate_LinkFilename(t *testing.T) {
	cases := []struct {
		url, format string
		spoiler     bool
		want        string
	}{
		{"https://a.com/../../etc/passwd.png", "png", false, "passwd.png"},
		{"https://a.com/a%2F..%2Fb.png", "png", false, "b.png"},
		{"https://a.com/%E5%9C%96.png", "png", false, "image.png"},
		{"https://a.com/x.png?name=../../evil.exe", "png", false, "x.png"},
		{"https://a.com/", "jpeg", false, "image.jpg"},
		{"https://a.com/.png", "png", false, "image.png"},
		{"https://a.com/x.jpg", "png", false, "image.png"},
		{"https://a.com/x.JPEG", "jpeg", false, "x.JPEG"},
		{"https://a.com/x.webp", "webp", true, "SPOILER_x.webp"},
		{"https://a.com/a b<>:\"|.gif", "gif", false, "ab.gif"},
		{"https://a.com/" + strings.Repeat("a", 200) + ".png", "png", false, strings.Repeat("a", 64) + ".png"},
	}
	for _, c := range cases {
		got := linkFilename(recognize.ImageSource{URL: c.url, FetchURL: c.url, Spoiler: c.spoiler}, c.format)
		if got != c.want {
			t.Errorf("%s（%s）：%q，預期 %q", c.url, c.format, got, c.want)
		}
		if strings.ContainsAny(got, "/\\") {
			t.Errorf("%s：檔名不可含路徑分隔：%q", c.url, got)
		}
	}
}
