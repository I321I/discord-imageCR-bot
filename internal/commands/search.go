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
	"time"
	"unicode/utf16"

	"discordbot/internal/access"
	"discordbot/internal/imagemsg"
	"discordbot/internal/index"
	"discordbot/internal/pipeline"
	"discordbot/internal/recognize"

	"github.com/bwmarrin/discordgo"
)

// /search 的回覆文字（ephemeral）。
const (
	msgSearchImageGone      = "找不到這張圖片，可能已被刪除"      // 找不到圖或沒權限（不區分，Q40）
	msgSearchNoMatch        = "找不到符合的圖片"            // 直接送出自己打的字、沒有符合（Q35）
	msgSearchEmptyQuery     = "請輸入關鍵字，或從提示中選擇一張圖片"  // 只打空白、沒選或值不是字串
	msgSearchTooLarge       = "圖片超過 25MB，無法貼出"      // 附件 Size > 25MiB，不下載
	msgSearchUploadTooLarge = "圖片太大，超過這裡的上傳上限，無法貼出" // 上傳被 Discord 以檔案太大拒絕（HTTP 413 或 code 40005）
	msgSearchDownloadFailed = "下載圖片失敗，請稍後再試"        // 下載失敗（網路、長度不符）
	msgSearchPostFailed     = "貼圖失敗，請稍後再試"          // 其他上傳錯誤
	msgSearchQueueTimeout   = "目前貼圖的人太多，請稍後再試"      // 排隊超過 MaxWait（暫定）
	msgSearchLinkGone       = "圖床上的圖片已被刪除，無法貼出"     // 網址圖：圖床回 404／410 或內容不是圖片
	msgSearchLinkUnverified = "目前無法確認圖片，請稍後再試"      // 網址圖：無法確認（403、被擋、逾時等）且 Discord 沒有圖片預覽
)

// defaultSearchMaxWait 是排隊等貼圖的預設上限：deferred 回應的 token 只有 15 分鐘，
// 留 1 分鐘給讀訊息、下載與上傳。
const defaultSearchMaxWait = 14 * time.Minute

// defaultLinkCheckTimeout 是貼出網址圖前確認圖片還在的預設逾時（把關後決定 (d)）。
const defaultLinkCheckTimeout = 15 * time.Second

const (
	// searchOption 是 /search 的參數名稱（Q32）。
	searchOption = "關鍵字"
	// searchLimit 是 autocomplete 的提示數上限（Discord 上限 25）。
	searchLimit = 25
	// searchPostSlots 是同時貼圖（讀訊息、下載、上傳）的數量上限：部署機記憶體小，排到才下載。
	searchPostSlots = 1
	// choiceNameMax 是提示文字的長度上限（Discord 上限 100 字）。
	choiceNameMax = 100
	// choiceNameEmpty 是沒有任何文字可顯示時的提示文字（使用者決定）。
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
	RemoveAttachment(k index.Key)
}

// LinkChecker 確認網址上的圖片還在（*pipeline.URLFetcher 直接滿足，防 SSRF）：
// 圖片已被刪除回 pipeline.ErrImageGone，內容不是圖片回 pipeline.ErrNotImage。
type LinkChecker interface {
	Check(ctx context.Context, url string) error
}

// Search 實作 /search 的 autocomplete 與送出。可用場景：伺服器、與 bot 的私訊、其他私訊與群組（Q41）。
type Search struct {
	API    SearchAPI
	Access SearchAccess
	Index  SearchIndex
	Fetch  pipeline.Fetcher
	// LinkCheck 在貼出網址圖前確認圖片還在；nil 時視為無法確認（見 postLink）。
	LinkCheck LinkChecker
	// LinkCheckTimeout 是確認圖片的逾時；0 為 defaultLinkCheckTimeout。
	LinkCheckTimeout time.Duration
	// Ctx 是 bot 關機時才取消的 ctx：排隊等貼圖時關機就放棄；nil 時只用 handler 的 ctx。
	Ctx context.Context
	// MaxWait 是排隊等貼圖的上限，超過就放棄（刪除 deferred 回應、改發 ephemeral followup）；0 為 defaultSearchMaxWait。
	MaxWait time.Duration

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
//   - 圖片訊息：「作品｜圖片文字」，空欄省略、失敗欄位顯示「⚠️」；兩欄都空時改顯示「角色｜關鍵字」（使用者決定），
//     全部空白為 choiceNameEmpty；
//   - 舊圖片訊息：訊息文字開頭；
//   - 換行與連續空白換成一個空白；一則訊息有多張圖時加「（第 N 張）」（第 1 張也標；張數與編號計圖片附件與圖片網址）；
//   - 超過 100 字時截斷文字並加「…」（編號保留）。長度以 UTF-16 單位計算（保守：emoji 等算 2）。
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
		n := e.Number
		if n <= 0 {
			n = e.Key.Attachment + 1 // 沒有編號（舊版 Entry）時退回附件位置
		}
		suffix = fmt.Sprintf("（第 %d 張）", n)
	}
	return truncateUTF16(body, choiceNameMax-utf16Units(suffix)) + suffix
}

