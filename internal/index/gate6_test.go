package index

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"discordbot/internal/imagemsg"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// 把關（階段 6 第二輪）：discordgo v0.29.0 反序列化 MESSAGE_UPDATE 時，
// 沒有 attachments 欄位或為 null → nil；空陣列 → 非 nil 的空切片。並經過 state 合併後仍保持不變。
func TestGate6_MessageUpdateAttachmentsNilVsEmpty(t *testing.T) {
	cases := []struct {
		name       string
		attachJSON string // 空字串表示不帶欄位
		wantNil    bool
		wantKept   bool // 舊圖片訊息是否保留在索引
	}{
		{"欄位不存在", "", true, true},
		{"null", `,"attachments":null`, true, true},
		{"空陣列", `,"attachments":[]`, false, false},
		{"仍有圖片", `,"attachments":[{"id":"a","filename":"a.png","content_type":"image/png"}]`, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := `{"id":"100","channel_id":"c","guild_id":"g","content":"新文字","author":{"id":"u1"}` + c.attachJSON + `}`
			var ev discordgo.MessageUpdate
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				t.Fatal(err)
			}

			// 經過 state（MaxMessageCount > 0，快取中已有該訊息）合併，確認事件本身的 Attachments 不被改寫。
			s := &discordgo.Session{StateEnabled: true, State: discordgo.NewState()}
			s.State.MaxMessageCount = 10
			if err := s.State.GuildAdd(&discordgo.Guild{ID: "g"}); err != nil {
				t.Fatal(err)
			}
			if err := s.State.ChannelAdd(&discordgo.Channel{ID: "c", GuildID: "g"}); err != nil {
				t.Fatal(err)
			}
			if err := s.State.MessageAdd(legacyMsg("c", "100", "舊文字", 1)); err != nil {
				t.Fatal(err)
			}
			if err := s.State.OnInterface(s, &ev); err != nil {
				t.Fatal(err)
			}

			if got := ev.Message.Attachments == nil; got != c.wantNil {
				t.Fatalf("Attachments == nil 為 %v，預期 %v（%#v）", got, c.wantNil, ev.Message.Attachments)
			}

			_, st, x, _, h := newHooksEnv(t)
			addSource(t, st, "g", "c", "200")
			x.PutLegacy("g", legacyMsg("c", "100", "舊文字", 1))
			h.MessageUpdated(ev.Message)
			e, ok := x.Get(Key{ChannelID: "c", MessageID: "100"})
			if ok != c.wantKept {
				t.Fatalf("保留=%v，預期 %v", ok, c.wantKept)
			}
			if c.name == "仍有圖片" && e.LegacyText != "新文字" {
				t.Errorf("文字應更新為新文字，得到 %q", e.LegacyText)
			}
		})
	}
}

