package commands

import (
	"context"
	"slices"
	"strings"
	"testing"

	"discordbot/internal/discordtest"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

func TestDeveloperIDs_Owner(t *testing.T) {
	got := DeveloperIDs(&discordgo.Application{Owner: &discordgo.User{ID: "owner"}})
	if !slices.Equal(got, []string{"owner"}) {
		t.Errorf("得到 %v", got)
	}
}

func TestDeveloperIDs_Team(t *testing.T) {
	app := &discordgo.Application{
		Owner: &discordgo.User{ID: "team-pseudo-user"},
		Team: &discordgo.Team{Members: []*discordgo.TeamMember{
			{User: &discordgo.User{ID: "a"}, MembershipState: discordgo.MembershipStateAccepted},
			{User: &discordgo.User{ID: "b"}, MembershipState: discordgo.MembershipStateAccepted},
			{User: &discordgo.User{ID: "invited"}, MembershipState: discordgo.MembershipStateInvited},
			{User: nil},
		}},
	}
	got := DeveloperIDs(app)
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("得到 %v", got)
	}
}

func TestDeveloperIDs_Nil(t *testing.T) {
	if got := DeveloperIDs(nil); len(got) != 0 {
		t.Errorf("得到 %v", got)
	}
	if got := DeveloperIDs(&discordgo.Application{}); len(got) != 0 {
		t.Errorf("得到 %v", got)
	}
}

type devEnv struct {
	fake *discordtest.Fake
	st   *store.Store
	h    Handler
}

func newDevEnv(t *testing.T) *devEnv {
	t.Helper()
	f := discordtest.NewFake()
	st := store.New(f, "storage")
	if err := st.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &devEnv{fake: f, st: st, h: NewDevHandler(st, []string{"dev"}, "storage", f)}
}

func devInteraction(userID, sub string, inGuild bool) *discordgo.InteractionCreate {
	i := &discordgo.Interaction{
		ID:   "1",
		Type: discordgo.InteractionApplicationCommand,
		Data: discordgo.ApplicationCommandInteractionData{
			Name:    CmdStorage,
			Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: sub, Type: discordgo.ApplicationCommandOptionSubCommand}},
		},
	}
	if inGuild {
		i.GuildID = "g"
		i.Member = &discordgo.Member{User: &discordgo.User{ID: userID}}
	} else {
		i.User = &discordgo.User{ID: userID}
	}
	return &discordgo.InteractionCreate{Interaction: i}
}

func TestDev_RejectsNonDeveloper(t *testing.T) {
	for _, inGuild := range []bool{true, false} {
		e := newDevEnv(t)
		e.h(context.Background(), devInteraction("someone", "save", inGuild))
		if got := lastReply(t, e.fake); got != "只有開發者可以使用此指令" {
			t.Errorf("inGuild=%v：回覆 %q", inGuild, got)
		}
		if len(e.fake.Sent) != 0 {
			t.Errorf("inGuild=%v：非開發者不應寫入", inGuild)
		}
	}
}

func TestDev_AcceptsDeveloperInGuildAndDM(t *testing.T) {
	for _, inGuild := range []bool{true, false} {
		e := newDevEnv(t)
		e.h(context.Background(), devInteraction("dev", "show", inGuild))
		if got := lastReply(t, e.fake); got == "只有開發者可以使用此指令" {
			t.Errorf("inGuild=%v：開發者不應被拒絕", inGuild)
		}
	}
}

func TestDev_Show(t *testing.T) {
	e := newDevEnv(t)
	e.h(context.Background(), devInteraction("dev", "show", true))
	if got, want := lastReply(t, e.fake), "儲存頻道：<#storage>（ID：storage）"; got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
}

func TestDev_Reload(t *testing.T) {
	e := newDevEnv(t)
	d := store.NewData()
	d.AddChannel("g", "c", "1")
	msgs, _ := store.Encode(d, "v1")
	e.fake.AddMessage(&discordgo.Message{ChannelID: "storage", Content: msgs[0]})
	e.h(context.Background(), devInteraction("dev", "reload", true))
	if _, _, ok := e.st.Snapshot().SourceChannel("c"); !ok {
		t.Error("reload 應重新讀取儲存頻道")
	}
	if got := lastEdit(t, e.fake); got != "已重新讀取儲存頻道" {
		t.Errorf("回覆 %q", got)
	}
}

func TestDev_ReloadFailure(t *testing.T) {
	e := newDevEnv(t)
	e.fake.FailOn["ChannelMessages"] = discordtest.ErrInjected
	e.h(context.Background(), devInteraction("dev", "reload", true))
	if got := lastEdit(t, e.fake); !strings.HasPrefix(got, "重新讀取失敗：") {
		t.Errorf("回覆 %q", got)
	}
}

func TestDev_Save(t *testing.T) {
	e := newDevEnv(t)
	e.h(context.Background(), devInteraction("dev", "save", true))
	if len(e.fake.Sent) != 1 || e.fake.Sent[0].ChannelID != "storage" {
		t.Errorf("save 應寫入儲存頻道：%+v", e.fake.Sent)
	}
	if got := lastEdit(t, e.fake); got != "已寫回儲存頻道" {
		t.Errorf("回覆 %q", got)
	}
}

func TestDev_SaveFailure(t *testing.T) {
	e := newDevEnv(t)
	e.fake.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	e.h(context.Background(), devInteraction("dev", "save", true))
	if got := lastEdit(t, e.fake); !strings.HasPrefix(got, "寫回失敗：") {
		t.Errorf("回覆 %q", got)
	}
}
