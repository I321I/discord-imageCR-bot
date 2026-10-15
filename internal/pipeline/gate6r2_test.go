package pipeline

import (
	"context"
	"slices"
	"testing"
	"time"

	"discordbot/internal/index"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// 把關（階段 6 第二輪）：掃描交給補處理、仍在佇列中的訊息，關機（Hooks 的 ctx 取消）時被放棄：
// 不下載、不發、不刪，移除 ⏳ 且不加 ❌。
func TestGate6_ShutdownAbandonsQueuedBackfill(t *testing.T) {
	e := newEnv(t)
	text, entered, release := blockingText()
	e.p.Text = text
	live := e.imageMessage(t, "live.png")
	queued := e.imageMessage(t, "queued.png")
	d := e.p.Enqueue(context.Background(), testGuild, live) // 即時訊息用不會取消的 ctx，佔住佇列
	<-entered

	shutdownCtx, shutdown := context.WithCancel(context.Background())
	h := index.NewHooks(shutdownCtx, e.fake, index.New(), e.p, e.p.Store)
	h.SetBotID("bot")
	h.ScanAll(e.p.Store.Snapshot())
	waitUntil(t, "補處理的訊息加 ⏳", func() bool {
		return slices.Contains(e.fake.ReactionsOn(testSource, queued.ID), "⏳")
	})

	shutdown()
	close(release)
	if err := <-d; err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "補處理的訊息移除 ⏳", func() bool {
		return len(e.fake.ReactionsOn(testSource, queued.ID)) == 0
	})
	time.Sleep(50 * time.Millisecond)
	if slices.Contains(e.fetch.fetched(), queued.Attachments[0].URL) {
		t.Error("關機後補處理不應下載")
	}
	if !e.originalExists(queued) {
		t.Error("關機後補處理不應刪除原訊息")
	}
	if len(e.fake.Sent) != 1 {
		t.Errorf("只有佔住佇列的即時訊息應發出，Sent=%d", len(e.fake.Sent))
	}
	if got := e.fake.ReactionsOn(testSource, queued.ID); len(got) != 0 {
		t.Errorf("不應加 ❌，反應=%q", got)
	}
}

// 把關：掃描交給補處理、仍在佇列中時頻道被移除（設定先移除，再 ChannelRemoved）：
// 補處理被放棄；處理中的那則發出的圖片訊息經由 Hooks 也不寫入索引。
func TestGate6_ChannelRemovedAbandonsQueuedBackfill(t *testing.T) {
	e := newEnv(t)
	text, entered, release := blockingText()
	e.p.Text = text
	idx := index.New()
	h := index.NewHooks(context.Background(), e.fake, idx, e.p, e.p.Store)
	h.SetBotID("bot")
	e.p.Index = h

	live := e.imageMessage(t, "live.png")
	queued := e.imageMessage(t, "queued.png")
	d := e.p.Enqueue(context.Background(), testGuild, live)
	<-entered
	h.ScanAll(e.p.Store.Snapshot())
	waitUntil(t, "補處理的訊息加 ⏳", func() bool {
		return slices.Contains(e.fake.ReactionsOn(testSource, queued.ID), "⏳")
	})

	if err := e.p.Store.Update(context.Background(), func(d *store.Data) error {
		d.RemoveChannel(testGuild, testSource)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.ChannelRemoved(testSource)
	close(release)
	<-d
	waitUntil(t, "補處理的訊息移除 ⏳", func() bool {
		return len(e.fake.ReactionsOn(testSource, queued.ID)) == 0
	})
	time.Sleep(50 * time.Millisecond)
	if slices.Contains(e.fetch.fetched(), queued.Attachments[0].URL) {
		t.Error("頻道移除後補處理不應下載")
	}
	if !e.originalExists(queued) {
		t.Error("頻道移除後補處理不應刪除原訊息")
	}
	if got := idx.Search("", map[string]bool{testSource: true}, 0); len(got) != 0 {
		t.Errorf("頻道移除後索引不應有項目，得到 %d 筆", len(got))
	}
}

// 把關：真正的 Pipeline 當補處理者，同一頻道反覆新增/移除與即時訊息並發，不死結。
func TestGate6_PipelineAndHooksConcurrentNoDeadlock(t *testing.T) {
	e := newEnv(t)
	h := index.NewHooks(context.Background(), e.fake, index.New(), e.p, e.p.Store)
	h.SetBotID("bot")
	e.p.Index = h
	for i := 0; i < 20; i++ {
		e.imageMessage(t, "x.png")
	}
	var live []*discordgo.Message // 先建立：imageMessage 寫 fetch.data 不加鎖
	for i := 0; i < 10; i++ {
		live = append(live, e.imageMessage(t, "y.png"))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		finished := make(chan struct{})
		go func() {
			for i := 0; i < 10; i++ {
				_ = e.p.Store.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel(testGuild, testSource); return nil })
				h.ChannelRemoved(testSource)
				_ = e.p.Store.Update(context.Background(), func(d *store.Data) error { d.AddChannel(testGuild, testSource, "1"); return nil })
				h.ChannelAdded(testGuild, testSource)
			}
			close(finished)
		}()
		for _, m := range live {
			e.p.HandleMessage(context.Background(), m)
		}
		<-finished
		h.Wait()
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("60 秒未結束：疑似死結")
	}
}
