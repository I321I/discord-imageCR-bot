package index

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/imagemsg"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// fakeBackfill 記錄交給補處理的訊息（依呼叫順序）。
type fakeBackfill struct {
	mu   sync.Mutex
	ids  []string
	gid  []string
	ctxs []context.Context
}

func (b *fakeBackfill) EnqueueBackfill(ctx context.Context, guildID string, m *discordgo.Message) <-chan error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ctxs = append(b.ctxs, ctx)
	b.ids = append(b.ids, m.ID)
	b.gid = append(b.gid, guildID)
	ch := make(chan error, 1)
	ch <- nil
	return ch
}

func (b *fakeBackfill) got() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.ids)
}

const botID = "bot"

func addUserImage(f *discordtest.Fake, ch, id, content string) *discordgo.Message {
	return f.AddMessage(&discordgo.Message{
		ID: id, ChannelID: ch, Content: content, Author: &discordgo.User{ID: "u1"},
		Attachments: []*discordgo.MessageAttachment{{Filename: "a.png", ContentType: "image/png"}},
	})
}

func addBotImage(f *discordtest.Fake, ch, id string, fields imagemsg.Fields) {
	f.AddMessage(&discordgo.Message{
		ID: id, ChannelID: ch, Content: imagemsg.Format(fields, "u1"), Author: &discordgo.User{ID: botID, Bot: true},
		Attachments: []*discordgo.MessageAttachment{{Filename: "a.png", ContentType: "image/png"}},
	})
}

func failedReaction() []*discordgo.MessageReactions {
	return []*discordgo.MessageReactions{{Count: 1, Me: true, Emoji: &discordgo.Emoji{Name: "❌"}}}
}

// setAt 前的使用者帶圖訊息 → 舊圖片訊息；setAt 後 → 由舊到新交給補處理；bot 的圖片訊息 → 收錄。
func TestScan_ClassifiesBySetAt(t *testing.T) {
	f := discordtest.NewFake()
	addUserImage(f, "c", "100", "舊圖一")
	addUserImage(f, "c", "101", "舊圖二")
	addBotImage(f, "c", "150", imagemsg.Fields{Work: "作品甲"})
	addUserImage(f, "c", "201", "")
	addUserImage(f, "c", "202", "新圖")
	addUserImage(f, "c", "203", "")
	x, bf := New(), &fakeBackfill{}
	if err := ScanChannel(context.Background(), f, x, bf, botID, "g", "c", "200"); err != nil {
		t.Fatal(err)
	}
	if got := keys(x.Search("", allow("c"), 0)); !slices.Equal(got, []string{"c:150:0", "c:101:0", "c:100:0"}) {
		t.Errorf("索引 %v", got)
	}
	if got := bf.got(); !slices.Equal(got, []string{"201", "202", "203"}) {
		t.Errorf("補處理應由舊到新：%v", got)
	}
	if bf.gid[0] != "g" {
		t.Errorf("補處理伺服器 %v", bf.gid)
	}
	if e, _ := x.Get(Key{ChannelID: "c", MessageID: "100"}); e.Kind != KindLegacy || e.GuildID != "g" {
		t.Errorf("舊訊息 %+v", e)
	}
}

// 以 snowflake 數值比較：setAt "1000" 時 "999" 在前（字串比較會判錯）、"10000" 在後。
func TestScan_SnowflakeNumericCompare(t *testing.T) {
	f := discordtest.NewFake()
	addUserImage(f, "c", "999", "舊")
	addUserImage(f, "c", "10000", "新")
	x, bf := New(), &fakeBackfill{}
	if err := ScanChannel(context.Background(), f, x, bf, botID, "g", "c", "1000"); err != nil {
		t.Fatal(err)
	}
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "999"}); !ok {
		t.Error("999 < 1000 應為舊圖片訊息")
	}
	if got := bf.got(); !slices.Equal(got, []string{"10000"}) {
		t.Errorf("10000 > 1000 應補處理：%v", got)
	}
}

