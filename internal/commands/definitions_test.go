package commands

import (
	"slices"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func byName(cmds []*discordgo.ApplicationCommand) map[string]*discordgo.ApplicationCommand {
	m := map[string]*discordgo.ApplicationCommand{}
	for _, c := range cmds {
		m[c.Name] = c
	}
	return m
}

func subNames(c *discordgo.ApplicationCommand) []string {
	var out []string
	for _, o := range c.Options {
		if o.Type == discordgo.ApplicationCommandOptionSubCommand {
			out = append(out, o.Name)
		}
	}
	return out
}

func TestDefinitions(t *testing.T) {
	global, dev := Definitions()
	g := byName(global)
	if len(g) != 5 {
		t.Fatalf("全域指令應有 5 個，得到 %v", len(g))
	}

	ch := g["channel"]
	if ch == nil {
		t.Fatal("缺 channel")
	}
	if ch.IntegrationTypes == nil || !slices.Equal(*ch.IntegrationTypes, []discordgo.ApplicationIntegrationType{discordgo.ApplicationIntegrationGuildInstall}) {
		t.Errorf("channel 應只有伺服器安裝")
	}
	if ch.Contexts == nil || !slices.Equal(*ch.Contexts, []discordgo.InteractionContextType{discordgo.InteractionContextGuild}) {
		t.Errorf("channel 應只能在伺服器使用")
	}
	if ch.DefaultMemberPermissions == nil || *ch.DefaultMemberPermissions != discordgo.PermissionManageGuild {
		t.Errorf("channel 預設應需要管理伺服器權限")
	}
	if !slices.Equal(subNames(ch), []string{"add", "remove", "list"}) {
		t.Errorf("channel 子指令不符：%v", subNames(ch))
	}
	if ch.Options[0].Description != "設定頻道為圖片存取處之一" {
		t.Errorf("子指令描述應沿用現有文字")
	}

	s := g["search"]
	if s == nil {
		t.Fatal("缺 search")
	}
	if s.Description != "搜尋圖庫圖片" {
		t.Errorf("search 描述不符：%q", s.Description)
	}
	if !slices.Equal(*s.IntegrationTypes, []discordgo.ApplicationIntegrationType{discordgo.ApplicationIntegrationUserInstall}) {
		t.Errorf("search 應為使用者安裝")
	}
	allCtx := []discordgo.InteractionContextType{discordgo.InteractionContextGuild, discordgo.InteractionContextBotDM, discordgo.InteractionContextPrivateChannel}
	if s.Contexts == nil || !slices.Equal(*s.Contexts, allCtx) {
		t.Errorf("search 應可在伺服器、bot 私訊、其他私訊使用")
	}
	if len(s.Options) != 1 {
		t.Fatalf("search 應有 1 個參數")
	}
	o := s.Options[0]
	if o.Type != discordgo.ApplicationCommandOptionString || o.Name != "關鍵字" || !o.Required || !o.Autocomplete || o.Description != "輸入作品、字幕、角色或關鍵字" {
		t.Errorf("search 參數不符：%+v", o)
	}

	src := g["sources"]
	if src == nil {
		t.Fatal("缺 sources")
	}
	if src.Description != "選擇要搜尋的圖庫頻道" || !slices.Equal(*src.IntegrationTypes, []discordgo.ApplicationIntegrationType{discordgo.ApplicationIntegrationUserInstall}) {
		t.Errorf("sources 不符：%+v", src)
	}

	for _, name := range []string{"編輯圖片訊息", "重新辨識"} {
		c := g[name]
		if c == nil {
			t.Fatalf("缺 %s", name)
		}
		if c.Type != discordgo.MessageApplicationCommand {
			t.Errorf("%s 應為訊息右鍵指令", name)
		}
		if !slices.Equal(*c.IntegrationTypes, []discordgo.ApplicationIntegrationType{discordgo.ApplicationIntegrationGuildInstall}) {
			t.Errorf("%s 應只有伺服器安裝", name)
		}
		if !slices.Equal(*c.Contexts, []discordgo.InteractionContextType{discordgo.InteractionContextGuild}) {
			t.Errorf("%s 應只能在伺服器使用", name)
		}
	}

	if len(dev) != 1 || dev[0].Name != "storage" {
		t.Fatalf("開發者指令應只有 storage：%v", dev)
	}
	if !slices.Equal(subNames(dev[0]), []string{"show", "reload", "save"}) {
		t.Errorf("storage 子指令不符：%v", subNames(dev[0]))
	}
}

func TestDefinitions_FreshCopies(t *testing.T) {
	a, _ := Definitions()
	b, _ := Definitions()
	*a[0].DefaultMemberPermissions = 0
	if *byName(b)["channel"].DefaultMemberPermissions != discordgo.PermissionManageGuild {
		t.Error("每次呼叫應回傳新的定義")
	}
}
