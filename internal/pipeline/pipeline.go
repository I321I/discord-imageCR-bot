// Package pipeline 處理圖片來源頻道的新訊息：下載附件、辨識、發出圖片訊息、刪除原訊息。
package pipeline

import (
	"context"
	"image"
	"log"
	"sync"

	"discordbot/internal/imagemsg"
	"discordbot/internal/recognize"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// API 是 pipeline 需要的 Discord 方法（*discordgo.Session 直接滿足）。
type API interface {
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
	f := imagemsg.Fields{Work: imagemsg.FailedMarker, Text: imagemsg.FailedMarker}
	pre, err := p.prepare(ctx, data)
	if err != nil {
		log.Printf("圖片無法辨識：%v", err)
		return f
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if pre.workPNG == nil {
			return
		}
		work, err := p.Work.RecognizeWork(ctx, pre.workPNG)
		if err != nil || work == "" {
			log.Printf("作品辨識失敗：%v", err)
			return
		}
		f.Work = work
	}()
	text, err := recognize.RecognizeWithFallback(ctx, p.Text, pre.subtitle, pre.whole)
	if err != nil || text == "" {
		log.Printf("圖片文字辨識失敗：%v", err)
	} else {
		f.Text = text
	}
	wg.Wait()
	return f
}