// truncateUTF16 在 s 的 UTF-16 長度超過 limit 時截斷並加「…」，結果不超過 limit（Discord 以 UTF-16 計算長度上限）。
func truncateUTF16(s string, limit int) string {
	if utf16Units(s) <= limit {
		return s
	}
	room := limit - 1 // 「…」
	var b strings.Builder
	for _, r := range s {
		if room -= utf16.RuneLen(r); room < 0 {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + "…"
}

// utf16Units 回傳 s 的 UTF-16 長度。
func utf16Units(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
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
//     下載 → 以附件重新上傳，只發圖片（Q39）。網址圖：重新抓訊息確認網址仍在 → 確認圖床上的圖片沒有確定被刪除
//     （LinkCheck）→ 貼出網址（不下載、不重新上傳；劇透時為 ||網址||；mention 不通知）。
//  3. deferred 之後才發現錯誤：刪除 deferred 回應，改發 ephemeral followup。訊息已不存在（404）時從索引移除；
//     附件序號越界、附件 ID 與索引不符或已不是圖片時只移除索引中該筆。
//
// 只打空白、沒選或值不是字串：ephemeral「請輸入關鍵字…」，不貼圖。排隊超過 MaxWait 就放棄。
func (s *Search) Submit(ctx context.Context, i *discordgo.InteractionCreate) {
	value := optionValue(i)
	if strings.TrimSpace(value) == "" {
		respondEphemeral(s.API, i, msgSearchEmptyQuery)
		return
	}
	chs, err := s.selected(ctx, interactionUserID(i.Interaction))
	if err != nil {
		log.Printf("interaction %s：查詢可用頻道失敗：%v", i.ID, err)
		respondEphemeral(s.API, i, msgSearchImageGone)
		return
	}
	var entry index.Entry
	if k, err := index.ParseKey(value); err == nil {
		e, ok := s.Index.Get(k)
		if g, allowed := chs[k.ChannelID]; !ok || !allowed || e.GuildID != g {
			respondEphemeral(s.API, i, msgSearchImageGone)
			return
		}
		entry = e
	} else {
		found := s.Index.Search(value, allowedSet(chs), 1)
		if len(found) == 0 {
			respondEphemeral(s.API, i, msgSearchNoMatch)
			return
		}
		entry = found[0]
	}

	err = s.API.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
	if err != nil {
		log.Printf("回覆 interaction %s（deferred）失敗：%v", i.ID, err)
		return
	}
	if msg := s.post(ctx, i, entry); msg != "" {
		s.failAfterDefer(i, msg)
	}
}

// post 排到後讀訊息、下載並以附件補上 deferred 回應（網址圖見 postLink）；失敗時回傳要告知使用者的文字。
func (s *Search) post(ctx context.Context, i *discordgo.InteractionCreate, entry index.Entry) string {
	key := entry.Key
	ctx, cancel := withShutdown(ctx, s.Ctx)
	defer cancel()
	release, err := s.wait(ctx)
	if err != nil {
		log.Printf("interaction %s：等待貼圖時中斷：%v", i.ID, err)
		if errors.Is(err, errQueueTimeout) {
			return msgSearchQueueTimeout
		}
		return msgRestarting
	}
	defer release()

	m, err := s.API.ChannelMessage(key.ChannelID, key.MessageID)
	if err != nil {
		if restStatus(err) == http.StatusNotFound {
			s.Index.RemoveMessage(key.ChannelID, key.MessageID)
		} else {
			log.Printf("/search：讀取訊息 %s/%s 失敗：%v", key.ChannelID, key.MessageID, err)
		}
		return msgSearchImageGone
	}
	if key.Link {
		return s.postLink(ctx, i, entry, m)
	}
	var a *discordgo.MessageAttachment
	if key.Attachment < len(m.Attachments) {
		a = m.Attachments[key.Attachment]
	}
	if a == nil || (entry.AttachmentID != "" && a.ID != entry.AttachmentID) || !recognize.IsImageAttachment(a) {
		// 索引過時（附件被移除、換掉或不再是圖片）：只移除這一筆。
		s.Index.RemoveAttachment(key)
		return msgSearchImageGone
	}
	data, err := download(ctx, s.Fetch, a)
	if err != nil {
		log.Printf("/search：下載 %s/%s 的附件失敗：%v", key.ChannelID, key.MessageID, err)
		switch {
		case ctx.Err() != nil:
			return msgRestarting
		case errors.Is(err, pipeline.ErrTooLarge):
			return msgSearchTooLarge
		}
		return msgSearchDownloadFailed
	}
	_, err = s.API.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Files: []*discordgo.File{{Name: a.Filename, ContentType: a.ContentType, Reader: bytes.NewReader(data)}},
	})
	if err != nil {
		log.Printf("/search：上傳圖片（interaction %s）失敗：%v", i.ID, err)
		if uploadTooLarge(err) {
			return msgSearchUploadTooLarge
		}
		return msgSearchPostFailed
	}
	return ""
}

// postLink 貼出網址圖（已取得貼圖位置、m 為重新抓到的訊息）：作者是 bot、webhook，或網址已不在該位置 →
// 移除該筆、「找不到」；
// 確認圖床上的圖片（逾時 LinkCheckTimeout）：確定已刪除（404、410、imgur removed.png）或確定不是圖片 →
// 移除該筆、「圖床上的圖片已被刪除」；其他失敗（403、被安全規則擋下、逾時、太大等，或沒有 LinkCheck）時，
// 只有 Discord 對這個網址產生了圖片預覽（recognize.HasImagePreview）才照樣貼出；只靠副檔名判定的回
// 「目前無法確認圖片」、不移除索引（避免公開散播偽裝成圖片的釣魚連結，使用者決定）。
// 以網址補上 deferred 回應（mention 不通知）。失敗時回傳要告知使用者的文字。
func (s *Search) postLink(ctx context.Context, i *discordgo.InteractionCreate, entry index.Entry, m *discordgo.Message) string {
	key := entry.Key
	if m.Author == nil || m.Author.Bot || m.WebhookID != "" {
		// 舊圖片訊息一定是一般使用者發的：作者不符視為找不到（PM 決定）。
		s.Index.RemoveAttachment(key)
		return msgSearchImageGone
	}
	var src *recognize.ImageSource
	for _, x := range recognize.LinkSources(m) {
		if x.Index == key.Attachment && x.URL == entry.ImageURL {
			src = &x
			break
		}
	}
	if src == nil {
		s.Index.RemoveAttachment(key)
		return msgSearchImageGone
	}
	err := errNoLinkCheck
	if s.LinkCheck != nil {
		err = s.checkLink(ctx, src.FetchURL)
	}
	if err != nil {
		log.Printf("/search：確認 %s/%s 的網址圖 %s 失敗：%v", key.ChannelID, key.MessageID, src.FetchURL, err)
		switch {
		case ctx.Err() != nil:
			return msgRestarting
		case errors.Is(err, pipeline.ErrImageGone), errors.Is(err, pipeline.ErrNotImage):
			s.Index.RemoveAttachment(key)
			return msgSearchLinkGone
		case !recognize.HasImagePreview(src.URL, m.Embeds):
			return msgSearchLinkUnverified
		}
	}
	content := src.URL
	if src.Spoiler {
		content = "||" + content + "||"
	}
	_, err = s.API.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
		Content:         &content,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
	if err != nil {
		log.Printf("/search：貼出網址（interaction %s）失敗：%v", i.ID, err)
		return msgSearchPostFailed
	}
	return ""
}

// errNoLinkCheck 表示沒有設定 LinkCheck，無法確認網址圖。
var errNoLinkCheck = errors.New("no link checker")

// checkLink 以 LinkCheckTimeout 的期限確認網址上的圖片還在。
func (s *Search) checkLink(ctx context.Context, url string) error {
	limit := s.LinkCheckTimeout
	if limit <= 0 {
		limit = defaultLinkCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	return s.LinkCheck.Check(ctx, url)
}

// restStatus 回傳 Discord REST 錯誤的 HTTP 狀態碼；不是 REST 錯誤時回 0。
func restStatus(err error) int {
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Response != nil {
		return rest.Response.StatusCode
	}
	return 0
}

// uploadTooLarge 回傳上傳是否被 Discord 以檔案太大拒絕：HTTP 413，或 JSON 錯誤碼 40005（Request entity too large）。
func uploadTooLarge(err error) bool {
	var rest *discordgo.RESTError
	if !errors.As(err, &rest) {
		return false
	}
	if rest.Response != nil && rest.Response.StatusCode == http.StatusRequestEntityTooLarge {
		return true
	}
	return rest.Message != nil && rest.Message.Code == discordgo.ErrCodeRequestEntityTooLarge
}

// errQueueTimeout 表示排隊等貼圖超過 MaxWait。
var errQueueTimeout = errors.New("queue wait exceeded")

// wait 排隊取得貼圖位置，最多等 MaxWait；ctx 先結束時回 ctx 的錯誤，逾時回 errQueueTimeout。
func (s *Search) wait(ctx context.Context) (release func(), err error) {
	limit := s.MaxWait
	if limit <= 0 {
		limit = defaultSearchMaxWait
	}
	wctx, cancel := context.WithTimeoutCause(ctx, limit, errQueueTimeout)
	defer cancel()
	if release = s.acquire(wctx); release != nil {
		return release, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, context.Cause(wctx)
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
