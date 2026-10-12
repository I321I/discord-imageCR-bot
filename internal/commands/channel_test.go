package commands

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"discordbot/internal/discordtest"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

const allBotPerms = discordgo.PermissionViewChannel | discordgo.PermissionReadMessageHistory |
	discordgo.PermissionSendMessages | discordgo.PermissionAttachFiles | discordgo.PermissionManageMessages |
	discordgo.PermissionAddReactions

type fakeHooks struct {
	added   [][2]string
	removed []string
}

func (h *fakeHooks) ChannelAdded(guildID, channelID string) {
	h.added = append(h.added, [2]string{guildID, channelID})
}
func (h *fakeHooks) ChannelRemoved(channelID string) { h.removed = append(h.removed, channelID) }

type channelEnv struct {
	fake  *discordtest.Fake
	st    *store.Store
	state *discordgo.State
	hooks *fakeHooks
	h     Handler
}

func newChannelEnv(t *testing.T) *channelEnv {
	t.Helper()
	state := discordgo.NewState()
	for _, g := range []*discordgo.Guild{
		{ID: "g1", Name: "伺服器一", Channels: []*discordgo.Channel{
			{ID: "c1", GuildID: "g1", Name: "一般", Type: discordgo.ChannelTypeGuildText},
			{ID: "c2", GuildID: "g1", Name: "閒聊", Type: discordgo.ChannelTypeGuildText},
			{ID: "news", GuildID: "g1", Name: "公告", Type: discordgo.ChannelTypeGuildNews},
			{ID: "voice", GuildID: "g1", Name: "語音", Type: discordgo.ChannelTypeGuildVoice},
		}},
		{ID: "g2", Name: "伺服器二", Channels: []*discordgo.Channel{
			{ID: "c3", GuildID: "g2", Name: "圖庫", Type: discordgo.ChannelTypeGuildText},
		}},
	} {
		if err := state.GuildAdd(g); err != nil {
			t.Fatal(err)
		}
	}
	f := discordtest.NewFake()
	st := store.New(f, "storage")
	if err := st.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	hooks := &fakeHooks{}
	return &channelEnv{fake: f, st: st, state: state, hooks: hooks, h: NewChannelHandler(st, state, f, hooks)}
}

var nextInteractionID = 1000

func channelInteraction(guildID, channelID, sub string) *discordgo.InteractionCreate {
	nextInteractionID++
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID:             strconv.Itoa(nextInteractionID),
		Type:           discordgo.InteractionApplicationCommand,
		GuildID:        guildID,
		ChannelID:      channelID,
		AppPermissions: allBotPerms,
		Member:         &discordgo.Member{User: &discordgo.User{ID: "u1"}},
		Data: discordgo.ApplicationCommandInteractionData{
			Name: CmdChannel,
			Options: []*discordgo.ApplicationCommandInteractionDataOption{
				{Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand},
			},
		},
	}}
}

// lastReply 回傳最後一個回應的內容，並檢查它是 ephemeral 的訊息回應。
func lastReply(t *testing.T, f *discordtest.Fake) string {
	t.Helper()
	if len(f.Responses) == 0 {
		t.Fatal("沒有任何回應")
	}
	r := f.Responses[len(f.Responses)-1].Response
	if r.Type != discordgo.InteractionResponseChannelMessageWithSource {
		t.Errorf("回應類型應為訊息，得到 %v", r.Type)
	}
	if r.Data == nil || r.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Errorf("回應應為 ephemeral")
	}
	return r.Data.Content
}

// lastEdit 檢查最後一個回應是 ephemeral 的 deferred 回應，並回傳最後一次補上的內容。
func lastEdit(t *testing.T, f *discordtest.Fake) string {
	t.Helper()
	if len(f.Responses) == 0 {
		t.Fatal("沒有任何回應")
	}
	r := f.Responses[len(f.Responses)-1].Response
	if r.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
		t.Errorf("回應類型應為 deferred 訊息，得到 %v", r.Type)
	}
	if r.Data == nil || r.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Errorf("deferred 回應應為 ephemeral")
	}
	if len(f.ResponseEdits) == 0 {
		t.Fatal("deferred 後沒有補上內容")
	}
	e := f.ResponseEdits[len(f.ResponseEdits)-1]
	if e.Interaction != f.Responses[len(f.Responses)-1].Interaction {
		t.Error("補上內容的 interaction 與 deferred 回應不同")
	}
	if e.Edit.Content == nil {
		t.Fatal("補上的內容為空")
	}
	return *e.Edit.Content
}