func TestScan_Paginates(t *testing.T) {
	f := discordtest.NewFake()
	for i := 1; i <= 250; i++ {
		addUserImage(f, "c", strconv.Itoa(1000+i), "文字")
	}
	x, bf := New(), &fakeBackfill{}
	if err := ScanChannel(context.Background(), f, x, bf, botID, "g", "c", "1100"); err != nil {
		t.Fatal(err)
	}
	if n := len(x.Search("", allow("c"), 0)); n != 99 {
		t.Errorf("舊圖片訊息應 99 筆，得到 %d", n)
	}
	got := bf.got()
	if len(got) != 151 || got[0] != "1100" || got[150] != "1250" {
		t.Errorf("補處理 %d 筆（%v…）", len(got), got[:min(3, len(got))])
	}
	if n := f.CallCount("ChannelMessages"); n < 3 {
		t.Errorf("應分頁讀取，ChannelMessages 呼叫 %d 次", n)
	}
}

// bot 的非圖片訊息、其他 bot 與 webhook 的圖、非圖片附件、純文字都不收錄也不補處理。
func TestScan_BotNonImageMessagesIgnored(t *testing.T) {
	f := discordtest.NewFake()
	f.AddMessage(&discordgo.Message{ID: "10", ChannelID: "c", Content: "頻道已設定", Author: &discordgo.User{ID: botID, Bot: true}})
	f.AddMessage(&discordgo.Message{ID: "11", ChannelID: "c", Content: "別的 bot", Author: &discordgo.User{ID: "ob", Bot: true},
		Attachments: []*discordgo.MessageAttachment{{Filename: "a.png", ContentType: "image/png"}}})
	f.AddMessage(&discordgo.Message{ID: "12", ChannelID: "c", Content: "webhook", WebhookID: "w", Author: &discordgo.User{ID: "w"},
		Attachments: []*discordgo.MessageAttachment{{Filename: "a.png", ContentType: "image/png"}}})
	f.AddMessage(&discordgo.Message{ID: "13", ChannelID: "c", Content: "影片", Author: &discordgo.User{ID: "u"},
		Attachments: []*discordgo.MessageAttachment{{Filename: "a.mp4", ContentType: "video/mp4"}}})
	f.AddMessage(&discordgo.Message{ID: "14", ChannelID: "c", Content: "純文字", Author: &discordgo.User{ID: "u"}})
	for _, id := range []string{"21", "22", "23", "24"} {
		f.AddMessage(&discordgo.Message{ID: id, ChannelID: "c", Author: &discordgo.User{ID: "u"}})
	}
	x, bf := New(), &fakeBackfill{}
	if err := ScanChannel(context.Background(), f, x, bf, botID, "g", "c", "20"); err != nil {
		t.Fatal(err)
	}
	if got := x.Search("", allow("c"), 0); len(got) != 0 {
		t.Errorf("不應收錄：%v", keys(got))
	}
	if got := bf.got(); len(got) != 0 {
		t.Errorf("不應補處理：%v", got)
	}
}

// 決定 10：setAt 之後帶 bot ❌ 的原訊息不補處理，也不當成舊圖片訊息收錄。
func TestScan_FailedOriginalNeitherBackfilledNorIndexed(t *testing.T) {
	f := discordtest.NewFake()
	m := addUserImage(f, "c", "300", "失敗過的圖")
	m.Reactions = failedReaction()
	// 別人加的 ❌（Me=false）不算。
	o := addUserImage(f, "c", "301", "")
	o.Reactions = []*discordgo.MessageReactions{{Count: 1, Me: false, Emoji: &discordgo.Emoji{Name: "❌"}}}
	x, bf := New(), &fakeBackfill{}
	if err := ScanChannel(context.Background(), f, x, bf, botID, "g", "c", "200"); err != nil {
		t.Fatal(err)
	}
	if got := bf.got(); !slices.Equal(got, []string{"301"}) {
		t.Errorf("只應補處理沒有 bot ❌ 的訊息：%v", got)
	}
	if got := x.Search("", allow("c"), 0); len(got) != 0 {
		t.Errorf("帶 ❌ 的原訊息不應收錄：%v", keys(got))
	}
}

func TestScan_ErrorReturned(t *testing.T) {
	f := discordtest.NewFake()
	f.FailOn["ChannelMessages"] = discordtest.ErrInjected
	if err := ScanChannel(context.Background(), f, New(), &fakeBackfill{}, botID, "g", "c", "1"); err == nil {
		t.Error("讀取失敗應回錯誤")
	}
}

