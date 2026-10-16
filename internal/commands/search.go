package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"discordbot/internal/access"
	"discordbot/internal/imagemsg"
	"discordbot/internal/index"
	"discordbot/internal/pipeline"

	"github.com/bwmarrin/discordgo"
)

// /search 的回覆文字（ephemeral）。
const (
	msgSearchImageGone  = "找不到這張圖片，可能已被刪除" // 找不到圖或沒權限（不區分，Q40）
	msgSearchNoMatch    = "找不到符合的圖片"       // 直接送出自己打的字、沒有符合（Q35）
	msgSearchPostFailed = "貼圖失敗，請稍後再試"     // 下載或上傳失敗（暫定）
)

const (
	// searchOption 是 /search 的參數名稱（Q32）。
	searchOption = "關鍵字"
	// searchLimit 是 autocomplete 的提示數上限（Discord 上限 25）。
	searchLimit = 25
	// searchPostSlots 是同時貼圖（讀訊息、下載、上傳）的數量上限：部署機記憶體小，排到才下載。
	searchPostSlots = 1
	// choiceNameMax 是提示文字的長度上限（Discord 上限 100 字）。
	choiceNameMax = 100
	// choiceNameEmpty 是沒有任何文字可顯示時的提示文字（暫定）。
	choiceNameEmpty = "（沒有文字）"
)

// SearchAPI 是 /search 需要的 Discord 方法，*discordgo.Session 直接滿足。
type SearchAPI interface {
	API
	ChannelMessage(channelID, messageID string, options ...discordgo.RequestOption) (*discordgo.Message, error)
	InteractionResponseDelete(interaction *discordgo.Interaction, options ...discordgo.RequestOption) error
	FollowupMessageCreate(interaction *discordgo.Interaction, wait bool, data *discordgo.WebhookParams, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// SearchAccess 回傳使用者要搜尋的頻道（*access.Resolver 直接滿足：可用頻道 ∩ 使用者勾選）。
type SearchAccess interface {
	Selected(ctx context.Context, userID string) ([]access.ChannelRef, error)
}

// SearchIndex 是 /search 使用的索引方法（*index.Index 直接滿足）。
type SearchIndex interface {
	Search(query string, allowed map[string]bool, limit int) []index.Entry
	Get(k index.Key) (index.Entry, bool)
	RemoveMessage(channelID, messageID string)
}

// Search 實作 /search 的 autocomplete 與送出。可用場景：伺服器、與 bot 的私訊、其他私訊與群組（Q41）。
type Search struct {
	API    SearchAPI
	Access SearchAccess
	Index  SearchIndex
	Fetch  pipeline.Fetcher
	// Ctx 是 bot 關機時才取消的 ctx：排隊等貼圖時關機就放棄；nil 時只用 handler 的 ctx。
	Ctx context.Context

	slotOnce sync.Once
	slots    chan struct{} // 容量 searchPostSlots
}

// optionValue 回傳 /search 參數的字串值（沒有或型別不符時為空字串）。
func optionValue(i *discordgo.InteractionCreate) string {
	for _, o := range i.ApplicationCommandData().Options {
		if o != nil && o.Name == searchOption {
			v, _ := o.Value.(string)
			return v
		}
	}
	return ""
}

// selected 回傳使用者要搜尋的頻道：頻道 ID → 伺服器 ID。
func (s *Search) selected(ctx context.Context, userID string) (map[string]string, error) {
	refs, err := s.Access.Selected(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(refs))
	for _, r := range refs {
		out[r.ChannelID] = r.GuildID
	}
	return out, nil
}

func allowedSet(chs map[string]string) map[string]bool {
	out := make(map[string]bool, len(chs))
	for ch := range chs {
		out[ch] = true
	}
	return out
}

// Autocomplete 回傳最多 25 個提示：使用者要搜尋的頻道中符合輸入的圖（空輸入為最新的圖，Q33）；
// 沒有符合時不顯示提示（Q34）。可用頻道的成員查詢約 2 秒截止，用已查到的結果（Q51，Resolver.Deadline）；
// 啟動建索引期間回傳已建好的部分（Q52）。Value 為 index.Key 字串。
func (s *Search) Autocomplete(ctx context.Context, i *discordgo.InteractionCreate) {
	var out []*discordgo.ApplicationCommandOptionChoice
	chs, err := s.selected(ctx, interactionUserID(i.Interaction))
	if err != nil {
		log.Printf("interaction %s：查詢可用頻道失敗：%v", i.ID, err)
	} else {
		for _, e := range s.Index.Search(optionValue(i), allowedSet(chs), searchLimit) {
			out = append(out, &discordgo.ApplicationCommandOptionChoice{Name: ChoiceName(e), Value: e.Key.String()})
		}
	}
	err = s.API.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: out},
	})
	if err != nil {
		log.Printf("回覆 interaction %s（autocomplete）失敗：%v", i.ID, err)
	}
}

