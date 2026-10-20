package index

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// 把關第二輪：重讀（部分更新）、單則刪除、批次刪除、頻道移除、bot 圖片訊息更新同時大量發生時，
// 不可死結（putMu → refetchMu 鎖順序），結束後 refetching 不可殘留登記（token 洩漏）。
// 部分訊息不在 Fake 中（重讀回 404）也要清除登記。
func TestGateR2_RefetchDeleteStress_NoDeadlockNoLeak(t *testing.T) {
	f, st, x, _, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "200")
	addSource(t, st, "g", "c2", "200")
	if err := st.Update(context.Background(), func(d *store.Data) error { d.URLSince = "300"; return nil }); err != nil {
		t.Fatal(err)
	}
	const n = 40
	for i := 0; i < n; i++ {
		if i%4 != 3 { // 每 4 則有 1 則不在 Fake 中（重讀 404）
			addUserLink(f, "c", fmt.Sprint(250-i), fmt.Sprintf("文字%d\n%s", i, example1URL))
		}
	}
	var wg sync.WaitGroup
	for round := 0; round < 5; round++ {
		for i := 0; i < n; i++ {
			id := fmt.Sprint(250 - i)
			wg.Add(3)
			go func() {
				defer wg.Done()
				h.MessageUpdated(&discordgo.Message{ID: id, ChannelID: "c", Content: "x " + example1URL, Author: &discordgo.User{ID: "u1"}})
			}()
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					h.MessageDeleted("c", id)
				} else {
					h.MessagesDeleted("c", []string{id, "999"})
				}
			}()
			go func() {
				defer wg.Done()
				h.MessageUpdated(&discordgo.Message{ID: "1" + id, ChannelID: "c2", Author: &discordgo.User{ID: botID}, Content: "x"})
				h.ChannelRemoved("c3") // 不是來源：只取 putMu 的路徑
			}()
		}
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("疑似死結：30 秒內未完成")
	}
	h.refetchMu.Lock()
	left := len(h.refetching)
	h.refetchMu.Unlock()
	if left != 0 {
		t.Errorf("重讀登記殘留 %d 筆", left)
	}
	_ = x
}

// 把關第二輪：同一則訊息兩個重讀同時進行，刪除時兩個都標記；刪除之後才開始的重讀不受影響（不沿用舊標記）。
func TestGateR2_RefetchTokens_PerRefetch(t *testing.T) {
	_, st, _, _, h := newHooksEnv(t)
	_ = st
	t1, done1 := h.startRefetch("c", "1")
	t2, done2 := h.startRefetch("c", "1")
	h.MessageDeleted("c", "1")
	t3, done3 := h.startRefetch("c", "1")
	h.refetchMu.Lock()
	d1, d2, d3 := t1.deleted, t2.deleted, t3.deleted
	h.refetchMu.Unlock()
	if !d1 || !d2 || d3 {
		t.Errorf("標記錯誤：%v %v %v（預期 true true false）", d1, d2, d3)
	}
	done2()
	done1()
	done3()
	done3() // 重複取消登記不可 panic 或誤刪
	h.refetchMu.Lock()
	defer h.refetchMu.Unlock()
	if len(h.refetching) != 0 {
		t.Errorf("登記殘留：%v", h.refetching)
	}
}
