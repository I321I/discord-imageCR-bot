package commands

import (
	"context"
	"fmt"
	"log"
	"slices"

	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

const (
	msgDevOnly          = "只有開發者可以使用此指令"
	msgReloadSuperseded = "讀取期間有新的寫入，已保留最新設定（未套用這次讀到的資料）"
)

// DeveloperIDs 回傳 application 的開發者：有 Team 時為已接受邀請的團隊成員，否則為擁有者。
func DeveloperIDs(app *discordgo.Application) []string {
	if app == nil {
		return nil
	}
	if app.Team != nil {
		var ids []string
		for _, m := range app.Team.Members {
			if m == nil || m.User == nil || m.MembershipState != discordgo.MembershipStateAccepted {
				continue
			}
			ids = append(ids, m.User.ID)
		}
		return ids
	}
	if app.Owner != nil && app.Owner.ID != "" {
		return []string{app.Owner.ID}
	}
	return nil
}

// interactionUserID 回傳下指令的使用者：伺服器內為 Member.User，私訊為 User。
func interactionUserID(i *discordgo.Interaction) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// NewDevHandler 回傳 /storage（開發者指令）的 handler。
func NewDevHandler(st *store.Store, devIDs []string, storageChannelID string, api API) Handler {
	return func(ctx context.Context, i *discordgo.InteractionCreate) {
		uid := interactionUserID(i.Interaction)
		if uid == "" || !slices.Contains(devIDs, uid) {
			respondEphemeral(api, i, msgDevOnly)
			return
		}
		options := i.ApplicationCommandData().Options
		if len(options) == 0 {
			return
		}
		switch options[0].Name {
		case "show":
			respondEphemeral(api, i, fmt.Sprintf("儲存頻道：<#%s>（ID：%s）", storageChannelID, storageChannelID))
		case "reload":
			if !deferEphemeral(api, i) {
				return
			}
			applied, err := st.Reload(ctx)
			if err != nil {
				log.Printf("開發者重新讀取儲存頻道失敗：%v", err)
				editResponse(api, i, "重新讀取失敗："+err.Error())
				return
			}
			if !applied {
				editResponse(api, i, msgReloadSuperseded)
				return
			}
			editResponse(api, i, "已重新讀取儲存頻道")
		case "save":
			if !deferEphemeral(api, i) {
				return
			}
			if err := st.Save(ctx); err != nil {
				log.Printf("開發者寫回儲存頻道失敗：%v", err)
				editResponse(api, i, "寫回失敗："+err.Error())
				return
			}
			editResponse(api, i, "已寫回儲存頻道")
		}
	}
}