// ChoiceName 回傳提示文字（Q38）：
//   - 圖片訊息：「作品｜圖片文字」，空欄省略、失敗欄位顯示「⚠️」；兩欄都空時改顯示「角色｜關鍵字」（暫定），
//     全部空白為 choiceNameEmpty；
//   - 舊圖片訊息：訊息文字開頭；
//   - 換行與連續空白換成一個空白；一則訊息有多張圖時加「（第 N 張）」；
//   - 超過 100 字（rune）時截斷文字並加「…」（編號保留）。
func ChoiceName(e index.Entry) string {
	var body string
	if e.Kind == index.KindLegacy {
		body = oneLine(e.LegacyText)
	} else {
		body = joinShown(e.Fields.Work, e.Fields.Text)
		if body == "" {
			body = joinShown(e.Fields.Characters, e.Fields.Keywords)
		}
	}
	if body == "" {
		body = choiceNameEmpty
	}
	suffix := ""
	if e.AttachmentCount > 1 {
		suffix = fmt.Sprintf("（第 %d 張）", e.Key.Attachment+1)
	}
	room := choiceNameMax - len([]rune(suffix))
	if r := []rune(body); len(r) > room {
		body = string(r[:room-1]) + "…"
	}
	return body + suffix
}

// joinShown 以「｜」連接非空的欄位（失敗標記顯示為「⚠️」）。
func joinShown(values ...string) string {
	var parts []string
	for _, v := range values {
		switch v = oneLine(v); v {
		case "":
		case imagemsg.FailedMarker:
			parts = append(parts, "⚠️")
		default:
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, "｜")
}

// oneLine 把換行與連續空白換成一個空白，並去除前後空白。
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Submit 是 /search 送出的 handler：
//  1. 值是 index.Key（從提示選的）→ 該圖必須在索引中，且所在頻道（與伺服器）在使用者要搜尋的頻道內——
//     值由 client 傳來、可被偽造，所以重新檢查；否則 ephemeral「找不到這張圖片」（不區分沒權限與不存在，Q40）。
//     值不是 Key（直接送出自己打的字）→ 貼出第一個符合的結果；沒有符合 → ephemeral「找不到符合的圖片」（Q35）。
//  2. 回 deferred 公開回應 → 排隊（同時貼圖數有上限，排到才讀訊息與下載）→ 重新抓訊息取得最新附件網址 →
//     下載 → 以附件重新上傳，只發圖片（Q39）。
//  3. deferred 之後才發現錯誤：刪除 deferred 回應，改發 ephemeral followup。訊息已不存在（404）時從索引移除。
func (s *Search) Submit(ctx context.Context, i *discordgo.InteractionCreate) {
	value := optionValue(i)
	chs, err := s.selected(ctx, interactionUserID(i.Interaction))
	if err != nil {
		log.Printf("interaction %s：查詢可用頻道失敗：%v", i.ID, err)
		respondEphemeral(s.API, i, msgSearchImageGone)
		return
	}
	var key index.Key
	if k, err := index.ParseKey(value); err == nil {
		e, ok := s.Index.Get(k)
		if g, allowed := chs[k.ChannelID]; !ok || !allowed || e.GuildID != g {
			respondEphemeral(s.API, i, msgSearchImageGone)
			return
		}
		key = k
	} else {
		found := s.Index.Search(value, allowedSet(chs), 1)
		if len(found) == 0 {
			respondEphemeral(s.API, i, msgSearchNoMatch)
			return
		}
		key = found[0].Key
	}

	err = s.API.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		log.Printf("回覆 interaction %s（deferred）失敗：%v", i.ID, err)
		return
	}
	if msg := s.post(ctx, i, key); msg != "" {
		s.failAfterDefer(i, msg)
	}
}

