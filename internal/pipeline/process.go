package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"log"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"discordbot/internal/imagemsg"
	"discordbot/internal/recognize"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// 原訊息上的處理狀態（Q19）：處理中加 ⏳；失敗而保留原訊息時移除 ⏳ 並加 ❌。
const (
	reactionProcessing = "⏳"
	reactionFailed     = "❌"
)

// ErrNoImages 表示訊息中沒有要處理的圖片（例如補處理重讀後網址被編輯掉、附件被移除、只剩 gifv）：
// 不發、不刪原訊息、不加 ❌。只有 Reprocess 會收到這個錯誤，其他排入方式回 nil。
var ErrNoImages = errors.New("message has no images to process")

// HandleMessage 判斷新訊息是否要自動處理，要的話呼叫 Process（錯誤只記 log）。
// 只處理一般使用者（不含 bot、webhook，Q50）在來源頻道發的、有圖片（圖片附件或圖片網址，不含 gifv，U3）
// 且不含非圖片附件的訊息。只含圖片網址的訊息必須在圖片網址功能上線時間點之後（store.IsNewMessage，U1）。
// 訊息含尚無預覽的連結（recognize.UnresolvedLinks）時，等 EmbedWait 後重讀訊息再判定（預覽是非同步產生的），
// 期間 ctx 結束就放棄；重讀後仍未判定的連結當一般連結。
func (p *Pipeline) HandleMessage(ctx context.Context, m *discordgo.Message) {
	if m == nil || m.Author == nil || m.Author.Bot || m.WebhookID != "" {
		return
	}
	d := p.Store.Snapshot()
	guildID, setAt, ok := d.SourceChannel(m.ChannelID)
	if !ok {
		return
	}
	if len(m.Attachments) == 0 && !store.IsNewMessage(m.ID, setAt, d.URLSince, true) {
		return
	}
	if recognize.UnresolvedLinks(m) {
		p.waitForEmbeds(ctx, guildID, m)
		return
	}
	if !hasSources(m) {
		return
	}
	if err := p.Process(ctx, guildID, m); err != nil {
		log.Printf("處理訊息 %s/%s 失敗：%v", m.ChannelID, m.ID, err)
	}
}

// hasSources 回傳訊息是否有新訊息要處理的圖片（不含 gifv）且不含非圖片附件。
func hasSources(m *discordgo.Message) bool {
	srcs, ok := recognize.NewMessageSources(m)
	return ok && len(srcs) > 0
}

// embedWait 回傳等待預覽的時間。
func (p *Pipeline) embedWait() time.Duration {
	if p.EmbedWait > 0 {
		return p.EmbedWait
	}
	return defaultEmbedWait
}

// waitForEmbeds 等 EmbedWait 後重讀訊息，有圖片就排入佇列（同補處理：輪到時再重讀）。
// ctx 先結束、訊息已刪除、帶 ❌ 或沒有圖片時放棄。
func (p *Pipeline) waitForEmbeds(ctx context.Context, guildID string, m *discordgo.Message) {
	t := time.NewTimer(p.embedWait())
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
	}
	fresh, ok := p.refetch(m)
	if !ok || HasOwnReaction(fresh, reactionFailed) || !hasSources(fresh) {
		return
	}
	if err := <-p.EnqueueBackfill(ctx, guildID, fresh); err != nil {
		log.Printf("處理訊息 %s/%s 失敗：%v", m.ChannelID, m.ID, err)
	}
}

// refetch 重讀訊息；已刪除（404）或其他錯誤時回 false（其他錯誤記 log）。
func (p *Pipeline) refetch(m *discordgo.Message) (*discordgo.Message, bool) {
	fresh, err := p.API.ChannelMessage(m.ChannelID, m.ID)
	if err != nil {
		if !isNotFound(err) {
			log.Printf("重讀訊息 %s/%s 失敗：%v", m.ChannelID, m.ID, err)
		}
		return nil, false
	}
	return fresh, true
}

// isNotFound 回傳 err 是否為 Discord REST 404。
func isNotFound(err error) bool {
	var rest *discordgo.RESTError
	return errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusNotFound
}

