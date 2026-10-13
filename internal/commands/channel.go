package commands

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// API 是指令 handler 需要的 Discord 方法，*discordgo.Session 直接滿足。
type API interface {
	InteractionRespond(interaction *discordgo.Interaction, resp *discordgo.InteractionResponse, options ...discordgo.RequestOption) error
	InteractionResponseEdit(interaction *discordgo.Interaction, newresp *discordgo.WebhookEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// StateReader 讀取 gateway state，*discordgo.State 直接滿足。
type StateReader interface {
	Guild(guildID string) (*discordgo.Guild, error)
	Channel(channelID string) (*discordgo.Channel, error)
}

// IndexHooks 在來源頻道新增/移除成功後被呼叫。
// 呼叫端來自不同的 Discord 事件 goroutine，實作必須可並行呼叫（自行上鎖）。
type IndexHooks interface {
	ChannelAdded(guildID, channelID string)
	ChannelRemoved(channelID string)
}

// NopIndexHooks 不做任何事（索引完成前使用）。
type NopIndexHooks struct{}

func (NopIndexHooks) ChannelAdded(guildID, channelID string) {}
func (NopIndexHooks) ChannelRemoved(channelID string)        {}

const (
	msgStorageFailed      = "儲存設定失敗，請稍後再試"
	msgUnsupportedChannel = "此頻道類型不支援"
	msgChannelNotFound    = "找不到此頻道，請稍後再試"
)

// botPermissions 是 bot 在來源頻道需要的權限（依顯示順序）。
var botPermissions = []struct {
	perm int64
	name string
}{
	{discordgo.PermissionViewChannel, "檢視頻道"},
	{discordgo.PermissionReadMessageHistory, "讀取訊息歷史"},
	{discordgo.PermissionSendMessages, "傳送訊息"},
	{discordgo.PermissionAttachFiles, "附加檔案"},
	{discordgo.PermissionManageMessages, "管理訊息"},
	{discordgo.PermissionAddReactions, "新增反應"},
}

var (
	errAlreadyAdded = errors.New("頻道已存在")
	errNotListed    = errors.New("頻道不在列表中")
)

type channelHandler struct {
	st    *store.Store
	state StateReader
	api   API
	hooks IndexHooks
}

// NewChannelHandler 回傳 /channel 的 handler。
func NewChannelHandler(st *store.Store, state StateReader, api API, hooks IndexHooks) Handler {
	h := &channelHandler{st: st, state: state, api: api, hooks: hooks}
	return h.handle
}

func (h *channelHandler) handle(ctx context.Context, i *discordgo.InteractionCreate) {
	if i.GuildID == "" {
		return
	}
	options := i.ApplicationCommandData().Options
	if len(options) == 0 {
		return
	}
	switch options[0].Name {
	case "add":
		// 會寫入儲存頻道，可能超過 3 秒，先送 deferred 回應再補上內容。
		if !deferEphemeral(h.api, i) {
			return
		}
		h.add(ctx, i)
	case "remove":
		if !deferEphemeral(h.api, i) {
			return
		}
		h.remove(ctx, i)
	case "list":
		h.list(i)
	}
}

// add 在 deferred 回應之後執行，結果以 edit 補上。
func (h *channelHandler) add(ctx context.Context, i *discordgo.InteractionCreate) {
	ch, err := h.state.Channel(i.ChannelID)
	if err != nil {
		h.edit(i, msgChannelNotFound)
		return
	}
	if ch.Type != discordgo.ChannelTypeGuildText && ch.Type != discordgo.ChannelTypeGuildNews {
		h.edit(i, msgUnsupportedChannel)
		return
	}

	err = h.st.Update(ctx, func(d *store.Data) error {
		// interaction ID 就是當下的 snowflake，當作設定時間點。
		if !d.AddChannel(i.GuildID, i.ChannelID, i.ID) {
			return errAlreadyAdded
		}
		return nil
	})
	switch {
	case errors.Is(err, errAlreadyAdded):
		h.editWithList(i, "頻道已存在！目前頻道列表：\n%v", "")
		return
	case err != nil:
		log.Printf("新增來源頻道 %s 時寫入儲存頻道失敗：%v", i.ChannelID, err)
		h.edit(i, msgStorageFailed)
		return
	}
	h.hooks.ChannelAdded(i.GuildID, i.ChannelID)
	h.editWithList(i, "成功新增頻道！新增後頻道列表：\n%v", missingPermissionsWarning(i.AppPermissions))
}

// remove 在 deferred 回應之後執行，結果以 edit 補上。
func (h *channelHandler) remove(ctx context.Context, i *discordgo.InteractionCreate) {
	err := h.st.Update(ctx, func(d *store.Data) error {
		if !d.RemoveChannel(i.GuildID, i.ChannelID) {
			return errNotListed
		}
		return nil
	})
	switch {
	case errors.Is(err, errNotListed):
		h.edit(i, "此頻道不存在於頻道列表中")
		return
	case err != nil:
		log.Printf("移除來源頻道 %s 時寫入儲存頻道失敗：%v", i.ChannelID, err)
		h.edit(i, msgStorageFailed)
		return
	}
	h.hooks.ChannelRemoved(i.ChannelID)
	h.editWithList(i, "移除此頻道自頻道列表，目前頻道列表：\n%v", "")
}

func (h *channelHandler) list(i *discordgo.InteractionCreate) {
	h.replyWithList(i, "頻道列表：\n%v", "")
}

// replyWithList 以 format 包住目前伺服器的頻道列表回覆；suffix 接在最後。
func (h *channelHandler) replyWithList(i *discordgo.InteractionCreate, format, suffix string) {
	h.reply(i, fmt.Sprintf(format, h.channelList(i.GuildID))+suffix)
}

// editWithList 同 replyWithList，但以 edit 補上 deferred 回應的內容。
func (h *channelHandler) editWithList(i *discordgo.InteractionCreate, format, suffix string) {
	h.edit(i, fmt.Sprintf(format, h.channelList(i.GuildID))+suffix)
}

// channelList 回傳 "伺服器：X\n[a、b]\n"；沒有頻道時回空字串。
// state 查不到的伺服器顯示伺服器 ID、查不到的頻道（例如 bot 離線時被刪除）顯示頻道 ID。
func (h *channelHandler) channelList(guildID string) string {
	ids := h.st.Snapshot().Channels(guildID)
	if len(ids) == 0 {
		return ""
	}
	guildName := guildID
	if guild, err := h.state.Guild(guildID); err == nil {
		guildName = guild.Name
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		ch, err := h.state.Channel(id)
		if err != nil {
			names = append(names, id)
			continue
		}
		names = append(names, ch.Name)
	}
	return fmt.Sprintf("伺服器：%v\n[%v]\n", guildName, strings.Join(names, "、"))
}

func missingPermissionsWarning(perms int64) string {
	if perms&discordgo.PermissionAdministrator != 0 {
		return ""
	}
	var missing []string
	for _, p := range botPermissions {
		if perms&p.perm == 0 {
			missing = append(missing, p.name)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "\n⚠️ 機器人在此頻道缺少以下權限，圖片可能無法正常處理：" + strings.Join(missing, "、")
}

func (h *channelHandler) reply(i *discordgo.InteractionCreate, content string) {
	respondEphemeral(h.api, i, content)
}

func (h *channelHandler) edit(i *discordgo.InteractionCreate, content string) {
	editResponse(h.api, i, content)
}

// ChannelDeleted 處理頻道被刪除：若是來源頻道，從設定移除並寫回儲存頻道，成功後通知索引。
// 寫入失敗只記 log（記憶體中的資料不變）。
func ChannelDeleted(ctx context.Context, st *store.Store, hooks IndexHooks, guildID, channelID string) {
	err := st.Update(ctx, func(d *store.Data) error {
		if !d.RemoveChannel(guildID, channelID) {
			return errNotListed
		}
		return nil
	})
	switch {
	case errors.Is(err, errNotListed):
		return
	case err != nil:
		log.Printf("來源頻道 %s 已被刪除，但寫入儲存頻道失敗：%v", channelID, err)
		return
	}
	log.Printf("來源頻道 %s 已被刪除，已從設定移除", channelID)
	hooks.ChannelRemoved(channelID)
}

// GuildRemoved 處理 bot 離開伺服器（被踢出或伺服器被刪除）：移除該伺服器所有來源頻道並寫回，
// 成功後對每個被移除的頻道通知索引。沒有來源頻道時不寫入；寫入失敗只記 log。
func GuildRemoved(ctx context.Context, st *store.Store, hooks IndexHooks, guildID string) {
	removeGuildChannels(ctx, st, hooks, guildID, func(string, string) bool { return true }, "bot 已離開伺服器")
}

// PruneMissingChannels 比對伺服器實際存在的頻道（GUILD_CREATE 事件中的頻道 ID），
// 移除設定中已不存在的來源頻道並寫回，成功後對每個被移除的頻道通知索引。沒有變動時不寫入；寫入失敗只記 log。
// 只移除設定時間點（setAt snowflake）早於 receivedAt（收到 GUILD_CREATE 的時間）的來源頻道，
// 避免把事件之後才新增的頻道誤刪；setAt 無法解析時無法判斷先後，不移除。
func PruneMissingChannels(ctx context.Context, st *store.Store, hooks IndexHooks, guildID string, existingIDs []string, receivedAt time.Time) {
	exists := make(map[string]bool, len(existingIDs))
	for _, id := range existingIDs {
		exists[id] = true
	}
	shouldRemove := func(id, setAt string) bool {
		if exists[id] {
			return false
		}
		t, err := discordgo.SnowflakeTimestamp(setAt)
		return err == nil && t.Before(receivedAt)
	}
	removeGuildChannels(ctx, st, hooks, guildID, shouldRemove, "頻道已不存在")
}

// removeGuildChannels 移除伺服器中 shouldRemove 為 true 的來源頻道並寫回；reason 用於 log。
func removeGuildChannels(ctx context.Context, st *store.Store, hooks IndexHooks, guildID string, shouldRemove func(channelID, setAt string) bool, reason string) {
	var removed []string
	err := st.Update(ctx, func(d *store.Data) error {
		removed = nil
		for _, id := range d.Channels(guildID) {
			if shouldRemove(id, d.Guilds[guildID][id]) && d.RemoveChannel(guildID, id) {
				removed = append(removed, id)
			}
		}
		if len(removed) == 0 {
			return errNotListed
		}
		return nil
	})
	switch {
	case errors.Is(err, errNotListed):
		return
	case err != nil:
		log.Printf("伺服器 %s 的來源頻道 %v（%s）移除時寫入儲存頻道失敗：%v", guildID, removed, reason, err)
		return
	}
	log.Printf("伺服器 %s 的來源頻道 %v（%s）已從設定移除", guildID, removed, reason)
	for _, id := range removed {
		hooks.ChannelRemoved(id)
	}
}

// deferEphemeral 送出 ephemeral 的 deferred 回應；失敗時記 log 並回 false。
func deferEphemeral(api API, i *discordgo.InteractionCreate) bool {
	err := api.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})
	if err != nil {
		log.Printf("回覆 interaction %s（deferred）失敗：%v", i.ID, err)
		return false
	}
	return true
}

// editResponse 補上 deferred 回應的內容。
func editResponse(api API, i *discordgo.InteractionCreate, content string) {
	if _, err := api.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content}); err != nil {
		log.Printf("補上 interaction %s 的回應失敗：%v", i.ID, err)
	}
}

func respondEphemeral(api API, i *discordgo.InteractionCreate, content string) {
	err := api.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
	if err != nil {
		log.Printf("回覆 interaction %s 失敗：%v", i.ID, err)
	}
}
