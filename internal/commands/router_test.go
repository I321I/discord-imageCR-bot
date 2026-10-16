package commands

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func ic(t discordgo.InteractionType, data discordgo.InteractionData) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "1", Type: t, Data: data}}
}

func TestRouter_DispatchesByInteractionType(t *testing.T) {
	var got []string
	rec := func(name string) Handler {
		return func(ctx context.Context, i *discordgo.InteractionCreate) { got = append(got, name) }
	}
	r := &Router{
		Commands:     map[string]Handler{"channel": rec("cmd")},
		Autocomplete: map[string]Handler{"search": rec("auto")},
		Components:   map[string]Handler{"pickch": rec("comp")},
		Modals:       map[string]Handler{"editimg": rec("modal")},
	}
	r.Handle(nil, ic(discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: "channel"}))
	r.Handle(nil, ic(discordgo.InteractionApplicationCommandAutocomplete, discordgo.ApplicationCommandInteractionData{Name: "search"}))
	r.Handle(nil, ic(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "pickch:2"}))
	r.Handle(nil, ic(discordgo.InteractionModalSubmit, discordgo.ModalSubmitInteractionData{CustomID: "editimg:c:m"}))
	want := []string{"cmd", "auto", "comp", "modal"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("分派結果 %v，預期 %v", got, want)
	}

	// 未知名稱、未知類型、PING 都不 panic、不呼叫任何 handler
	got = nil
	r.Handle(nil, ic(discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: "unknown"}))
	r.Handle(nil, ic(discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "channel"}))
	r.Handle(nil, ic(discordgo.InteractionModalSubmit, discordgo.ModalSubmitInteractionData{CustomID: "x"}))
	r.Handle(nil, ic(discordgo.InteractionPing, nil))
	r.Handle(nil, ic(discordgo.InteractionType(99), nil))
	if len(got) != 0 {
		t.Errorf("不應呼叫任何 handler：%v", got)
	}
}

func TestRouter_NilMaps(t *testing.T) {
	r := &Router{}
	r.Handle(nil, ic(discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: "x"}))
}

func TestRouter_RecoversPanic(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	r := &Router{Commands: map[string]Handler{"boom": func(ctx context.Context, i *discordgo.InteractionCreate) {
		panic("kaboom")
	}}}
	r.Handle(nil, ic(discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: "boom"}))
	if !strings.Contains(buf.String(), "kaboom") {
		t.Errorf("panic 應被 recover 並記 log，log 內容：%q", buf.String())
	}
}
