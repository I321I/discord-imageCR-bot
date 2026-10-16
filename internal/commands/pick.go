package commands

import (
	"context"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"

	"discordbot/internal/access"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// /sources 的回覆文字（ephemeral）。
const (
	msgSourcesPrompt  = "勾選要搜尋的圖庫頻道（未選過時預設全部）"         // Q48
	msgSourcesNone    = "目前沒有可用的圖庫頻道"                  // Q48
	msgSourcesUpdated = "已更新搜尋頻道"                      // Q45
	msgSourcesTooMany = "（可用的圖庫頻道超過 125 個，只列出前 125 個）" // Q43 的說明（暫定）
)

const (
	// SourcesMenuPrefix 是 /sources 選單的 CustomID 前綴；完整 CustomID 為 "pickch:<選單序號>"。
	SourcesMenuPrefix = "pickch"
	// sourcesPerMenu 是一個選單的選項上限（Discord 上限 25）。
	sourcesPerMenu = 25
	// sourcesMaxMenus 是一則訊息的選單上限（Discord 一則訊息最多 5 列元件），共 125 個頻道（Q43）。
	sourcesMaxMenus = 5
	// sourceLabelMax 是選項文字的長度上限（Discord 上限 100 字，以 UTF-16 計算）。
	sourceLabelMax = 100
)

// SourcesAccess 回傳使用者可用的來源頻道，以及結果是否完整（*access.Resolver 直接滿足）。
type SourcesAccess interface {
	AvailableComplete(ctx context.Context, userID string) (refs []access.ChannelRef, complete bool, err error)
}

// Sources 實作 /sources：使用者勾選 /search 要搜尋的圖庫頻道。可用場景同 /search（伺服器、bot 私訊、其他私訊）。
type Sources struct {
	API    API
	Access SourcesAccess
	Store  *store.Store
}

// sourceOption 是選單中的一個頻道。
type sourceOption struct {
	label, channelID string
}

// Command 是 /sources 的 handler：先回 deferred ephemeral，再列出可用頻道（需檢視頻道＋讀取訊息歷史）。
// 每 25 個一個選單，最多 5 個（125 個頻道），再多只列前 125 個並附說明（Q43）。
// 預設勾選 = 目前的勾選；沒選過 = 全部（Q46、Q47）。可一個都不選（MinValues 0）。
// 沒有可用頻道時：結果完整回「目前沒有可用的圖庫頻道」，不完整回「目前無法確認權限，請稍後再試」。
func (s *Sources) Command(ctx context.Context, i *discordgo.InteractionCreate) {
	err := s.API.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})
	if err != nil {
		log.Printf("回覆 interaction %s（deferred）失敗：%v", i.ID, err)
		return
	}
	userID := interactionUserID(i.Interaction)
	refs, complete, err := s.Access.AvailableComplete(ctx, userID)
	if err != nil {
		log.Printf("interaction %s：查詢可用頻道失敗：%v", i.ID, err)
		editSources(s.API, i, msgPermissionUnknown, nil)
		return
	}
	if len(refs) == 0 {
		// 結果不完整時可能只是暫時查不到（階段 11 決定 3），不說「沒有」。
		msg := msgSourcesNone
		if !complete {
			msg = msgPermissionUnknown
		}
		editSources(s.API, i, msg, nil)
		return
	}
	content := msgSourcesPrompt
	if len(refs) > sourcesPerMenu*sourcesMaxMenus {
		content += "\n" + msgSourcesTooMany
	}
	editSources(s.API, i, content, sourceMenus(pagesOf(refs), s.isSelected(userID)))
}

// pagesOf 把頻道每 25 個分成一頁（最多 5 頁）。
func pagesOf(refs []access.ChannelRef) [][]sourceOption {
	refs = refs[:min(len(refs), sourcesPerMenu*sourcesMaxMenus)]
	var pages [][]sourceOption
	for start := 0; start < len(refs); start += sourcesPerMenu {
		var page []sourceOption
		for _, r := range refs[start:min(start+sourcesPerMenu, len(refs))] {
			page = append(page, sourceOption{label: sourceLabel(r), channelID: r.ChannelID})
		}
		pages = append(pages, page)
	}
	return pages
}

// isSelected 回傳判斷頻道目前是否被勾選的函式（沒選過 = 全部勾選）。
func (s *Sources) isSelected(userID string) func(channelID string) bool {
	ids, chosen := s.Store.Snapshot().UserChannels(userID)
	return func(channelID string) bool { return !chosen || slices.Contains(ids, channelID) }
}

