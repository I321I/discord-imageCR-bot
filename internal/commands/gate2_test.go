package commands

import (
	"context"
	"sync"
	"testing"

	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// 把關者第二輪：add / remove / ChannelDeleted 並行時不 race、不死結，
// 且最後記憶體與儲存頻道一致、hook 呼叫與實際結果相符。

type safeHooks struct {
	mu      sync.Mutex
	added   int
	removed int
}

func (h *safeHooks) ChannelAdded(string, string) { h.mu.Lock(); h.added++; h.mu.Unlock() }
func (h *safeHooks) ChannelRemoved(string)       { h.mu.Lock(); h.removed++; h.mu.Unlock() }

func TestGate2_AddRemoveDeleteConcurrent(t *testing.T) {
	for round := 0; round < 20; round++ {
		e := newChannelEnv(t)
		hooks := &safeHooks{}
		h := NewChannelHandler(e.st, e.state, e.fake, hooks)
		var ints [][2]any
		for i := 0; i < 10; i++ {
			ints = append(ints,
				[2]any{"add", channelInteraction("g1", "c1", "add")},
				[2]any{"remove", channelInteraction("g1", "c1", "remove")},
				[2]any{"add", channelInteraction("g1", "c2", "add")},
				[2]any{"list", channelInteraction("g1", "c2", "list")},
			)
		}
		var wg sync.WaitGroup
		for _, it := range ints {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h(context.Background(), it[1].(*discordgo.InteractionCreate))
			}()
		}
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ChannelDeleted(context.Background(), e.st, hooks, "g1", "c1")
			}()
		}
		wg.Wait()

		mem := e.st.Snapshot()
		s2 := store.New(e.fake, "storage")
		if err := s2.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		disk := s2.Snapshot()
		for _, id := range []string{"c1", "c2"} {
			_, _, a := mem.SourceChannel(id)
			_, _, b := disk.SourceChannel(id)
			if a != b {
				t.Errorf("round %d：%s 記憶體=%v 儲存頻道=%v 不一致", round, id, a, b)
			}
		}
		if _, _, ok := mem.SourceChannel("c2"); !ok {
			t.Errorf("round %d：c2 應存在", round)
		}
		hooks.mu.Lock()
		_, _, c1 := mem.SourceChannel("c1")
		net := hooks.added - hooks.removed
		want := 1 // c2
		if c1 {
			want = 2
		}
		if net != want {
			t.Errorf("round %d：hook 新增 %d 次、移除 %d 次，與結果（%d 個頻道）不符", round, hooks.added, hooks.removed, want)
		}
		hooks.mu.Unlock()
		// 每個 interaction 都有回應（deferred + edit，或直接回應）。
		resp, edits := len(e.fake.Responses), len(e.fake.ResponseEdits) // wg.Wait 之後讀取
		if resp != len(ints) {
			t.Errorf("round %d：%d 個 interaction 只回應 %d 次", round, len(ints), resp)
		}
		if edits != 30 { // add/remove 各 30 個 deferred
			t.Errorf("round %d：deferred 補上內容 %d 次，預期 30", round, edits)
		}
	}
}
