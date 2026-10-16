package pipeline

import (
	"context"
	"slices"
	"testing"
	"time"

	"discordbot/internal/index"
)

// 把關（階段 6）：啟動掃描交給補處理的訊息，在掃描結束後輪到時仍應被處理。
// 掃描 goroutine 結束時會 cancel 掃描用的 ctx；若補處理沿用該 ctx，輪到時 run 會當成關機而放棄。
func TestGate6_ScanAllBackfillIsProcessed(t *testing.T) {
	e := newEnv(t) // 來源頻道 testSource，setAt "1"
	m := e.imageMessage(t, "backfill.png")
	idx := index.New()
	h := index.NewHooks(context.Background(), e.fake, idx, e.p, e.p.Store)
	h.SetBotID("bot")
	h.ScanAll(e.p.Store.Snapshot())
	waitUntil(t, "補處理完成（原訊息被刪除或不再有 ⏳）", func() bool {
		return !e.originalExists(m) || !slices.Contains(e.fake.ReactionsOn(testSource, m.ID), "⏳")
	})
	if len(e.fake.Sent) != 1 {
		t.Errorf("設定時間點之後、未處理的訊息應被補處理並發出 1 則圖片訊息，Sent=%d；原訊息仍在=%v，反應=%q",
			len(e.fake.Sent), e.originalExists(m), e.fake.ReactionsOn(testSource, m.ID))
	}
}

// 把關：即時 messageCreate 處理中，啟動掃描也讀到同一則 → 只處理一次、不加 ❌。
func TestGate6_LiveAndScanSameMessageProcessedOnce(t *testing.T) {
	e := newEnv(t)
	text, entered, release := blockingText()
	e.p.Text = text
	m := e.imageMessage(t, "same.png")
	live := e.p.Enqueue(context.Background(), testGuild, m)
	<-entered
	h := index.NewHooks(context.Background(), e.fake, index.New(), e.p, e.p.Store)
	h.SetBotID("bot")
	h.ScanAll(e.p.Store.Snapshot())
	close(release)
	if err := <-live; err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // 讓可能重複排入的補處理跑完
	if len(e.fake.Sent) != 1 {
		t.Errorf("同一則只應處理一次，Sent=%d", len(e.fake.Sent))
	}
	if slices.Contains(e.fake.ReactionsOn(testSource, m.ID), "❌") {
		t.Error("不應加 ❌")
	}
}

// 把關：佇列 goroutine 剛好結束時加入的新項目不會遺失（反覆「排一則、等完成、立即再排」）。
func TestGate6_QueueRestartNoLostJob(t *testing.T) {
	e := newEnv(t)
	const n = 200
	for i := 0; i < n; i++ {
		m := e.imageMessage(t, "x.png")
		select {
		case err := <-e.p.Enqueue(context.Background(), testGuild, m):
			if err != nil {
				t.Fatalf("第 %d 則：%v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("第 %d 則逾時：佇列項目遺失", i)
		}
	}
	if len(e.fake.Sent) != n {
		t.Errorf("Sent=%d，預期 %d", len(e.fake.Sent), n)
	}
}
