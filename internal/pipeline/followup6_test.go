package pipeline

import (
	"context"
	"slices"
	"testing"
	"time"

	"discordbot/internal/store"
)

// 階段 6 後決定 1：補處理排隊期間頻道被移除 → 輪到時放棄，移除 ⏳，原訊息不動（不刪、不加 ❌、不發）。
func TestQueue_ChannelRemovedWhileQueued_BackfillLeavesOriginal(t *testing.T) {
	e := newEnv(t)
	text, entered, release := blockingText()
	e.p.Text = text
	busy := e.imageMessage(t, "busy.png")
	queued := e.imageMessage(t, "queued.png")
	d1 := e.p.Enqueue(context.Background(), testGuild, busy)
	<-entered
	d2 := e.p.EnqueueBackfill(context.Background(), testGuild, queued)
	if err := e.p.Store.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel(testGuild, testSource); return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-d1
	if err := <-d2; err != nil {
		t.Errorf("放棄不算錯誤：%v", err)
	}
	if !e.originalExists(queued) {
		t.Error("原訊息不應被刪除")
	}
	if slices.Contains(e.fetch.fetched(), queued.Attachments[0].URL) {
		t.Error("不應下載")
	}
	if got := e.fake.ReactionsOn(testSource, queued.ID); len(got) != 0 {
		t.Errorf("應只移除 ⏳、不加 ❌，反應=%q", got)
	}
	for _, s := range e.fake.Sent {
		if s.ChannelID == testSource && len(s.Data.Files) > 0 && s.Data.Files[0].Name == "queued.png" {
			t.Error("不應發出圖片訊息")
		}
	}
}

// 階段 6 後決定 5：⏳ 在排入佇列時就加（即時訊息與啟動補處理皆同），不等輪到。
func TestQueue_HourglassAddedWhenQueued(t *testing.T) {
	e := newEnv(t)
	text, entered, release := blockingText()
	e.p.Text = text
	busy := e.imageMessage(t, "busy.png")
	live := e.imageMessage(t, "live.png")
	backfill := e.imageMessage(t, "backfill.png")
	d0 := e.p.Enqueue(context.Background(), testGuild, busy)
	<-entered
	d1 := e.p.Enqueue(context.Background(), testGuild, live)
	d2 := e.p.EnqueueBackfill(context.Background(), testGuild, backfill)
	// Enqueue 返回時 ⏳ 已加上（不需等待）。
	for _, m := range []string{live.ID, backfill.ID} {
		if got := e.fake.ReactionsOn(testSource, m); !slices.Equal(got, []string{"⏳"}) {
			t.Errorf("訊息 %s 排入佇列時應已有 ⏳，得到 %q", m, got)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if n := len(e.fetch.fetched()); n != 1 {
		t.Errorf("排隊中的訊息不應下載，已下載 %d 張", n)
	}
	close(release)
	for _, d := range []<-chan error{d0, d1, d2} {
		if err := <-d; err != nil {
			t.Fatal(err)
		}
	}
}