// 掃描被取消（頻道移除）時不交給補處理。
func TestScan_CanceledDoesNotBackfill(t *testing.T) {
	f := discordtest.NewFake()
	addUserImage(f, "c", "300", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	bf := &fakeBackfill{}
	if err := ScanChannel(ctx, f, New(), bf, botID, "g", "c", "200"); err == nil {
		t.Error("取消時應回錯誤")
	}
	if got := bf.got(); len(got) != 0 {
		t.Errorf("取消時不應補處理：%v", got)
	}
}

// blockSecondPage 讓第二次 ChannelMessages 阻塞到 release 關閉。
type blockSecondPage struct {
	*discordtest.Fake
	mu      sync.Mutex
	n       int
	entered chan struct{}
	release chan struct{}
}

func (b *blockSecondPage) ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	b.mu.Lock()
	b.n++
	n := b.n
	b.mu.Unlock()
	if n == 2 {
		close(b.entered)
		<-b.release
	}
	return b.Fake.ChannelMessages(channelID, limit, beforeID, afterID, aroundID, options...)
}

// Q52：掃描期間搜尋回傳已建好的部分。
func TestScan_PartialResultsSearchableDuringScan(t *testing.T) {
	f := discordtest.NewFake()
	for i := 1; i <= 150; i++ {
		addBotImage(f, "c", strconv.Itoa(1000+i), imagemsg.Fields{Work: "甲"})
	}
	api := &blockSecondPage{Fake: f, entered: make(chan struct{}), release: make(chan struct{})}
	x := New()
	done := make(chan error, 1)
	go func() { done <- ScanChannel(context.Background(), api, x, &fakeBackfill{}, botID, "g", "c", "1") }()
	<-api.entered
	if n := len(x.Search("甲", allow("c"), 0)); n != 100 {
		t.Errorf("第一頁讀完後應可搜到 100 筆，得到 %d", n)
	}
	close(api.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(x.Search("甲", allow("c"), 0)); n != 150 {
		t.Errorf("掃描完成應有 150 筆，得到 %d", n)
	}
}

// --- Hooks ---

func newHooksEnv(t *testing.T) (*discordtest.Fake, *store.Store, *Index, *fakeBackfill, *Hooks) {
	t.Helper()
	f := discordtest.NewFake()
	st := store.New(f, "storage")
	st.UseEmpty()
	x, bf := New(), &fakeBackfill{}
	h := NewHooks(context.Background(), f, x, bf, st)
	h.SetBotID(botID)
	return f, st, x, bf, h
}

func addSource(t *testing.T, st *store.Store, guildID, channelID, setAt string) {
	t.Helper()
	if err := st.Update(context.Background(), func(d *store.Data) error {
		d.AddChannel(guildID, channelID, setAt)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待逾時：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// /channel add 後掃描該頻道（setAt 取自設定）。
func TestHooks_ChannelAddedScans(t *testing.T) {
	f, st, x, bf, h := newHooksEnv(t)
	addUserImage(f, "c", "100", "舊")
	addUserImage(f, "c", "300", "新")
	addSource(t, st, "g", "c", "200")
	h.ChannelAdded("g", "c")
	waitCond(t, "掃描完成", func() bool { return len(bf.got()) == 1 })
	h.Wait()
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "100"}); !ok {
		t.Error("舊訊息應收錄")
	}
	if got := bf.got(); !slices.Equal(got, []string{"300"}) {
		t.Errorf("補處理 %v", got)
	}
}

// /channel remove 後清除該頻道的索引。
func TestHooks_ChannelRemovedClears(t *testing.T) {
	_, _, x, _, h := newHooksEnv(t)
	x.PutLegacy("g", &discordgo.Message{ID: "1", ChannelID: "c", Content: "甲", Attachments: []*discordgo.MessageAttachment{{Filename: "a.png"}}})
	h.ChannelRemoved("c")
	if got := x.Search("", allow("c"), 0); len(got) != 0 {
		t.Errorf("應清除：%v", keys(got))
	}
}

// 掃描中移除頻道：掃描停止、不補處理、移除後不留下項目。
func TestHooks_ChannelRemovedDuringScan(t *testing.T) {
	f, st, x, bf, h := newHooksEnv(t)
	for i := 1; i <= 150; i++ {
		addUserImage(f, "c", strconv.Itoa(1000+i), "文字")
	}
	addSource(t, st, "g", "c", "2000")
	api := &blockSecondPage{Fake: f, entered: make(chan struct{}), release: make(chan struct{})}
	h.api = api
	h.ChannelAdded("g", "c")
	<-api.entered
	// 實際呼叫端先從設定移除頻道，才呼叫 ChannelRemoved。
	if err := st.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel("g", "c"); return nil }); err != nil {
		t.Fatal(err)
	}
	removed := make(chan struct{})
	go func() {
		h.ChannelRemoved("c")
		close(removed)
	}()
	time.Sleep(20 * time.Millisecond)
	close(api.release)
	<-removed
	if got := x.Search("", allow("c"), 0); len(got) != 0 {
		t.Errorf("移除後不應留下項目：%d 筆", len(got))
	}
	if got := bf.got(); len(got) != 0 {
		t.Errorf("移除後不應補處理：%v", got)
	}
}

