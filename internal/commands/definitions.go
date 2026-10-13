package commands

import "github.com/bwmarrin/discordgo"

// 指令名稱
const (
	CmdChannel     = "channel"
	CmdSearch      = "search"
	CmdSources     = "sources"
	CmdEditImage   = "編輯圖片訊息"
	CmdRerecognize = "重新辨識"
	CmdStorage     = "storage"
)

// Definitions 回傳要註冊的指令：global 為全域指令，devGuild 只註冊在開發者伺服器（SERVER_ID）。
// 每次呼叫都回傳新的值。
func Definitions() (global []*discordgo.ApplicationCommand, devGuild []*discordgo.ApplicationCommand) {
	guildInstall := func() *[]discordgo.ApplicationIntegrationType {
		return &[]discordgo.ApplicationIntegrationType{discordgo.ApplicationIntegrationGuildInstall}
	}
	userInstall := func() *[]discordgo.ApplicationIntegrationType {
		return &[]discordgo.ApplicationIntegrationType{discordgo.ApplicationIntegrationUserInstall}
	}
	guildOnly := func() *[]discordgo.InteractionContextType {
		return &[]discordgo.InteractionContextType{discordgo.InteractionContextGuild}
	}
	everywhere := func() *[]discordgo.InteractionContextType {
		return &[]discordgo.InteractionContextType{
			discordgo.InteractionContextGuild,
			discordgo.InteractionContextBotDM,
			discordgo.InteractionContextPrivateChannel,
		}
	}
	var manageGuild int64 = discordgo.PermissionManageGuild
	sub := func(name, desc string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionSubCommand, Name: name, Description: desc}
	}

	global = []*discordgo.ApplicationCommand{
		{
			Name:                     CmdChannel,
			Description:              "頻道設定",
			IntegrationTypes:         guildInstall(),
			Contexts:                 guildOnly(),
			DefaultMemberPermissions: &manageGuild,
			Options: []*discordgo.ApplicationCommandOption{
				sub("add", "設定頻道為圖片存取處之一"),
				sub("remove", "移除頻道自圖片存取處"),
				sub("list", "列出此伺服器為圖片存取處的頻道"),
			},
		},
		{
			Name:             CmdSearch,
			Description:      "搜尋圖庫圖片",
			IntegrationTypes: userInstall(),
			Contexts:         everywhere(),
			Options: []*discordgo.ApplicationCommandOption{{
				Type:         discordgo.ApplicationCommandOptionString,
				Name:         "關鍵字",
				Description:  "輸入作品、字幕、角色或關鍵字",
				Required:     true,
				Autocomplete: true,
			}},
		},
		{
			Name:             CmdSources,
			Description:      "選擇要搜尋的圖庫頻道",
			IntegrationTypes: userInstall(),
			Contexts:         everywhere(),
		},
		{
			Type:             discordgo.MessageApplicationCommand,
			Name:             CmdEditImage,
			IntegrationTypes: guildInstall(),
			Contexts:         guildOnly(),
		},
		{
			Type:             discordgo.MessageApplicationCommand,
			Name:             CmdRerecognize,
			IntegrationTypes: guildInstall(),
			Contexts:         guildOnly(),
		},
	}

	devGuild = []*discordgo.ApplicationCommand{
		{
			Name:             CmdStorage,
			Description:      "儲存頻道（開發者專用）",
			IntegrationTypes: guildInstall(),
			Contexts:         guildOnly(),
			Options: []*discordgo.ApplicationCommandOption{
				sub("show", "查看目前的儲存頻道"),
				sub("reload", "重新讀取儲存頻道中的設定"),
				sub("save", "把目前的設定寫回儲存頻道"),
			},
		},
	}
	return global, devGuild
}