func (e *channelEnv) run(i *discordgo.InteractionCreate) {
	e.h(context.Background(), i)
}

func TestChannelAdd_UsesInteractionGuildAndSetAt(t *testing.T) {
	e := newChannelEnv(t)
	i := channelInteraction("g1", "c1", "add")
	e.run(i)
	g, setAt, ok := e.st.Snapshot().SourceChannel("c1")
	if !ok || g != "g1" || setAt != i.ID {
		t.Fatalf("store 應記錄 g1/c1/setAt=%s，得到 %s %s %v", i.ID, g, setAt, ok)
	}
	if got, want := lastEdit(t, e.fake), "成功新增頻道！新增後頻道列表：\n伺服器：伺服器一\n[一般]\n"; got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
	if len(e.hooks.added) != 1 || e.hooks.added[0] != [2]string{"g1", "c1"} {
		t.Errorf("應呼叫 ChannelAdded(g1, c1)：%v", e.hooks.added)
	}
	if len(e.fake.Sent) != 1 || e.fake.Sent[0].ChannelID != "storage" {
		t.Errorf("應寫入儲存頻道一次：%+v", e.fake.Sent)
	}
}

func TestChannelAdd_Duplicate(t *testing.T) {
	e := newChannelEnv(t)
	first := channelInteraction("g1", "c1", "add")
	e.run(first)
	e.run(channelInteraction("g1", "c1", "add"))
	if got, want := lastEdit(t, e.fake), "頻道已存在！目前頻道列表：\n伺服器：伺服器一\n[一般]\n"; got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
	if _, setAt, _ := e.st.Snapshot().SourceChannel("c1"); setAt != first.ID {
		t.Errorf("重複新增不應改 setAt")
	}
	if len(e.fake.Sent) != 1 || len(e.hooks.added) != 1 {
		t.Errorf("重複新增不應寫入或呼叫 hook")
	}
}

func TestChannelAdd_NewsChannelAllowed(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "news", "add"))
	if _, _, ok := e.st.Snapshot().SourceChannel("news"); !ok {
		t.Error("公告頻道應可設定")
	}
}

func TestChannelAdd_UnsupportedType(t *testing.T) {
	for _, ch := range []string{"voice"} {
		e := newChannelEnv(t)
		e.run(channelInteraction("g1", ch, "add"))
		if got := lastEdit(t, e.fake); got != "此頻道類型不支援" {
			t.Errorf("%s：回覆 %q", ch, got)
		}
		if _, _, ok := e.st.Snapshot().SourceChannel(ch); ok {
			t.Errorf("%s 不應被設定", ch)
		}
	}
}

func TestChannelAdd_MissingPermissionsWarning(t *testing.T) {
	e := newChannelEnv(t)
	i := channelInteraction("g1", "c1", "add")
	i.AppPermissions = discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionAttachFiles
	e.run(i)
	if _, _, ok := e.st.Snapshot().SourceChannel("c1"); !ok {
		t.Fatal("缺權限仍應設定")
	}
	want := "成功新增頻道！新增後頻道列表：\n伺服器：伺服器一\n[一般]\n\n⚠️ 機器人在此頻道缺少以下權限，圖片可能無法正常處理：讀取訊息歷史、管理訊息、新增反應"
	if got := lastEdit(t, e.fake); got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
}

func TestChannelAdd_ChannelNotInState(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "unknown-channel", "add"))
	if got := lastEdit(t, e.fake); got != "找不到此頻道，請稍後再試" {
		t.Errorf("回覆 %q", got)
	}
	if _, _, ok := e.st.Snapshot().SourceChannel("unknown-channel"); ok {
		t.Error("不應被設定")
	}
	if len(e.fake.Sent) != 0 {
		t.Error("不應寫入")
	}
}

