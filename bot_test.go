package main

import (
	"context"
	"errors"
	"testing"

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
