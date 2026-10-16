package main

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

type recordScanner struct {
	mu      sync.Mutex
	botID   string
	scans   []string // 每次 ScanAll 時的 botID
	data    []*store.Data
	removed []string // ChannelRemoved 的頻道
	done    chan struct{}
}

func (r *recordScanner) ChannelAdded(guildID, channelID string) {}

func (r *recordScanner) ChannelRemoved(channelID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, channelID)
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
		// 階段 11 決定 1 之後 READY 沒列出的伺服器會被清除，所以補上伺服器 g。
		h(nil, &discordgo.Ready{User: &discordgo.User{ID: "bot"}, Guilds: []*discordgo.Guild{{ID: "g"}}})
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
	onReady(r, st)(nil, &discordgo.Ready{Guilds: []*discordgo.Guild{{ID: "g"}}}) // 階段 11 決定 1：補上伺服器 g
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("沒有掃描")
	}
	if r.scans[0] != "old" {
		t.Errorf("bot ID 不應被清空：%v", r.scans)
	}
}

// 階段 11 的使用者決定 1：READY 時清除 bot 已不在的伺服器的來源頻道（暫時無法使用的伺服器算仍在），
// 先清除再掃描（掃描用清除後的設定），並對被移除的頻道通知索引。
func TestOnReady_PrunesAbsentGuildsBeforeScan(t *testing.T) {
	f, st := newLoadedStore(t, "g1", "c1")
	if err := st.Update(context.Background(), func(d *store.Data) error {
		d.AddChannel("g2", "c2", "1")
		d.AddChannel("g3", "c3", "1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sent := len(f.Sent)
	r := &recordScanner{done: make(chan struct{}, 1)}
	onReady(r, st)(nil, &discordgo.Ready{Guilds: []*discordgo.Guild{{ID: "g1"}, {ID: "g2", Unavailable: true}}})
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("沒有掃描")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	scanned := r.data[0]
	if _, _, ok := scanned.SourceChannel("c3"); ok {
		t.Error("掃描應在清除之後，不應包含已不在的伺服器的頻道 c3")
	}
	for _, c := range []string{"c1", "c2"} {
		if _, _, ok := scanned.SourceChannel(c); !ok {
			t.Errorf("仍在（含暫時無法使用）的伺服器的頻道 %s 不應被清除", c)
		}
	}
	if !slices.Equal(r.removed, []string{"c3"}) {
		t.Errorf("應對 c3 呼叫 ChannelRemoved：%v", r.removed)
	}
	if len(f.Sent) != sent+1 {
		t.Errorf("應寫回儲存頻道一次，得到 %d 次", len(f.Sent)-sent)
	}
}

// 所有來源伺服器都還在：不寫入。
func TestOnReady_NoAbsentGuildDoesNotWrite(t *testing.T) {
	f, st := newLoadedStore(t, "g1", "c1")
	sent := len(f.Sent)
	r := &recordScanner{done: make(chan struct{}, 1)}
	onReady(r, st)(nil, &discordgo.Ready{Guilds: []*discordgo.Guild{{ID: "g1"}}})
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("沒有掃描")
	}
	if len(f.Sent) != sent {
		t.Errorf("沒有變動時不應寫入，寫了 %d 次", len(f.Sent)-sent)
	}
}

// PM 決定（比照 GUILD_CREATE 頻道清單為空就跳過）：READY 的伺服器清單為空時不清除任何來源頻道，仍照常掃描。
func TestOnReady_EmptyGuildListSkipsPrune(t *testing.T) {
	f, st := newLoadedStore(t, "g1", "c1")
	sent := len(f.Sent)
	r := &recordScanner{done: make(chan struct{}, 1)}
	onReady(r, st)(nil, &discordgo.Ready{})
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("沒有掃描")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, _, ok := st.Snapshot().SourceChannel("c1"); !ok {
		t.Error("伺服器清單為空時不應清除來源頻道")
	}
	if _, _, ok := r.data[0].SourceChannel("c1"); !ok {
		t.Error("應照常掃描來源頻道 c1")
	}
	if len(f.Sent) != sent || len(r.removed) != 0 {
		t.Errorf("不應寫入或呼叫 ChannelRemoved：寫入 %d 次、removed %v", len(f.Sent)-sent, r.removed)
	}
}