func TestChannelAdd_MissingAddReactionsWarning(t *testing.T) {
	e := newChannelEnv(t)
	i := channelInteraction("g1", "c1", "add")
	i.AppPermissions = allBotPerms &^ discordgo.PermissionAddReactions
	e.run(i)
	want := "成功新增頻道！新增後頻道列表：\n伺服器：伺服器一\n[一般]\n\n⚠️ 機器人在此頻道缺少以下權限，圖片可能無法正常處理：新增反應"
	if got := lastEdit(t, e.fake); got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
}

func TestChannelAdd_AdministratorHasAllPermissions(t *testing.T) {
	e := newChannelEnv(t)
	i := channelInteraction("g1", "c1", "add")
	i.AppPermissions = discordgo.PermissionAdministrator
	e.run(i)
	if got := lastEdit(t, e.fake); strings.Contains(got, "缺少") {
		t.Errorf("管理員權限不應警告：%q", got)
	}
}

func TestChannelAdd_StorageWriteFails(t *testing.T) {
	e := newChannelEnv(t)
	e.fake.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	e.run(channelInteraction("g1", "c1", "add"))
	if got := lastEdit(t, e.fake); got != "儲存設定失敗，請稍後再試" {
		t.Errorf("回覆 %q", got)
	}
	if _, _, ok := e.st.Snapshot().SourceChannel("c1"); ok {
		t.Error("寫入失敗應撤回")
	}
	if len(e.hooks.added) != 0 {
		t.Error("寫入失敗不應呼叫 hook")
	}
}

func TestChannelRemove_UsesInteractionGuild(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "c1", "add"))
	e.run(channelInteraction("g1", "c2", "add"))
	e.run(channelInteraction("g2", "c3", "add"))
	e.run(channelInteraction("g1", "c1", "remove"))
	if got, want := lastEdit(t, e.fake), "移除此頻道自頻道列表，目前頻道列表：\n伺服器：伺服器一\n[閒聊]\n"; got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
	d := e.st.Snapshot()
	if _, _, ok := d.SourceChannel("c1"); ok {
		t.Error("c1 應被移除")
	}
	if len(d.Channels("g2")) != 1 || len(d.Channels("g1")) != 1 {
		t.Errorf("其他頻道不應受影響：%+v", d.Guilds)
	}
	if len(e.hooks.removed) != 1 || e.hooks.removed[0] != "c1" {
		t.Errorf("應呼叫 ChannelRemoved(c1)：%v", e.hooks.removed)
	}
}

func TestChannelRemove_NotListed(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "c1", "remove"))
	if got := lastEdit(t, e.fake); got != "此頻道不存在於頻道列表中" {
		t.Errorf("回覆 %q", got)
	}
	if len(e.fake.Sent) != 0 || len(e.hooks.removed) != 0 {
		t.Error("不應寫入或呼叫 hook")
	}
}

func TestChannelRemove_StorageWriteFails(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "c1", "add"))
	e.fake.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	e.run(channelInteraction("g1", "c1", "remove"))
	if got := lastEdit(t, e.fake); got != "儲存設定失敗，請稍後再試" {
		t.Errorf("回覆 %q", got)
	}
	if _, _, ok := e.st.Snapshot().SourceChannel("c1"); !ok {
		t.Error("寫入失敗應撤回（頻道仍在）")
	}
	if len(e.hooks.removed) != 0 {
		t.Error("寫入失敗不應呼叫 hook")
	}
}

func TestChannelList_OnlyCurrentGuild(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "c1", "add"))
	e.run(channelInteraction("g1", "c2", "add"))
	e.run(channelInteraction("g2", "c3", "add"))
	e.run(channelInteraction("g1", "c1", "list"))
	if got, want := lastReply(t, e.fake), "頻道列表：\n伺服器：伺服器一\n[一般、閒聊]\n"; got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
}

