// Package pipeline 處理圖片來源頻道的新訊息：下載附件、辨識、發出圖片訊息、刪除原訊息。
package pipeline

import (
	"context"
	"image"
	"log"
	"slices"
	"sync"

	"discordbot/internal/imagemsg"
	"discordbot/internal/recognize"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// API 是 pipeline 需要的 Discord 方法（*discordgo.Session 直接滿足）。
type API interface {
	ChannelMessage(channelID, messageID string, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageDelete(channelID, messageID string, options ...discordgo.RequestOption) error
	MessageReactionAdd(channelID, messageID, emojiID string, options ...discordgo.RequestOption) error
	MessageReactionRemove(channelID, messageID, emojiID, userID string, options ...discordgo.RequestOption) error
}

// Fetcher 下載附件。
type Fetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// IndexSink 接收發出的圖片訊息（階段 6 的索引）。
type IndexSink interface {
	PutImageMessage(guildID string, m *discordgo.Message)
}

// NopIndex 是不做任何事的 IndexSink（索引完成前使用）。
type NopIndex struct{}

func (NopIndex) PutImageMessage(string, *discordgo.Message) {}

// Pipeline 處理來源頻道的新訊息。Work、Text 由呼叫端包上排隊（recognize.Serialize、SerializeText）。
type Pipeline struct {
	API   API
	Fetch Fetcher
	Work  recognize.WorkRecognizer
	Text  recognize.TextRecognizer
	Store *store.Store
	Index IndexSink

	decodeOnce sync.Once
	decodeSlot chan struct{} // 容量 1：全域同時只解碼一張原圖

	rerecOnce sync.Once
	rerecSlot chan struct{} // 容量 1：「重新辨識」同時只進行一個（排到才下載）

	qmu      sync.Mutex      // 保護以下佇列欄位
	jobs     []*job          // 排隊中的原訊息，依 ID 由舊到新
	pending  map[string]bool // 在佇列中或處理中的訊息 ID
	draining bool            // 是否有處理佇列的 goroutine
}

// acquireDecode 取得解碼位置，回傳釋放函式；ctx 先結束時回 nil。
func (p *Pipeline) acquireDecode(ctx context.Context) (release func()) {
	p.decodeOnce.Do(func() { p.decodeSlot = make(chan struct{}, 1) })
	select {
	case p.decodeSlot <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-p.decodeSlot }) }
	case <-ctx.Done():
		return nil
	}
}

// prepared 是解碼一次後產生、送去辨識的縮圖。
type prepared struct {
	workPNG  []byte      // trace.moe 用（長邊 ≤ 1000px 的 PNG）；編碼失敗時為 nil
	subtitle image.Image // OCR 字幕區
	whole    image.Image // OCR 整張
}

// prepare 取得解碼位置、解碼一次並產生 trace.moe 與 OCR 用的縮圖，之後立即釋放解碼位置；
// 原圖只在此函式內使用，返回後即可被回收。
func (p *Pipeline) prepare(ctx context.Context, data []byte) (*prepared, error) {
	release := p.acquireDecode(ctx)
	if release == nil {
		return nil, ctx.Err()
	}
	defer release()
	img, err := recognize.DecodeFirstFrame(data)
	if err != nil {
		return nil, err
	}
	out := &prepared{
		subtitle: recognize.PrepareForOCR(img),
		whole:    recognize.PrepareWholeForOCR(img),
	}
	if out.workPNG, err = recognize.EncodePNG(recognize.PrepareForTraceMoe(img)); err != nil {
		log.Printf("編碼 trace.moe 用圖片失敗：%v", err)
	}
	return out, nil
}

// Recognize 辨識一張圖片的作品（trace.moe）與圖片文字（OCR），只填 Work、Text；
// 各自失敗（含沒讀到字、解碼失敗、尺寸超過上限）時該欄為 imagemsg.FailedMarker。
// 同一時間只解碼一張；解碼並產生縮圖後就釋放原圖與解碼位置，trace.moe 與 OCR 並行（各自已排隊）。
func (p *Pipeline) Recognize(ctx context.Context, data []byte) imagemsg.Fields {
	return p.recognize(ctx, data, true, true)
}

// recognize 只辨識 work、text 為 true 的欄位（同 Recognize）；沒要求的欄位留空。
func (p *Pipeline) recognize(ctx context.Context, data []byte, work, text bool) imagemsg.Fields {
	var f imagemsg.Fields
	if work {
		f.Work = imagemsg.FailedMarker
	}
	if text {
		f.Text = imagemsg.FailedMarker
	}
	pre, err := p.prepare(ctx, data)
	if err != nil {
		log.Printf("圖片無法辨識：%v", err)
		return f
	}
	var wg sync.WaitGroup
	if work {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if pre.workPNG == nil {
				return
			}
			w, err := p.Work.RecognizeWork(ctx, pre.workPNG)
			if err != nil || w == "" {
				log.Printf("作品辨識失敗：%v", err)
				return
			}
			f.Work = w
		}()
	}
	if text {
		t, err := recognize.RecognizeWithFallback(ctx, p.Text, pre.subtitle, pre.whole)
		if err != nil || t == "" {
			log.Printf("圖片文字辨識失敗：%v", err)
		} else {
			f.Text = t
		}
	}
	wg.Wait()
	return f
}

// RecognizeAttachment 下載附件 a 並只辨識 fields 中的作品、圖片文字（「重新辨識」使用）。
// 辨識以優先排隊（recognize.WithPriority，Q62）；仍失敗的欄位為 imagemsg.FailedMarker，沒要求的欄位留空。
// 下載失敗（含超過 25MB、長度不符）時回錯誤、不辨識。不經過原訊息佇列。
// 同時只進行一個：排到才下載（避免多個附件同時佔記憶體）；排隊中 ctx 結束時回 ctx 的錯誤。
func (p *Pipeline) RecognizeAttachment(ctx context.Context, a *discordgo.MessageAttachment, fields []imagemsg.Field) (imagemsg.Fields, error) {
	p.rerecOnce.Do(func() { p.rerecSlot = make(chan struct{}, 1) })
	select {
	case p.rerecSlot <- struct{}{}:
		defer func() { <-p.rerecSlot }()
	case <-ctx.Done():
		return imagemsg.Fields{}, ctx.Err()
	}
	data, err := p.download(ctx, a)
	if err != nil {
		return imagemsg.Fields{}, err
	}
	return p.recognize(recognize.WithPriority(ctx), data, slices.Contains(fields, imagemsg.FieldWork), slices.Contains(fields, imagemsg.FieldText)), nil
}
