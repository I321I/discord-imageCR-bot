package commands

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 不同伺服器的 GuildRemoved / PruneMissingChannels 並行執行：不互相影響、無 data race。
type lockedHooks struct {
	NopIndexHooks
	mu      sync.Mutex
	removed []string
}

func (h *lockedHooks) ChannelRemoved(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removed = append(h.removed, id)
}

func TestGate3_GuildEventsConcurrent(t *testing.T) {
	e := newChannelEnv(t)
	hooks := &lockedHooks{}
	addSources(t, e.st, "g1", "a1", "a2")
	addSources(t, e.st, "g2", "b1", "b2")
	addSources(t, e.st, "g3", "k1")
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); GuildRemoved(context.Background(), e.st, hooks, "g1") }()
	go func() {
		defer wg.Done()
		PruneMissingChannels(context.Background(), e.st, hooks, "g2", []string{"b1"}, time.Now())
	}()
	go func() {
		defer wg.Done()
		PruneMissingChannels(context.Background(), e.st, hooks, "g3", []string{"k1"}, time.Now())
	}()
	wg.Wait()
	d := e.st.Snapshot()
	if len(d.Channels("g1")) != 0 || len(d.Channels("g2")) != 1 || len(d.Channels("g3")) != 1 {
		t.Errorf("結果不符：g1=%v g2=%v g3=%v", d.Channels("g1"), d.Channels("g2"), d.Channels("g3"))
	}
	if len(hooks.removed) != 3 {
		t.Errorf("應移除 a1、a2、b2：%v", hooks.removed)
	}
}

// 記錄行為：PruneMissingChannels 收到空清單時會清掉該伺服器所有來源頻道
// （GUILD_CREATE 頻道清單為空時由 onGuildCreate 跳過，不會呼叫到這裡）。
func TestGate3_PruneEmptyChannelListRemovesAll(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "g1", "c1", "c2")
	PruneMissingChannels(context.Background(), e.st, e.hooks, "g1", []string{}, time.Now())
	if n := len(e.st.Snapshot().Channels("g1")); n != 0 {
		t.Errorf("現行行為應全部移除，剩 %d", n)
	}
}
