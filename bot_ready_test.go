package main

import (
	"sync"
	"testing"
	"time"

	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

type recordScanner struct {
	mu    sync.Mutex
	botID string
	scans []string // 每次 ScanAll 時的 botID
	data  []*store.Data
	done  chan struct{}
}

func (r *recordScanner) SetBotID(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.botID = id
}

func (r *recordScanner) ScanAll(d *store.Data) {
	r.mu.Lock()
	r.scans = append(r.scans, r.botID)
	r.data = append(r.data, d)
	r.mu.Unlock()
	r.done <- struct{}{}
}

// 階段 6 後決定 6：每次收到 READY（首次啟動與重新 IDENTIFY）都設定 bot ID 並重新掃描所有來源頻道；
// 掃描用收到 READY 當下的設定。啟動掃描只由 READY 觸發，不會重複。
func TestOnReady_ScansAllEachReady(t *testing.T) {
	_, st := newLoadedStore(t, "g", "c1")
	r := &recordScanner{done: make(chan struct{}, 2)}
	h := onReady(r, st)
	for i := 0; i < 2; i++ {
		h(nil, &discordgo.Ready{User: &discordgo.User{ID: "bot"}})
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
			t.Fatalf("第 %d 次 READY 沒有掃描", i+1)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.scans) != 2 || r.scans[0] != "bot" || r.scans[1] != "bot" {
		t.Errorf("掃描前應先設定 bot ID：%v", r.scans)
	}
	if _, _, ok := r.data[0].SourceChannel("c1"); !ok {
		t.Error("應掃描目前設定中的來源頻道")
	}
}

// RESUMED 不是 READY，不觸發（discordgo 對 RESUMED 送的是 *discordgo.Resumed 事件，onReady 不會收到）；
// READY 沒有使用者資料時仍以既有 bot ID 掃描。
func TestOnReady_NilUserStillScans(t *testing.T) {
	_, st := newLoadedStore(t, "g", "c1")
	r := &recordScanner{done: make(chan struct{}, 1), botID: "old"}
	onReady(r, st)(nil, &discordgo.Ready{})
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("沒有掃描")
	}
	if r.scans[0] != "old" {
		t.Errorf("bot ID 不應被清空：%v", r.scans)
	}
}
