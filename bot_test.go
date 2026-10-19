package main

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"discordbot/internal/commands"
	"discordbot/internal/discordtest"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

func TestLoadStore_FailureStartsEmptyAndWritable(t *testing.T) {
	f := discordtest.NewFake()
	f.FailOn["ChannelMessages"] = discordtest.ErrInjected
	st := store.New(f, "storage")
	loadStore(context.Background(), st)
	if n := len(st.Snapshot().Guilds); n != 0 {
		t.Errorf("應以空資料啟動，得到 %d 個伺服器", n)
	}
	if err := st.Save(context.Background()); err != nil {
		t.Errorf("以空資料啟動後應可寫入：%v", err)
	}
}

func TestLoadStore_NoCompleteVersionStartsEmpty(t *testing.T) {
	f := discordtest.NewFake()
	f.AddMessage(&discordgo.Message{ChannelID: "storage", Content: "imgbot-store v1 1/2\n{"})
	st := store.New(f, "storage")
	loadStore(context.Background(), st)
	if err := st.Save(context.Background()); err != nil {
		t.Errorf("找不到完整版本時應以空資料啟動並可寫入：%v", err)
	}
}

func TestLoadStore_OK(t *testing.T) {
	f := discordtest.NewFake()
	d := store.NewData()
	d.AddChannel("g", "c", "1")
	msgs, _ := store.Encode(d, "v1")
	f.AddMessage(&discordgo.Message{ChannelID: "storage", Content: msgs[0]})
	st := store.New(f, "storage")
	loadStore(context.Background(), st)
	if _, _, ok := st.Snapshot().SourceChannel("c"); !ok {
		t.Error("應載入儲存頻道中的資料")
	}
}

type overwriteCall struct {
	appID, guildID string
	names          []string
}

type fakeRegistrar struct {
	calls []overwriteCall
	err   error
}

func (r *fakeRegistrar) ApplicationCommandBulkOverwrite(appID string, guildID string, cmds []*discordgo.ApplicationCommand, options ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	var names []string
	for _, c := range cmds {
		names = append(names, c.Name)
	}
	r.calls = append(r.calls, overwriteCall{appID, guildID, names})
	return cmds, r.err
}

func TestRegisterCommands_GlobalAndDevGuild(t *testing.T) {
	r := &fakeRegistrar{}
	registerCommands(r, "app", "devguild")
	if len(r.calls) != 2 {
		t.Fatalf("應呼叫 2 次 bulk overwrite，得到 %+v", r.calls)
	}
	if r.calls[0].guildID != "" || r.calls[0].appID != "app" || len(r.calls[0].names) != 5 {
		t.Errorf("第一次應為全域註冊 5 個指令：%+v", r.calls[0])
	}
	if r.calls[1].guildID != "devguild" || len(r.calls[1].names) != 1 || r.calls[1].names[0] != "storage" {
		t.Errorf("第二次應只在開發者伺服器註冊 storage：%+v", r.calls[1])
	}
}

func TestRegisterCommands_NoDevGuild(t *testing.T) {
	r := &fakeRegistrar{}
	registerCommands(r, "app", "")
	if len(r.calls) != 1 || r.calls[0].guildID != "" {
		t.Errorf("未設定 SERVER_ID 時只註冊全域指令：%+v", r.calls)
	}
}

func TestRegisterCommands_ErrorDoesNotPanic(t *testing.T) {
	r := &fakeRegistrar{err: errors.New("boom")}
	registerCommands(r, "app", "devguild")
	if len(r.calls) != 2 {
		t.Errorf("全域註冊失敗仍應嘗試註冊開發者指令：%+v", r.calls)
	}
}