// HandleMessageUpdate 處理來源頻道訊息的更新事件（預覽補上，或使用者編輯加上圖片網址，U2）：
// 一般使用者、新訊息（store.IsNewMessage）的訊息，重讀完整訊息（更新事件可能是部分的，也沒有反應資訊）後，
// 沒有帶 ❌、有圖片且沒有尚無預覽的連結時排入佇列（佇列以訊息 ID 去重；輪到時再重讀）。
// 已刪除、帶 ❌、沒有圖片時不處理。等待處理完成才返回（錯誤只記 log）。
func (p *Pipeline) HandleMessageUpdate(ctx context.Context, m *discordgo.Message) {
	if m == nil || (m.Author != nil && (m.Author.Bot || m.WebhookID != "")) {
		return
	}
	d := p.Store.Snapshot()
	_, setAt, ok := d.SourceChannel(m.ChannelID)
	// 附件資訊不明（部分更新）時以附件規則（只看 setAt）預先篩選，重讀後再依實際內容判斷。
	if !ok || !store.IsNewMessage(m.ID, setAt, d.URLSince, m.Attachments != nil && len(m.Attachments) == 0) {
		return
	}
	fresh, ok := p.refetch(m)
	if !ok || fresh.Author == nil || fresh.Author.Bot || fresh.WebhookID != "" || HasOwnReaction(fresh, reactionFailed) {
		return
	}
	d = p.Store.Snapshot()
	guildID, setAt, ok := d.SourceChannel(fresh.ChannelID)
	if !ok || !store.IsNewMessage(fresh.ID, setAt, d.URLSince, len(fresh.Attachments) == 0) {
		return
	}
	if recognize.UnresolvedLinks(fresh) || !hasSources(fresh) {
		return
	}
	if err := <-p.EnqueueBackfill(ctx, guildID, fresh); err != nil {
		log.Printf("處理訊息 %s/%s 失敗：%v", fresh.ChannelID, fresh.ID, err)
	}
}

// Process 把一則來源頻道的圖片訊息排入佇列（見 Enqueue）並等待處理完成：
// 下載全部圖片（附件：超過 25MB 不下載、長度與 Size 不符視為下載失敗；網址圖：經 URLFetch 下載，
// 必須能解碼成 PNG／JPEG／GIF／WEBP，U5）→ 逐張辨識（全部完成）→
// 每張發一則圖片訊息（重新上傳，mention 一律不通知，原訊息去掉圖片網址後的文字放進關鍵字）→
// 每則成功就加入索引 → 全部發完才刪除原訊息。
// 任何下載失敗都不發出任何圖片訊息；任何失敗都不刪除原訊息，並在原訊息加 ❌（Q24、Q25）。
// 訊息中沒有要處理的圖片（gifv 不算，U3）時不發、不刪、不加 ❌。
// ctx 結束（bot 關機）時不發出、不刪除、不加 ❌，留給下次啟動補處理。
func (p *Pipeline) Process(ctx context.Context, guildID string, m *discordgo.Message) error {
	return <-p.Enqueue(ctx, guildID, m)
}

// sourceFile 是下載好、要重新上傳的一張圖。
type sourceFile struct {
	name, contentType string
	data              []byte
}

func (p *Pipeline) process(ctx context.Context, guildID string, m *discordgo.Message) error {
	srcs, ok := recognize.NewMessageSources(m)
	if !ok || len(srcs) == 0 {
		return ErrNoImages
	}
	files := make([]sourceFile, len(srcs))
	for i, src := range srcs {
		f, err := p.downloadSource(ctx, src)
		if err != nil {
			return err
		}
		files[i] = f
	}

	keywords := imagemsg.NormalizeList(recognize.TextWithoutLinks(m.Content, srcs))
	contents := make([]string, len(files))
	for i, f := range files {
		fields := p.Recognize(ctx, f.data)
		fields.Keywords = keywords
		contents[i] = imagemsg.Format(fields, m.Author.ID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var sendErrs []error
	for i, f := range files {
		sent, err := p.API.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content:         contents[i],
			Files:           []*discordgo.File{{Name: f.name, ContentType: f.contentType, Reader: bytes.NewReader(f.data)}},
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}, // Q20、決定 6：只顯示 mention，不通知
		})
		if err != nil {
			sendErrs = append(sendErrs, fmt.Errorf("發送第 %d 張（%s）失敗：%w", i+1, f.name, err))
			continue
		}
		if p.Index != nil {
			p.Index.PutImageMessage(guildID, sent)
		}
	}
	if len(sendErrs) > 0 {
		return errors.Join(sendErrs...)
	}
	if err := p.API.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
		return fmt.Errorf("刪除原訊息失敗：%w", err)
	}
	return nil
}