// Component 是 /sources 選單送出的 handler：變更即儲存（Q45）。
// 每個選單只負責自己那一頁的頻道：新的勾選 = 原本的勾選（沒選過 = 目前全部可用頻道）去掉這一頁的頻道，再加上這一頁選到的頻道。
// 這一頁的頻道與選到的值都只接受目前可用的頻道（過濾偽造的值）；看不到但以前選過的頻道不在其中，因此保留（Q47）。
// 寫入失敗回 Q5 文字，設定不變，選單恢復成目前的勾選。
// 沒選過的使用者第一次變更時，若可用頻道結果不完整（某伺服器查詢逾時、失敗或暫時不在 state），
// 本次不儲存，回「目前無法確認權限，請稍後再試」（避免暫時查不到的伺服器被永久排除）。
// CustomID 的頁碼不存在時只重畫選單、不寫入。
func (s *Sources) Component(ctx context.Context, i *discordgo.InteractionCreate) {
	err := s.API.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	})
	if err != nil {
		log.Printf("回覆 interaction %s（deferred update）失敗：%v", i.ID, err)
		return
	}
	userID := interactionUserID(i.Interaction)
	data := i.MessageComponentData()
	pages := shownPages(i.Message)
	refs, complete, err := s.Access.AvailableComplete(ctx, userID)
	if err != nil {
		log.Printf("interaction %s：查詢可用頻道失敗：%v", i.ID, err)
		editSources(s.API, i, msgPermissionUnknown, sourceMenus(pages, s.isSelected(userID)))
		return
	}
	available := make([]string, len(refs))
	for n, r := range refs {
		available[n] = r.ChannelID
	}
	if len(pages) == 0 {
		// 訊息中沒有選單（例如沒有訊息內容）：依目前可用頻道分頁。
		pages = pagesOf(refs)
	}

	n, ok := menuIndex(data.CustomID)
	if !ok || n >= len(pages) {
		// 頁碼不存在：只重畫選單（內容不變），不寫入。
		log.Printf("interaction %s：/sources 選單 CustomID %q 不合法，只重畫選單", i.ID, data.CustomID)
		content := msgSourcesPrompt
		if i.Message != nil && i.Message.Content != "" {
			content = i.Message.Content
		}
		editSources(s.API, i, content, sourceMenus(pages, s.isSelected(userID)))
		return
	}
	var pageIDs []string
	for _, o := range pages[n] {
		if slices.Contains(available, o.channelID) {
			pageIDs = append(pageIDs, o.channelID)
		}
	}
	picked := slices.DeleteFunc(slices.Clone(data.Values), func(id string) bool { return !slices.Contains(pageIDs, id) })

	content := msgSourcesUpdated
	err = s.Store.Update(ctx, func(d *store.Data) error {
		prev, chosen := d.UserChannels(userID)
		if !chosen {
			if !complete {
				return errSourcesIncomplete
			}
			prev = available
		}
		next := slices.DeleteFunc(slices.Clone(prev), func(id string) bool { return slices.Contains(pageIDs, id) })
		for _, id := range picked {
			if !slices.Contains(next, id) {
				next = append(next, id)
			}
		}
		d.SetUserChannels(userID, next)
		return nil
	})
	switch {
	case errors.Is(err, errSourcesIncomplete):
		log.Printf("interaction %s：使用者 %s 的可用頻道結果不完整，第一次變更不儲存", i.ID, userID)
		content = msgPermissionUnknown
	case err != nil:
		log.Printf("儲存使用者 %s 的搜尋頻道失敗：%v", userID, err)
		content = msgStorageFailed
	}
	editSources(s.API, i, content, sourceMenus(pages, s.isSelected(userID)))
}

// errSourcesIncomplete 表示沒選過的使用者第一次變更時可用頻道結果不完整，本次不儲存。
var errSourcesIncomplete = errors.New("available channels incomplete")

// shownPages 取出 /sources 訊息中各選單的選項（使用者看到的頁面）。
// 從 JSON 解出的元件是指標（*ActionsRow、*SelectMenu），自己建立的是值，兩者都接受。
func shownPages(m *discordgo.Message) [][]sourceOption {
	if m == nil {
		return nil
	}
	var pages [][]sourceOption
	for _, c := range m.Components {
		var row discordgo.ActionsRow
		switch v := c.(type) {
		case *discordgo.ActionsRow:
			row = *v
		case discordgo.ActionsRow:
			row = v
		default:
			continue
		}
		for _, rc := range row.Components {
			var menu discordgo.SelectMenu
			switch v := rc.(type) {
			case *discordgo.SelectMenu:
				menu = *v
			case discordgo.SelectMenu:
				menu = v
			default:
				continue
			}
			if n, ok := menuIndex(menu.CustomID); !ok || n != len(pages) {
				continue
			}
			var page []sourceOption
			for _, o := range menu.Options {
				page = append(page, sourceOption{label: o.Label, channelID: o.Value})
			}
			pages = append(pages, page)
		}
	}
	return pages
}

// menuIndex 解析 "pickch:<n>"，n 必須在 0..sourcesMaxMenus-1。
func menuIndex(customID string) (int, bool) {
	p, rest, ok := strings.Cut(customID, ":")
	if !ok || p != SourcesMenuPrefix {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 || n >= sourcesMaxMenus {
		return 0, false
	}
	return n, true
}

// sourceMenus 為每一頁建立一個多選選單（可一個都不選），selected 決定預設勾選。
func sourceMenus(pages [][]sourceOption, selected func(channelID string) bool) []discordgo.MessageComponent {
	zero := 0
	var rows []discordgo.MessageComponent
	for n, page := range pages {
		menu := discordgo.SelectMenu{
			MenuType:  discordgo.StringSelectMenu,
			CustomID:  SourcesMenuPrefix + ":" + strconv.Itoa(n),
			MinValues: &zero,
			MaxValues: len(page),
		}
		for _, o := range page {
			menu.Options = append(menu.Options, discordgo.SelectMenuOption{Label: o.label, Value: o.channelID, Default: selected(o.channelID)})
		}
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{menu}})
	}
	return rows
}

// sourceLabel 回傳選項文字「伺服器名 / #頻道名」（Q44），超過 100 字時截斷並加「…」。
func sourceLabel(r access.ChannelRef) string {
	return truncateUTF16(r.GuildName+" / #"+r.ChannelName, sourceLabelMax)
}

// editSources 補上 /sources 的回應：文字與選單（nil 時清除選單）。
func editSources(api API, i *discordgo.InteractionCreate, content string, components []discordgo.MessageComponent) {
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	_, err := api.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content, Components: &components})
	if err != nil {
		log.Printf("補上 interaction %s 的回應失敗：%v", i.ID, err)
	}
}
