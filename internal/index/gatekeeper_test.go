package index

import (
	"context"
	"testing"

	"discordbot/internal/store"
)

// 把關者補充：READY 掃描使用的設定快照過期（快照後頻道被移除又重新加入，設定時間點變新）時，
// 不應以舊的設定時間點掃描並取代 ChannelAdded 的掃描：舊 setAt 與新 setAt 之間的訊息應當舊圖片訊息，不補處理。
// （階段 7 後的決定 (a)：startScan 在 h.mu 內重新讀目前設定的 setAt。）
func TestGK_StaleReadySnapshotDoesNotOverrideNewerSetAt(t *testing.T) {
	f, st, x, bf, h := newHooksEnv(t)
	addUserImage(f, "c", "300", "舊設定之後、新設定之前")
	addSource(t, st, "g", "c", "200")
	stale := st.Snapshot() // READY 當下的快照
	if err := st.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel("g", "c"); return nil }); err != nil {
		t.Fatal(err)
	}
	h.ChannelRemoved("c")
	addSource(t, st, "g", "c", "400")
	h.ChannelAdded("g", "c")
	h.ScanAll(stale)
	h.Wait()
	if got := bf.got(); len(got) != 0 {
		t.Errorf("新設定時間點之前的訊息不應補處理：%v", got)
	}
	if e, ok := x.Get(Key{ChannelID: "c", MessageID: "300"}); !ok || e.Kind != KindLegacy {
		t.Errorf("應收錄為舊圖片訊息：%+v（%v）", e, ok)
	}
}

// 把關者補充：READY 快照之後頻道已被移除，ScanAll 不應寫入索引（sourceWriter 確認來源）。
func TestGK_StaleReadySnapshotRemovedChannelNotIndexed(t *testing.T) {
	f, st, x, _, h := newHooksEnv(t)
	addUserImage(f, "c", "100", "舊圖")
	addSource(t, st, "g", "c", "200")
	stale := st.Snapshot()
	if err := st.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel("g", "c"); return nil }); err != nil {
		t.Fatal(err)
	}
	h.ChannelRemoved("c")
	h.ScanAll(stale)
	h.Wait()
	if got := x.Search("", allow("c"), 0); len(got) != 0 {
		t.Errorf("已移除的頻道不應被寫入索引：%v", keys(got))
	}
}