// post 排到後讀訊息、下載並以附件補上 deferred 回應；失敗時回傳要告知使用者的文字。
func (s *Search) post(ctx context.Context, i *discordgo.InteractionCreate, key index.Key) string {
	ctx, cancel := withShutdown(ctx, s.Ctx)
	defer cancel()
	release := s.acquire(ctx)
	if release == nil {
		log.Printf("interaction %s：等待貼圖時中斷：%v", i.ID, ctx.Err())
		return msgRestarting
	}
	defer release()

	m, err := s.API.ChannelMessage(key.ChannelID, key.MessageID)
	if err != nil {
		var rest *discordgo.RESTError
		if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusNotFound {
			s.Index.RemoveMessage(key.ChannelID, key.MessageID)
		} else {
			log.Printf("/search：讀取訊息 %s/%s 失敗：%v", key.ChannelID, key.MessageID, err)
		}
		return msgSearchImageGone
	}
	if key.Attachment >= len(m.Attachments) || m.Attachments[key.Attachment] == nil {
		return msgSearchImageGone
	}
	a := m.Attachments[key.Attachment]
	data, err := download(ctx, s.Fetch, a)
	if err != nil {
		log.Printf("/search：下載 %s/%s 的附件失敗：%v", key.ChannelID, key.MessageID, err)
		if ctx.Err() != nil {
			return msgRestarting
		}
		return msgSearchPostFailed
	}
	_, err = s.API.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Files: []*discordgo.File{{Name: a.Filename, ContentType: a.ContentType, Reader: bytes.NewReader(data)}},
	})
	if err != nil {
		log.Printf("/search：上傳圖片（interaction %s）失敗：%v", i.ID, err)
		return msgSearchPostFailed
	}
	return ""
}

// acquire 取得貼圖位置，回傳釋放函式；ctx 先結束時回 nil。
func (s *Search) acquire(ctx context.Context) (release func()) {
	s.slotOnce.Do(func() { s.slots = make(chan struct{}, searchPostSlots) })
	if ctx.Err() != nil {
		return nil
	}
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }
	case <-ctx.Done():
		return nil
	}
}

// failAfterDefer 刪除 deferred 公開回應，改發 ephemeral followup 告知 msg。
func (s *Search) failAfterDefer(i *discordgo.InteractionCreate, msg string) {
	if err := s.API.InteractionResponseDelete(i.Interaction); err != nil {
		log.Printf("刪除 interaction %s 的回應失敗：%v", i.ID, err)
	}
	_, err := s.API.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: msg,
		Flags:   discordgo.MessageFlagsEphemeral,
	})
	if err != nil {
		log.Printf("interaction %s 的 followup 失敗：%v", i.ID, err)
	}
}

// download 下載附件：超過 25MiB 不下載；長度與 Size 不符視為下載失敗（同自動處理）。
func download(ctx context.Context, f pipeline.Fetcher, a *discordgo.MessageAttachment) ([]byte, error) {
	if a.Size > pipeline.MaxAttachmentSize {
		return nil, fmt.Errorf("附件 %s：%w（%d bytes）", a.Filename, pipeline.ErrTooLarge, a.Size)
	}
	data, err := f.Fetch(ctx, a.URL)
	if err != nil {
		return nil, err
	}
	if len(data) != a.Size {
		return nil, fmt.Errorf("下載附件 %s 失敗：%w（預期 %d bytes，得到 %d）", a.Filename, pipeline.ErrSizeMismatch, a.Size, len(data))
	}
	return data, nil
}