// 啟動時掃描所有來源頻道。
func TestHooks_ScanAll(t *testing.T) {
	f, st, x, bf, h := newHooksEnv(t)
	addUserImage(f, "c1", "100", "甲")
	addUserImage(f, "c2", "101", "乙")
	addUserImage(f, "c2", "300", "")
	addSource(t, st, "g1", "c1", "200")
	addSource(t, st, "g2", "c2", "200")
	f.FailOn = map[string]error{}
	h.ScanAll(st.Snapshot())
	if got := keys(x.Search("", allow("c1", "c2"), 0)); len(got) != 2 {
		t.Errorf("索引 %v", got)
	}
	if e, _ := x.Get(Key{ChannelID: "c2", MessageID: "101"}); e.GuildID != "g2" {
		t.Errorf("伺服器 ID %+v", e)
	}
	if got := bf.got(); !slices.Equal(got, []string{"300"}) {
		t.Errorf("補處理 %v", got)
	}
}

// Q31：訊息被刪除時移除索引項目。
func TestOnMessageDelete_RemovesEntry(t *testing.T) {
	_, _, x, _, h := newHooksEnv(t)
	x.PutImageMessage("g", imageMsg("c", "1", imagemsg.Fields{Work: "甲"}, "u"))
	x.PutImageMessage("g", imageMsg("c", "2", imagemsg.Fields{Work: "甲"}, "u"))
	h.MessageDeleted("c", "1")
	if got := keys(x.Search("甲", allow("c"), 0)); !slices.Equal(got, []string{"c:2:0"}) {
		t.Errorf("得到 %v", got)
	}
	h.MessagesDeleted("c", []string{"2"})
	if got := x.Search("甲", allow("c"), 0); len(got) != 0 {
		t.Errorf("批次刪除後應為空：%v", keys(got))
	}
}

// Q31：舊圖片訊息修改文字 → 更新；改成沒有文字 → 移除；bot 圖片訊息被編輯 → 更新。
func TestOnMessageUpdate_UpdatesEntries(t *testing.T) {
	_, st, x, _, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "200")
	old := legacyMsg("c", "100", "舊文字", 1)
	x.PutLegacy("g", old)

	edited := legacyMsg("c", "100", "新文字", 1)
	h.MessageUpdated(edited)
	if got := x.Search("新文字", allow("c"), 0); len(got) != 1 {
		t.Errorf("修改後應搜到新文字：%v", keys(got))
	}
	if got := x.Search("舊文字", allow("c"), 0); len(got) != 0 {
		t.Errorf("修改後不應搜到舊文字：%v", keys(got))
	}
	h.MessageUpdated(legacyMsg("c", "100", "", 1))
	if got := x.Search("", allow("c"), 0); len(got) != 0 {
		t.Errorf("改成沒有文字應移除：%v", keys(got))
	}

	bm := imageMsg("c", "150", imagemsg.Fields{Work: "甲"}, "u")
	bm.Author = &discordgo.User{ID: botID, Bot: true}
	x.PutImageMessage("g", bm)
	bm2 := imageMsg("c", "150", imagemsg.Fields{Work: "乙"}, "u")
	bm2.Author = &discordgo.User{ID: botID, Bot: true}
	h.MessageUpdated(bm2)
	if got := x.Search("乙", allow("c"), 0); len(got) != 1 {
		t.Errorf("bot 圖片訊息編輯後應更新：%v", keys(got))
	}
}

