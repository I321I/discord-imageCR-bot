package index

import (
	"context"
	"testing"

	"discordbot/internal/store"
)

// READY 快照之後頻道已被移除：ScanAll 不應開始掃描該頻道（連歷史都不讀）。
func TestScanAll_SkipsChannelNoLongerSource(t *testing.T) {
	f, st, _, _, h := newHooksEnv(t)
	addUserImage(f, "c", "100", "舊圖")
	addSource(t, st, "g", "c", "200")
	stale := st.Snapshot()
	if err := st.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel("g", "c"); return nil }); err != nil {
		t.Fatal(err)
	}
	h.ChannelRemoved("c")
	h.ScanAll(stale)
	h.Wait()
	if n := f.CallCount("ChannelMessages"); n != 0 {
		t.Errorf("已非來源的頻道不應掃描，讀取歷史 %d 次", n)
	}
}

// BotID 回傳 SetBotID 設定的值（右鍵指令用來判斷是不是 bot 發的圖片訊息）。
func TestHooks_BotID(t *testing.T) {
	_, _, _, _, h := newHooksEnv(t)
	h.SetBotID("me")
	if got := h.BotID(); got != "me" {
		t.Errorf("得到 %q", got)
	}
}