// downloadSource 下載一張圖：附件見 download；網址圖見 downloadLink。
func (p *Pipeline) downloadSource(ctx context.Context, src recognize.ImageSource) (sourceFile, error) {
	if !src.IsLink() {
		data, err := p.download(ctx, src.Attachment)
		return sourceFile{name: src.Attachment.Filename, contentType: src.Attachment.ContentType, data: data}, err
	}
	return p.downloadLink(ctx, src)
}

// linkFormats 是網址圖支援的格式（image.DecodeConfig 的格式名稱）與對應副檔名（第一個為預設）。
var linkFormats = map[string][]string{
	"png":  {".png"},
	"jpeg": {".jpg", ".jpeg"},
	"gif":  {".gif"},
	"webp": {".webp"},
}

// downloadLink 下載網址圖（src.FetchURL），確認能解碼成 PNG／JPEG／GIF／WEBP（否則 ErrNotImage，U5），
// 決定上傳的檔名與類型。
func (p *Pipeline) downloadLink(ctx context.Context, src recognize.ImageSource) (sourceFile, error) {
	if p.URLFetch == nil {
		return sourceFile{}, fmt.Errorf("下載網址 %s 失敗：沒有網址下載器", src.URL)
	}
	data, err := p.URLFetch.Fetch(ctx, src.FetchURL)
	if err != nil {
		return sourceFile{}, fmt.Errorf("下載網址 %s 失敗：%w", src.URL, err)
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return sourceFile{}, fmt.Errorf("網址 %s：%w（%v）", src.URL, ErrNotImage, err)
	}
	if linkFormats[format] == nil {
		return sourceFile{}, fmt.Errorf("網址 %s：%w（格式 %s）", src.URL, ErrNotImage, format)
	}
	return sourceFile{name: linkFilename(src, format), contentType: "image/" + format, data: data}, nil
}

// maxLinkStem 是網址圖檔名主檔名的長度上限。
const maxLinkStem = 64

// linkFilename 回傳網址圖上傳的檔名：網址路徑最後一段（只留英數與 ._-）；副檔名與實際格式不符、
// 沒有副檔名或主檔名為空時改為 image.<格式>；劇透時加 SPOILER_ 前綴（U7）。
func linkFilename(src recognize.ImageSource, format string) string {
	name := ""
	if u, err := url.Parse(src.FetchURL); err == nil {
		base := path.Base(u.Path)
		ext := path.Ext(base)
		stem := strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
				return r
			}
			return -1
		}, strings.TrimSuffix(base, ext))
		if len(stem) > maxLinkStem {
			stem = stem[:maxLinkStem]
		}
		if stem != "" && slices.Contains(linkFormats[format], strings.ToLower(ext)) {
			name = stem + ext
		}
	}
	if name == "" {
		name = "image" + linkFormats[format][0]
	}
	if src.Spoiler {
		name = "SPOILER_" + name
	}
	return name
}

// download 下載附件：超過 25MB 不下載；長度與 Size 不符視為下載失敗。
func (p *Pipeline) download(ctx context.Context, a *discordgo.MessageAttachment) ([]byte, error) {
	if a.Size > MaxAttachmentSize {
		return nil, fmt.Errorf("附件 %s：%w（%d bytes）", a.Filename, ErrTooLarge, a.Size)
	}
	data, err := p.Fetch.Fetch(ctx, a.URL)
	if err != nil {
		return nil, fmt.Errorf("下載附件 %s 失敗：%w", a.Filename, err)
	}
	if len(data) != a.Size {
		return nil, fmt.Errorf("下載附件 %s 失敗：%w（預期 %d bytes，得到 %d）", a.Filename, ErrSizeMismatch, a.Size, len(data))
	}
	return data, nil
}

// react 在原訊息加上 bot 的反應；失敗只記 log。
func (p *Pipeline) react(m *discordgo.Message, emoji string) {
	if err := p.API.MessageReactionAdd(m.ChannelID, m.ID, emoji); err != nil {
		log.Printf("在訊息 %s/%s 加 %s 失敗：%v", m.ChannelID, m.ID, emoji, err)
	}
}

// unreact 移除 bot 在原訊息上的反應；失敗只記 log。
func (p *Pipeline) unreact(m *discordgo.Message, emoji string) {
	if err := p.API.MessageReactionRemove(m.ChannelID, m.ID, emoji, "@me"); err != nil {
		log.Printf("移除訊息 %s/%s 的 %s 失敗：%v", m.ChannelID, m.ID, emoji, err)
	}
}
