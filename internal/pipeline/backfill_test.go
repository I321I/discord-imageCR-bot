package pipeline

import (
	"context"
	"testing"

	"discordbot/internal/index"
)

// /channel add 觸發的掃描交給補處理的訊息，掃描結束後輪到時仍會真的處理（發出並刪除原訊息）。
func TestBackfill_ChannelAddedScanIsProcessed(t *testing.T) {
	e := newEnv(t) // 來源頻道 testSource，setAt "1"
	m := e.imageMessage(t, "added.png")
	h := index.NewHooks(context.Background(), e.fake, index.New(), e.p, e.p.Store)
	h.SetBotID("bot")
	h.ChannelAdded(testGuild, testSource)
	h.Wait()
	waitUntil(t, "補處理發出圖片訊息並刪除原訊息", func() bool {
		return e.fake.CallCount("ChannelMessageSendComplex") == 1 && !e.originalExists(m)
	})
}
