// Package commands 實作所有 slash command、右鍵指令與其互動元件。
package commands

import (
	"context"
	"log"
	"runtime/debug"
	"strings"

	"github.com/bwmarrin/discordgo"
)

type Handler func(ctx context.Context, i *discordgo.InteractionCreate)

// Router 依 interaction 類型與名稱分派給 handler。
type Router struct {
	Commands     map[string]Handler // InteractionApplicationCommand，key = 指令名稱（含右鍵指令名稱）
	Autocomplete map[string]Handler // InteractionApplicationCommandAutocomplete，key = 指令名稱
	Components   map[string]Handler // InteractionMessageComponent，key = CustomID 冒號前的前綴
	Modals       map[string]Handler // InteractionModalSubmit，key = CustomID 冒號前的前綴
}

// Handle 可直接傳給 session.AddHandler。handler panic 時會被 recover 並記 log。
func (r *Router) Handle(s *discordgo.Session, i *discordgo.InteractionCreate) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("處理 interaction %s 時 panic：%v\n%s", i.ID, p, debug.Stack())
		}
	}()

	var h Handler
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		h = r.Commands[i.ApplicationCommandData().Name]
	case discordgo.InteractionApplicationCommandAutocomplete:
		h = r.Autocomplete[i.ApplicationCommandData().Name]
	case discordgo.InteractionMessageComponent:
		h = r.Components[prefix(i.MessageComponentData().CustomID)]
	case discordgo.InteractionModalSubmit:
		h = r.Modals[prefix(i.ModalSubmitData().CustomID)]
	}
	if h != nil {
		h(context.Background(), i)
	}
}

func prefix(customID string) string {
	p, _, _ := strings.Cut(customID, ":")
	return p
}
