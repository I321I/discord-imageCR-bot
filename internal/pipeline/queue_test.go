package pipeline

import (
	"context"
	"fmt"
	"image"
	"slices"
	"sync"
	"testing"
	"time"

	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// blockingText 讓第一次 OCR 阻塞到 release 關閉，並通知已開始。
func blockingText() (textFunc, chan struct{}, chan struct{}) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	return textFunc(func(ctx context.Context, _ image.Image) (string, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "這就是人類啊", nil
	}), entered, release
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待逾時：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// 決定 7：一次只處理一則訊息；排隊中的訊息輪到時才下載。
func TestQueue_OneMessageAtATime(t *testing.T) {
	e := newEnv(t)
	text, entered, release := blockingText()
	e.p.Text = text
	m1 := e.imageMessage(t, "a.png")
	m2 := e.imageMessage(t, "b.png")
	d1 := e.p.Enqueue(context.Background(), testGuild, m1)
	<-entered
	d2 := e.p.Enqueue(context.Background(), testGuild, m2)
	waitUntil(t, "第二則加 ⏳", func() bool { return slices.Contains(e.fake.ReactionsOn(testSource, m2.ID), "⏳") })
	time.Sleep(50 * time.Millisecond)
	if slices.Contains(e.fetch.fetched(), m2.Attachments[0].URL) {
		t.Error("第一則處理中時第二則不應下載")
	}
	close(release)
	for _, d := range []<-chan error{d1, d2} {
		if err := <-d; err != nil {
			t.Fatal(err)
		}
	}
	if len(e.fake.Sent) != 2 || len(e.fake.Deleted) != 2 {
		t.Errorf("兩則都應完成：Sent %d、Deleted %d", len(e.fake.Sent), len(e.fake.Deleted))
	}
}

// 決定 9：多則訊息依原訊息順序（ID）發出，即使加入佇列的順序顛倒。
func TestQueue_SendsInOriginalMessageOrder(t *testing.T) {
	e := newEnv(t)
	text, entered, release := blockingText()
	e.p.Text = text
	first := e.imageMessage(t, "first.png")
	var msgs []*discordgo.Message
	for i := 0; i < 4; i++ {
		msgs = append(msgs, e.imageMessage(t, fmt.Sprintf("m%d.png", i)))
	}
	dones := []<-chan error{e.p.Enqueue(context.Background(), testGuild, first)}
	<-entered
	for i := len(msgs) - 1; i >= 0; i-- { // 倒序加入
		dones = append(dones, e.p.Enqueue(context.Background(), testGuild, msgs[i]))
	}
	close(release)
	for _, d := range dones {
		if err := <-d; err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, s := range e.fake.Sent {
		got = append(got, s.Data.Files[0].Name)
	}
	want := []string{"first.png", "m0.png", "m1.png", "m2.png", "m3.png"}
	if !slices.Equal(got, want) {
		t.Errorf("發出順序 %v，預期 %v", got, want)
	}
}

// 排序以 snowflake 數值比較（位數不同）。
func TestQueue_OrdersBySnowflakeNumerically(t *testing.T) {
	e := newEnv(t)
	text, entered, release := blockingText()
	e.p.Text = text
	block := e.imageMessage(t, "block.png")
	small := &discordgo.Message{ID: "9", ChannelID: testSource, Author: &discordgo.User{ID: testUser}}
	big := &discordgo.Message{ID: "10", ChannelID: testSource, Author: &discordgo.User{ID: testUser}}
	for _, m := range []*discordgo.Message{big, small} {
		url := "https://cdn.test/" + m.ID
		data := pngBytes(t, 8, 8)
		e.fetch.data[url] = data
		m.Attachments = []*discordgo.MessageAttachment{{Filename: m.ID + ".png", ContentType: "image/png", URL: url, Size: len(data)}}
		e.fake.AddMessage(m)
	}
	d0 := e.p.Enqueue(context.Background(), testGuild, block)
	<-entered
	dBig := e.p.Enqueue(context.Background(), testGuild, big)
	dSmall := e.p.Enqueue(context.Background(), testGuild, small)
	close(release)
	<-d0
	<-dBig
	<-dSmall
	if len(e.fake.Sent) != 3 || e.fake.Sent[1].Data.Files[0].Name != "9.png" || e.fake.Sent[2].Data.Files[0].Name != "10.png" {
		var got []string
		for _, s := range e.fake.Sent {
			got = append(got, s.Data.Files[0].Name)
		}
		t.Errorf("應依數值 9 < 10 發出，得到 %v", got)
	}
}

// 關機時排隊中的訊息直接放棄：不下載、不發、不刪、不加 ❌，移除 ⏳。
func TestQueue_ShutdownAbandonsQueued(t *testing.T) {
	e := newEnv(t)
	text, entered, _ := blockingText()
	e.p.Text = text
	ctx, cancel := context.WithCancel(context.Background())
	m1 := e.imageMessage(t, "a.png")
	m2 := e.imageMessage(t, "b.png")
	d1 := e.p.Enqueue(ctx, testGuild, m1)
	<-entered
	d2 := e.p.Enqueue(ctx, testGuild, m2)
	waitUntil(t, "第二則加 ⏳", func() bool { return slices.Contains(e.fake.ReactionsOn(testSource, m2.ID), "⏳") })
	cancel()
	for _, d := range []<-chan error{d1, d2} {
		select {
		case <-d:
		case <-time.After(5 * time.Second):
			t.Fatal("關機後沒有返回")
		}
	}
	if slices.Contains(e.fetch.fetched(), m2.Attachments[0].URL) {
		t.Error("排隊中的訊息不應下載")
	}
	if len(e.fake.Sent) != 0 || len(e.fake.Deleted) != 0 {
		t.Errorf("關機時不發不刪：Sent %d、Deleted %d", len(e.fake.Sent), len(e.fake.Deleted))
	}
	for _, m := range []*discordgo.Message{m1, m2} {
		if got := e.fake.ReactionsOn(testSource, m.ID); len(got) != 0 {
			t.Errorf("訊息 %s 應移除 ⏳ 且不加 ❌，得到 %q", m.ID, got)
		}
	}
}

// 同一則訊息已在佇列或處理中時再次加入：略過（不重複處理）。
func TestQueue_DuplicateIgnored(t *testing.T) {
	e := newEnv(t)
	text, entered, release := blockingText()
	e.p.Text = text
	m := e.imageMessage(t, "a.png")
	d1 := e.p.Enqueue(context.Background(), testGuild, m)
	<-entered
	d2 := e.p.Enqueue(context.Background(), testGuild, m)
	if err := <-d2; err != nil {
		t.Errorf("重複加入應直接返回 nil，得到 %v", err)
	}
	close(release)
	if err := <-d1; err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 1 {
		t.Errorf("同一則只應處理一次，Sent %d", len(e.fake.Sent))
	}
}

// 輪到時頻道已不是來源頻道：放棄（移除 ⏳，不下載不發）。
func TestQueue_ChannelRemovedWhileQueued(t *testing.T) {
	e := newEnv(t)
	text, entered, release := blockingText()
	e.p.Text = text
	m1 := e.imageMessage(t, "a.png")
	m2 := e.imageMessage(t, "b.png")
	m2.ChannelID = "src2"
	e.fake.AddMessage(m2)
	if err := e.p.Store.Update(context.Background(), func(d *store.Data) error { d.AddChannel(testGuild, "src2", "1"); return nil }); err != nil {
		t.Fatal(err)
	}
	d1 := e.p.Enqueue(context.Background(), testGuild, m1)
	<-entered
	d2 := e.p.Enqueue(context.Background(), testGuild, m2)
	if err := e.p.Store.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel(testGuild, "src2"); return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-d1
	<-d2
	if slices.Contains(e.fetch.fetched(), m2.Attachments[0].URL) {
		t.Error("頻道已移除時不應下載")
	}
	if got := e.fake.ReactionsOn("src2", m2.ID); len(got) != 0 {
		t.Errorf("應移除 ⏳，得到 %q", got)
	}
}

// 補處理：輪到時重新取訊息；已被刪除或帶 bot 的 ❌ → 略過。
func TestQueue_BackfillRefetch(t *testing.T) {
	e := newEnv(t)
	deleted := e.imageMessage(t, "deleted.png")
	failed := e.imageMessage(t, "failed.png")
	ok := e.imageMessage(t, "ok.png")
	if err := e.fake.ChannelMessageDelete(testSource, deleted.ID); err != nil {
		t.Fatal(err)
	}
	e.fake.Deleted = nil
	failed.Reactions = []*discordgo.MessageReactions{{Count: 1, Me: true, Emoji: &discordgo.Emoji{Name: "❌"}}}
	var dones []<-chan error
	for _, m := range []*discordgo.Message{deleted, failed, ok} {
		dones = append(dones, e.p.EnqueueBackfill(context.Background(), testGuild, m))
	}
	for _, d := range dones {
		<-d
	}
	if len(e.fake.Sent) != 1 || e.fake.Sent[0].Data.Files[0].Name != "ok.png" {
		t.Errorf("只應處理仍存在且沒有 ❌ 的訊息，Sent %d", len(e.fake.Sent))
	}
	for _, m := range []*discordgo.Message{deleted, failed} {
		if slices.Contains(e.fetch.fetched(), m.Attachments[0].URL) {
			t.Errorf("%s 不應下載", m.Attachments[0].Filename)
		}
		if got := e.fake.ReactionsOn(testSource, m.ID); slices.Contains(got, "⏳") {
			t.Errorf("%s 略過時應移除 ⏳，得到 %q", m.Attachments[0].Filename, got)
		}
	}
}