// 把關：Hooks 各入口（新增/移除頻道、pipeline 寫入、訊息編輯/刪除、搜尋、ScanAll）並發執行時
// 不死結、無 data race；結束後被移除的頻道沒有殘留項目。
func TestGate6_HooksConcurrentNoDeadlock(t *testing.T) {
	f, st, x, _, h := newHooksEnv(t)
	const nch = 4
	for c := 0; c < nch; c++ {
		ch := fmt.Sprintf("c%d", c)
		for i := 1; i <= 120; i++ {
			addUserImage(f, ch, strconv.Itoa(1000+i), "文字")
		}
		addSource(t, st, "g", ch, "1050")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for c := 0; c < nch; c++ {
			ch := fmt.Sprintf("c%d", c)
			wg.Add(4)
			go func() { // 反覆移除、新增
				defer wg.Done()
				for i := 0; i < 20; i++ {
					_ = st.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel("g", ch); return nil })
					h.ChannelRemoved(ch)
					_ = st.Update(context.Background(), func(d *store.Data) error { d.AddChannel("g", ch, "1050"); return nil })
					h.ChannelAdded("g", ch)
				}
			}()
			go func() { // pipeline 寫入
				defer wg.Done()
				for i := 0; i < 200; i++ {
					h.PutImageMessage("g", imageMsg(ch, strconv.Itoa(5000+i), imagemsg.Fields{Work: "甲"}, "u"))
				}
			}()
			go func() { // 訊息事件
				defer wg.Done()
				for i := 0; i < 200; i++ {
					h.MessageUpdated(legacyMsg(ch, strconv.Itoa(1001+i%40), "改", 1))
					h.MessageDeleted(ch, strconv.Itoa(1001+i%120))
				}
			}()
			go func() { // 搜尋
				defer wg.Done()
				for i := 0; i < 200; i++ {
					x.Search("文字", allow(ch), 25)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ScanAll(st.Snapshot())
		}()
		wg.Wait()
		h.Wait()
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("並發操作 60 秒未結束：疑似死結")
	}

	// 最後把所有頻道移除：不應有殘留。
	for c := 0; c < nch; c++ {
		ch := fmt.Sprintf("c%d", c)
		_ = st.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel("g", ch); return nil })
		h.ChannelRemoved(ch)
	}
	all := allow("c0", "c1", "c2", "c3")
	if got := x.Search("", all, 0); len(got) != 0 {
		t.Errorf("所有頻道移除後仍有 %d 筆", len(got))
	}
}

// 把關：ChannelRemoved 等待取消中的掃描時不持有 h.mu／putMu：
// 掃描卡在 API 呼叫時，其他頻道的新增、pipeline 寫入、訊息編輯不會被擋住。
func TestGate6_ChannelRemovedWaitDoesNotBlockOthers(t *testing.T) {
	f, st, x, _, h := newHooksEnv(t)
	for i := 1; i <= 150; i++ {
		addUserImage(f, "slow", strconv.Itoa(1000+i), "")
	}
	addSource(t, st, "g", "slow", "1")
	addSource(t, st, "g", "other", "1")
	api := &blockSecondPageSignal{blockAllSecondPages: blockAllSecondPages{Fake: f, release: make(chan struct{})}, entered: make(chan struct{})}
	h.api = api
	h.ChannelAdded("g", "slow") // 第二頁阻塞
	<-api.entered

	_ = st.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel("g", "slow"); return nil })
	removed := make(chan struct{})
	go func() {
		h.ChannelRemoved("slow") // 等待卡住的掃描
		close(removed)
	}()

	others := make(chan struct{})
	go func() {
		h.ChannelAdded("g", "other")
		h.PutImageMessage("g", imageMsg("other", "9", imagemsg.Fields{Work: "甲"}, "u"))
		h.MessageUpdated(legacyMsg("other", "0", "x", 1))
		close(others)
	}()
	select {
	case <-others:
	case <-time.After(5 * time.Second):
		t.Fatal("ChannelRemoved 等待掃描時擋住了其他頻道的操作")
	}
	select {
	case <-removed:
		t.Fatal("掃描還卡著，ChannelRemoved 不應已返回")
	default:
	}
	close(api.release)
	select {
	case <-removed:
	case <-time.After(5 * time.Second):
		t.Fatal("掃描放行後 ChannelRemoved 未返回")
	}
	h.Wait()
	if got := x.Search("", allow("slow"), 0); len(got) != 0 {
		t.Errorf("移除後 slow 不應有殘留：%v", keys(got))
	}
	if _, ok := x.Get(Key{ChannelID: "other", MessageID: "9"}); !ok {
		t.Error("other 的圖片訊息應收錄")
	}
}

// blockSecondPageSignal 與 blockAllSecondPages 相同，但第一次進入第二頁時關閉 entered。
type blockSecondPageSignal struct {
	blockAllSecondPages
	entered chan struct{}
	once    sync.Once
}

func (b *blockSecondPageSignal) ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	if beforeID != "" {
		b.once.Do(func() { close(b.entered) })
	}
	return b.blockAllSecondPages.ChannelMessages(channelID, limit, beforeID, afterID, aroundID, options...)
}