func TestOnChannelDelete_RemovesSourceChannel(t *testing.T) {
	f := discordtest.NewFake()
	st := store.New(f, "storage")
	st.UseEmpty()
	if err := st.Update(context.Background(), func(d *store.Data) error {
		d.AddChannel("g", "c", "1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := onChannelDelete(st, commands.NopIndexHooks{})
	h(nil, &discordgo.ChannelDelete{Channel: &discordgo.Channel{ID: "c", GuildID: "g"}})
	if _, _, ok := st.Snapshot().SourceChannel("c"); ok {
		t.Error("被刪除的來源頻道應從設定移除")
	}
	if len(f.Sent) != 2 {
		t.Errorf("應寫回儲存頻道，得到 %d 次寫入", len(f.Sent))
	}
}

// newLoadedStore 回傳已載入、含指定來源頻道的 store。
func newLoadedStore(t *testing.T, guildID string, channelIDs ...string) (*discordtest.Fake, *store.Store) {
	t.Helper()
	f := discordtest.NewFake()
	st := store.New(f, "storage")
	st.UseEmpty()
	if err := st.Update(context.Background(), func(d *store.Data) error {
		for _, id := range channelIDs {
			d.AddChannel(guildID, id, "1")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f, st
}

type recordHooks struct {
	commands.NopIndexHooks
	removed []string
}

func (h *recordHooks) ChannelRemoved(channelID string) { h.removed = append(h.removed, channelID) }

func TestOnGuildDelete_RemovedGuildClearsSources(t *testing.T) {
	_, st := newLoadedStore(t, "g", "c1", "c2")
	hooks := &recordHooks{}
	onGuildDelete(st, hooks)(nil, &discordgo.GuildDelete{Guild: &discordgo.Guild{ID: "g"}})
	if n := len(st.Snapshot().Channels("g")); n != 0 {
		t.Errorf("bot 離開伺服器後應移除所有來源頻道，剩 %d 個", n)
	}
	if len(hooks.removed) != 2 {
		t.Errorf("應對每個被移除的頻道呼叫 ChannelRemoved：%v", hooks.removed)
	}
}

func TestOnGuildDelete_UnavailableKeepsSources(t *testing.T) {
	f, st := newLoadedStore(t, "g", "c1")
	sent := len(f.Sent)
	hooks := &recordHooks{}
	onGuildDelete(st, hooks)(nil, &discordgo.GuildDelete{Guild: &discordgo.Guild{ID: "g", Unavailable: true}})
	if _, _, ok := st.Snapshot().SourceChannel("c1"); !ok || len(f.Sent) != sent || len(hooks.removed) != 0 {
		t.Error("伺服器暫時無法使用時不應移除來源頻道")
	}
	onGuildDelete(st, hooks)(nil, &discordgo.GuildDelete{})
}

func TestOnGuildCreate_PrunesMissingChannels(t *testing.T) {
	_, st := newLoadedStore(t, "g", "c1", "gone")
	hooks := &recordHooks{}
	onGuildCreate(st, hooks, &sync.RWMutex{})(nil, &discordgo.GuildCreate{Guild: &discordgo.Guild{ID: "g", Channels: []*discordgo.Channel{{ID: "c1"}}}})
	d := st.Snapshot()
	if _, _, ok := d.SourceChannel("gone"); ok {
		t.Error("已不存在的來源頻道應被移除")
	}
	if _, _, ok := d.SourceChannel("c1"); !ok {
		t.Error("仍存在的來源頻道不應被移除")
	}
	if len(hooks.removed) != 1 || hooks.removed[0] != "gone" {
		t.Errorf("應呼叫 ChannelRemoved(gone)：%v", hooks.removed)
	}
}

func TestOnGuildCreate_UnavailableDoesNothing(t *testing.T) {
	f, st := newLoadedStore(t, "g", "c1")
	sent := len(f.Sent)
	onGuildCreate(st, &recordHooks{}, &sync.RWMutex{})(nil, &discordgo.GuildCreate{Guild: &discordgo.Guild{ID: "g", Unavailable: true}})
	onGuildCreate(st, &recordHooks{}, &sync.RWMutex{})(nil, &discordgo.GuildCreate{})
	if _, _, ok := st.Snapshot().SourceChannel("c1"); !ok || len(f.Sent) != sent {
		t.Error("伺服器無法使用（事件中沒有頻道資料）時不應移除來源頻道")
	}
}

// GUILD_CREATE 頻道清單為空（且非 Unavailable）時跳過、不清除。
func TestOnGuildCreate_EmptyChannelListSkips(t *testing.T) {
	f, st := newLoadedStore(t, "g", "c1")
	sent := len(f.Sent)
	hooks := &recordHooks{}
	onGuildCreate(st, hooks, &sync.RWMutex{})(nil, &discordgo.GuildCreate{Guild: &discordgo.Guild{ID: "g"}})
	onGuildCreate(st, hooks, &sync.RWMutex{})(nil, &discordgo.GuildCreate{Guild: &discordgo.Guild{ID: "g", Channels: []*discordgo.Channel{}}})
	if _, _, ok := st.Snapshot().SourceChannel("c1"); !ok || len(f.Sent) != sent || len(hooks.removed) != 0 {
		t.Error("頻道清單為空時不應清除來源頻道")
	}
}

// GUILD_CREATE 收到之後才設定的來源頻道不可被清除。
func TestOnGuildCreate_KeepsChannelSetAfterReceived(t *testing.T) {
	_, st := newLoadedStore(t, "g", "gone")
	future := strconv.FormatUint(uint64(time.Now().Add(time.Hour).UnixMilli()-1420070400000)<<22, 10)
	if err := st.Update(context.Background(), func(d *store.Data) error {
		d.AddChannel("g", "fresh", future)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	onGuildCreate(st, &recordHooks{}, &sync.RWMutex{})(nil, &discordgo.GuildCreate{Guild: &discordgo.Guild{ID: "g", Channels: []*discordgo.Channel{{ID: "c1"}}}})
	d := st.Snapshot()
	if _, _, ok := d.SourceChannel("fresh"); !ok {
		t.Error("收到 GUILD_CREATE 之後才設定的來源頻道不應被清除")
	}
	if _, _, ok := d.SourceChannel("gone"); ok {
		t.Error("之前設定且已不存在的來源頻道應被清除")
	}
}

// mutateOnRUnlock 模擬 state 在讀鎖釋放後立刻原地改寫頻道陣列（State.ChannelRemove）。
type mutateOnRUnlock struct {
	rlocks int
	after  func()
}

func (m *mutateOnRUnlock) RLock() { m.rlocks++ }
func (m *mutateOnRUnlock) RUnlock() {
	if m.after != nil {
		m.after()
		m.after = nil
	}
}

// 頻道 ID 必須在 state 讀鎖下複製：讀鎖釋放後 state 改寫陣列，不應影響這次的比對。
func TestOnGuildCreate_CopiesChannelIDsUnderStateLock(t *testing.T) {
	_, st := newLoadedStore(t, "g", "c1", "c2")
	g := &discordgo.Guild{ID: "g", Channels: []*discordgo.Channel{{ID: "c1"}, {ID: "c2"}}}
	lock := &mutateOnRUnlock{after: func() {
		// 與 discordgo State.ChannelRemove 相同的原地刪除：移除 c1，c2 移到索引 0，最後一格保留舊值。
		g.Channels = append(g.Channels[:0], g.Channels[1:]...)
	}}
	onGuildCreate(st, &recordHooks{}, lock)(nil, &discordgo.GuildCreate{Guild: g})
	if lock.rlocks != 1 {
		t.Errorf("應取得 state 讀鎖一次，得到 %d 次", lock.rlocks)
	}
	d := st.Snapshot()
	for _, id := range []string{"c1", "c2"} {
		if _, _, ok := d.SourceChannel(id); !ok {
			t.Errorf("事件當下存在的來源頻道 %s 不應被清除", id)
		}
	}
}

// 與 state 並行改寫頻道陣列（持有寫鎖）時無 data race（以 -race 檢查）。
func TestOnGuildCreate_ConcurrentStateWriteNoRace(t *testing.T) {
	_, st := newLoadedStore(t, "g", "c1")
	state := &sync.RWMutex{}
	g := &discordgo.Guild{ID: "g", Channels: []*discordgo.Channel{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		state.Lock()
		defer state.Unlock()
		g.Channels = append(g.Channels[:1], g.Channels[2:]...)
	}()
	go func() {
		defer wg.Done()
		onGuildCreate(st, &recordHooks{}, state)(nil, &discordgo.GuildCreate{Guild: g})
	}()
	wg.Wait()
	if _, _, ok := st.Snapshot().SourceChannel("c1"); !ok {
		t.Error("c1 一直存在，不應被清除")
	}
}

type recordingHandler struct {
	got chan *discordgo.Message
	ctx chan context.Context
}

func (h *recordingHandler) HandleMessage(ctx context.Context, m *discordgo.Message) {
	h.ctx <- ctx
	h.got <- m
}

func TestOnMessageCreate_HandsMessageToPipeline(t *testing.T) {
	h := &recordingHandler{got: make(chan *discordgo.Message, 1), ctx: make(chan context.Context, 1)}
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")
	m := &discordgo.Message{ID: "m1", ChannelID: "c"}
	onMessageCreate(ctx, h)(nil, &discordgo.MessageCreate{Message: m})
	select {
	case got := <-h.got:
		if got != m {
			t.Errorf("得到 %+v，預期原訊息", got)
		}
		if c := <-h.ctx; c.Value(key{}) != "v" {
			t.Error("應傳入 bot 的 ctx（關機時取消）")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("沒有呼叫 HandleMessage")
	}
}

func TestOnMessageCreate_NilMessageIgnored(t *testing.T) {
	h := &recordingHandler{got: make(chan *discordgo.Message, 1), ctx: make(chan context.Context, 1)}
	onMessageCreate(context.Background(), h)(nil, &discordgo.MessageCreate{})
	select {
	case <-h.got:
		t.Error("沒有訊息內容時不應呼叫 HandleMessage")
	case <-time.After(50 * time.Millisecond):
	}
}

type ctxCheckingCloser struct {
	ctx            context.Context
	closed         bool
	ctxDoneAtClose bool
}

func (c *ctxCheckingCloser) Close() error {
	c.closed = true
	c.ctxDoneAtClose = c.ctx.Err() != nil
	return nil
}

// 關機：先取消 ctx（進行中的處理不再發出圖片），再關閉 OCR worker。
func TestShutdown_CancelsBeforeClosingOCR(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &ctxCheckingCloser{ctx: ctx}
	shutdown(cancel, c)
	if !c.closed {
		t.Fatal("應關閉 OCR")
	}
	if !c.ctxDoneAtClose {
		t.Error("關閉 OCR 前應先取消 ctx")
	}
}

// recordIndexer 記錄訊息刪除與編輯事件。
type recordIndexer struct {
	deleted []string
	updated []string
}

func (r *recordIndexer) MessageDeleted(channelID, messageID string) {
	r.deleted = append(r.deleted, channelID+"/"+messageID)
}
func (r *recordIndexer) MessagesDeleted(channelID string, ids []string) {
	for _, id := range ids {
		r.deleted = append(r.deleted, channelID+"/"+id)
	}
}
func (r *recordIndexer) MessageUpdated(m *discordgo.Message) { r.updated = append(r.updated, m.ID) }

// Q31：訊息刪除、批次刪除、編輯事件交給索引。
// （圖片網址支援後 onMessageUpdate 另外交給 pipeline，見 TestOnMessageUpdate_CallsIndexAndPipeline；此處以不做事的 handler 代替。）
func TestOnMessageEvents_UpdateIndex(t *testing.T) {
	r := &recordIndexer{}
	nop := nopUpdateHandler{}
	onMessageDelete(r)(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "1", ChannelID: "c"}})
	onMessageDeleteBulk(r)(nil, &discordgo.MessageDeleteBulk{ChannelID: "c", Messages: []string{"2", "3"}})
	onMessageUpdate(context.Background(), r, nop)(nil, &discordgo.MessageUpdate{Message: &discordgo.Message{ID: "4", ChannelID: "c"}})
	onMessageDelete(r)(nil, &discordgo.MessageDelete{})
	onMessageUpdate(context.Background(), r, nop)(nil, &discordgo.MessageUpdate{})
	if want := []string{"c/1", "c/2", "c/3"}; !slices.Equal(r.deleted, want) {
		t.Errorf("刪除 %v，預期 %v", r.deleted, want)
	}
	if !slices.Equal(r.updated, []string{"4"}) {
		t.Errorf("編輯 %v", r.updated)
	}
}

// 右鍵指令與編輯表單都接到 router。
func TestNewRouter_WiresContextMenus(t *testing.T) {
	nop := func(context.Context, *discordgo.InteractionCreate) {}
	r := newRouter(nop, nop, &commands.ContextMenu{}, &commands.Search{}, &commands.Sources{})
	for _, name := range []string{commands.CmdChannel, commands.CmdStorage, commands.CmdEditImage, commands.CmdRerecognize, commands.CmdSearch} {
		if r.Commands[name] == nil {
			t.Errorf("指令 %s 沒有 handler", name)
		}
	}
	if r.Modals[commands.EditModalPrefix] == nil {
		t.Error("編輯表單沒有 handler")
	}
	if r.Autocomplete[commands.CmdSearch] == nil {
		t.Error("/search 的 autocomplete 沒有 handler")
	}
}

// /sources 指令與它的選單接到 router（階段 10）。
func TestNewRouter_WiresSources(t *testing.T) {
	nop := func(context.Context, *discordgo.InteractionCreate) {}
	r := newRouter(nop, nop, &commands.ContextMenu{}, &commands.Search{}, &commands.Sources{})
	if r.Commands[commands.CmdSources] == nil {
		t.Error("/sources 沒有 handler")
	}
	if r.Components[commands.SourcesMenuPrefix] == nil {
		t.Error("/sources 的選單沒有 handler")
	}
}

type nopUpdateHandler struct{}

func (nopUpdateHandler) HandleMessageUpdate(context.Context, *discordgo.Message) {}
