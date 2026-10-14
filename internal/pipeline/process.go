package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"

	"discordbot/internal/imagemsg"
	"discordbot/internal/recognize"

	"github.com/bwmarrin/discordgo"
)

// 原訊息上的處理狀態（Q19）：處理中加 ⏳；失敗而保留原訊息時移除 ⏳ 並加 ❌。
const (
	reactionProcessing = "⏳"
	reactionFailed     = "❌"
)

// HandleMessage 判斷新訊息是否要自動處理，要的話呼叫 Process（錯誤只記 log）。
// 只處理一般使用者（不含 bot、webhook，Q50）在來源頻道發的、附件全是圖片的訊息。
func (p *Pipeline) HandleMessage(ctx context.Context, m *discordgo.Message) {
	if m == nil || m.Author == nil || m.Author.Bot || m.WebhookID != "" {
		return
	}
	guildID, _, ok := p.Store.Snapshot().SourceChannel(m.ChannelID)
	if !ok || !recognize.Eligible(m) {
		return
	}
	if err := p.Process(ctx, guildID, m); err != nil {
		log.Printf("處理訊息 %s/%s 失敗：%v", m.ChannelID, m.ID, err)
	}
}

// Process 處理一則來源頻道的圖片訊息：
// 下載全部附件（超過 25MB 不下載）→ 逐張辨識（全部完成）→ 每張發一則圖片訊息（重新上傳原檔，
// 上傳者 mention 不通知，原訊息文字放進關鍵字）→ 每則成功就加入索引 → 全部發完才刪除原訊息。
// 任何下載失敗都不發出任何圖片訊息；任何失敗都不刪除原訊息，並在原訊息加 ❌（Q24、Q25）。
// ctx 結束（bot 關機）時不發出、不刪除、不加 ❌，留給下次啟動補處理。
func (p *Pipeline) Process(ctx context.Context, guildID string, m *discordgo.Message) error {
	p.react(m, reactionProcessing)
	err := p.process(ctx, guildID, m)
	if err == nil {
		return nil
	}
	p.unreact(m, reactionProcessing)
	if ctx.Err() == nil {
		p.react(m, reactionFailed)
	}
	return err
}

func (p *Pipeline) process(ctx context.Context, guildID string, m *discordgo.Message) error {
	files := make([][]byte, len(m.Attachments))
	for i, a := range m.Attachments {
		if a.Size > MaxAttachmentSize {
			return fmt.Errorf("附件 %s：%w（%d bytes）", a.Filename, ErrTooLarge, a.Size)
		}
		data, err := p.Fetch.Fetch(ctx, a.URL)
		if err != nil {
			return fmt.Errorf("下載附件 %s 失敗：%w", a.Filename, err)
		}
		files[i] = data
	}

	keywords := imagemsg.NormalizeList(m.Content)
	contents := make([]string, len(files))
	for i, data := range files {
		f := p.Recognize(ctx, data)
		f.Keywords = keywords
		contents[i] = imagemsg.Format(f, m.Author.ID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var sendErrs []error
	for i, a := range m.Attachments {
		sent, err := p.API.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content:         contents[i],
			Files:           []*discordgo.File{{Name: a.Filename, ContentType: a.ContentType, Reader: bytes.NewReader(files[i])}},
			AllowedMentions: &discordgo.MessageAllowedMentions{}, // Q20：只顯示 mention，不通知
		})
		if err != nil {
			sendErrs = append(sendErrs, fmt.Errorf("發送第 %d 張（%s）失敗：%w", i+1, a.Filename, err))
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