// addMissingChannel 在 store 中加入一個 state 查不到的來源頻道（例如 bot 離線時被刪除）。
func (e *channelEnv) addMissingChannel(t *testing.T, guildID, channelID, setAt string) {
	t.Helper()
	if err := e.st.Update(context.Background(), func(d *store.Data) error {
		d.AddChannel(guildID, channelID, setAt)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestChannelList_ChannelMissingFromStateShowsID(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "c1", "add"))
	e.addMissingChannel(t, "g1", "gone", "999999999")
	e.run(channelInteraction("g1", "c1", "list"))
	if got, want := lastReply(t, e.fake), "頻道列表：\n伺服器：伺服器一\n[一般、gone]\n"; got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
}

func TestChannelAddRemove_ListWithMissingChannelStillSucceeds(t *testing.T) {
	e := newChannelEnv(t)
	e.addMissingChannel(t, "g1", "gone", "1")
	e.run(channelInteraction("g1", "c1", "add"))
	if got, want := lastEdit(t, e.fake), "成功新增頻道！新增後頻道列表：\n伺服器：伺服器一\n[gone、一般]\n"; got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
	e.run(channelInteraction("g1", "c1", "remove"))
	if got, want := lastEdit(t, e.fake), "移除此頻道自頻道列表，目前頻道列表：\n伺服器：伺服器一\n[gone]\n"; got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
}

func TestChannelDeleted_RemovesSourceAndSaves(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "c1", "add"))
	e.run(channelInteraction("g1", "c2", "add"))
	if err := e.st.Update(context.Background(), func(d *store.Data) error {
		d.SetUserChannels("u", []string{"c1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sent := len(e.fake.Sent)
	ChannelDeleted(context.Background(), e.st, e.hooks, "g1", "c1")
	d := e.st.Snapshot()
	if _, _, ok := d.SourceChannel("c1"); ok {
		t.Error("被刪除的頻道應從設定移除")
	}
	if _, chosen := d.UserChannels("u"); chosen {
		t.Error("勾選因此變成 0 個的使用者應視為沒選過")
	}
	if len(e.fake.Sent) != sent+1 {
		t.Errorf("應寫回儲存頻道一次，得到 %d 次", len(e.fake.Sent)-sent)
	}
	if len(e.hooks.removed) != 1 || e.hooks.removed[0] != "c1" {
		t.Errorf("應呼叫 ChannelRemoved(c1)：%v", e.hooks.removed)
	}
	s2 := store.New(e.fake, "storage")
	if err := s2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s2.Snapshot().SourceChannel("c1"); ok {
		t.Error("儲存頻道最新版本不應再有 c1")
	}
}

func TestChannelDeleted_NotSourceDoesNothing(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "c1", "add"))
	sent := len(e.fake.Sent)
	ChannelDeleted(context.Background(), e.st, e.hooks, "g1", "c2")
	if len(e.fake.Sent) != sent || len(e.hooks.removed) != 0 {
		t.Error("不是來源頻道時不應寫入或呼叫 hook")
	}
}

func TestChannelDeleted_WriteFailureKeepsData(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "c1", "add"))
	e.fake.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	ChannelDeleted(context.Background(), e.st, e.hooks, "g1", "c1")
	if _, _, ok := e.st.Snapshot().SourceChannel("c1"); !ok {
		t.Error("寫入失敗時記憶體不變")
	}
	if len(e.hooks.removed) != 0 {
		t.Error("寫入失敗不應呼叫 hook")
	}
}

func TestChannelList_Empty(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "c1", "list"))
	if got := lastReply(t, e.fake); got != "頻道列表：\n" {
		t.Errorf("回覆 %q", got)
	}
}

func TestChannelId(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "c1", "id"))
	if got := lastReply(t, e.fake); got != "頻道ID：c1" {
		t.Errorf("回覆 %q", got)
	}
}

func TestChannel_IgnoredInDM(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("", "dm", "add"))
	if len(e.fake.Responses) != 0 || len(e.fake.Sent) != 0 {
		t.Error("私訊中不應處理")
	}
}

func TestChannelAdd_DeferFailsSkipsAdd(t *testing.T) {
	e := newChannelEnv(t)
	e.fake.FailOn["InteractionRespond"] = discordtest.ErrInjected
	e.run(channelInteraction("g1", "c1", "add"))
	if _, _, ok := e.st.Snapshot().SourceChannel("c1"); ok {
		t.Error("deferred 回應失敗時（使用者看到互動失敗）不應新增")
	}
	if len(e.fake.ResponseEdits) != 0 || len(e.fake.Sent) != 0 {
		t.Error("deferred 回應失敗時不應寫入或補上內容")
	}
}