// setAt 之後的使用者訊息（待處理或失敗的原訊息）、非來源頻道、沒有作者的更新都不收錄。
func TestOnMessageUpdate_Ignores(t *testing.T) {
	_, st, x, _, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "200")
	h.MessageUpdated(legacyMsg("c", "300", "新訊息", 1))
	h.MessageUpdated(legacyMsg("other", "100", "非來源", 1))
	h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: "沒有作者"})
	if got := x.Search("", allow("c", "other"), 0); len(got) != 0 {
		t.Errorf("不應收錄：%v", keys(got))
	}
}

// 補處理用的 ctx 是 bot 關機 ctx，不是掃描用的 ctx：掃描結束後仍未取消，關機時才取消。
func TestHooks_BackfillUsesShutdownContext(t *testing.T) {
	f := discordtest.NewFake()
	st := store.New(f, "storage")
	st.UseEmpty()
	addUserImage(f, "c", "300", "")
	addSource(t, st, "g", "c", "200")
	ctx, cancel := context.WithCancel(context.Background())
	bf := &fakeBackfill{}
	h := NewHooks(ctx, f, New(), bf, st)
	h.SetBotID(botID)
	h.ScanAll(st.Snapshot())
	if len(bf.ctxs) != 1 {
		t.Fatalf("補處理 %d 則", len(bf.ctxs))
	}
	if err := bf.ctxs[0].Err(); err != nil {
		t.Errorf("掃描結束後補處理的 ctx 不應被取消：%v", err)
	}
	cancel()
	if bf.ctxs[0].Err() == nil {
		t.Error("關機時補處理的 ctx 應被取消")
	}
}

// 同一頻道並發 startScan：舊的掃描一定被取消，只有最後一個掃描完成（補處理只交出一輪）。
func TestHooks_ConcurrentChannelAddedCancelsOld(t *testing.T) {
	f, st, _, bf, h := newHooksEnv(t)
	for i := 1; i <= 150; i++ {
		addUserImage(f, "c", strconv.Itoa(1000+i), "")
	}
	addSource(t, st, "g", "c", "1000")
	api := &blockAllSecondPages{Fake: f, release: make(chan struct{})}
	h.api = api
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ChannelAdded("g", "c")
		}()
	}
	wg.Wait()
	close(api.release)
	h.Wait()
	if n := len(bf.got()); n != 150 {
		t.Errorf("只應有一個掃描完成並交出 150 則補處理，得到 %d", n)
	}
}

// blockAllSecondPages 讓每個掃描的第二頁阻塞到 release 關閉（before 非空即為第二頁之後）。
type blockAllSecondPages struct {
	*discordtest.Fake
	release chan struct{}
}

func (b *blockAllSecondPages) ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	if beforeID != "" {
		<-b.release
	}
	return b.Fake.ChannelMessages(channelID, limit, beforeID, afterID, aroundID, options...)
}

// pipeline 發出的圖片訊息經由 Hooks 寫入索引：頻道已移除後不寫入。
func TestHooks_PutImageMessageSkipsRemovedChannel(t *testing.T) {
	_, st, x, _, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "1")
	h.PutImageMessage("g", imageMsg("c", "10", imagemsg.Fields{Work: "甲"}, "u"))
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "10"}); !ok {
		t.Error("來源頻道的圖片訊息應收錄")
	}
	if err := st.Update(context.Background(), func(d *store.Data) error {
		d.RemoveChannel("g", "c")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.ChannelRemoved("c")
	h.PutImageMessage("g", imageMsg("c", "11", imagemsg.Fields{Work: "甲"}, "u"))
	if got := x.Search("", allow("c"), 0); len(got) != 0 {
		t.Errorf("頻道移除後不應寫入：%v", keys(got))
	}
}

// 舊圖片訊息收到沒有附件資訊（nil）的更新：保守不移除；附件確定被清空（空陣列）才移除。
func TestOnMessageUpdate_NoAttachmentInfoKeepsLegacy(t *testing.T) {
	_, st, x, _, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "200")
	x.PutLegacy("g", legacyMsg("c", "100", "舊文字", 1))
	h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: "舊文字", Author: &discordgo.User{ID: "u1"}})
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "100"}); !ok {
		t.Error("沒有附件資訊的更新不應移除")
	}
	h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: "舊文字", Author: &discordgo.User{ID: "u1"}, Attachments: []*discordgo.MessageAttachment{}})
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "100"}); ok {
		t.Error("附件被清空時應移除")
	}
}
