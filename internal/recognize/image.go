// Package recognize 辨識圖片的作品（trace.moe）與圖片文字（OCR）。
package recognize

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // 註冊 GIF 解碼；image.Decode 對 GIF 只回傳第一格
	_ "image/jpeg"
	"image/png"
	"path"
	"strings"

	"github.com/bwmarrin/discordgo"
	_ "golang.org/x/image/webp"
)

// imageContentTypes 是要處理的圖片 MIME 類型。
var imageContentTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// imageExtensions 在附件沒有 ContentType 時使用。
var imageExtensions = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".webp": true,
	".gif":  true,
}

// IsImageAttachment 回傳附件是否為要處理的圖片（PNG、JPG、WEBP、GIF）。
// 以 ContentType 判斷；ContentType 為空時才看副檔名。
func IsImageAttachment(a *discordgo.MessageAttachment) bool {
	if a == nil {
		return false
	}
	if a.ContentType != "" {
		mediaType, _, _ := strings.Cut(a.ContentType, ";")
		return imageContentTypes[strings.ToLower(strings.TrimSpace(mediaType))]
	}
	return imageExtensions[strings.ToLower(path.Ext(a.Filename))]
}

// Eligible 回傳訊息是否要自動處理：至少一個附件，且全部是圖片。
// 只有連結/embed 或純文字的訊息不處理；含任何非圖片附件的訊息整則不處理。
func Eligible(m *discordgo.Message) bool {
	if m == nil || len(m.Attachments) == 0 {
		return false
	}
	for _, a := range m.Attachments {
		if !IsImageAttachment(a) {
			return false
		}
	}
	return true
}

// 圖片尺寸上限：超過就不解碼（避免小檔案宣告超大尺寸耗盡記憶體）。
const (
	maxImageSide   = 10000      // 長或寬上限（px）
	maxImagePixels = 16_000_000 // 總像素上限（Q60：RGBA 解碼約 64MB）
)

// ErrImageTooLarge 表示圖片尺寸超過上限，不解碼也不辨識。
var ErrImageTooLarge = errors.New("image too large")

// checkImageSize 檢查 w x h 是否超過尺寸上限。
func checkImageSize(w, h int) error {
	if w > maxImageSide || h > maxImageSide || w*h > maxImagePixels {
		return fmt.Errorf("%w：%dx%d", ErrImageTooLarge, w, h)
	}
	return nil
}

// DecodeFirstFrame 解碼 PNG、JPEG、GIF（第一格）、WEBP。
// 解碼前先讀檔頭檢查尺寸，超過上限回 ErrImageTooLarge。
func DecodeFirstFrame(data []byte) (image.Image, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("解碼圖片失敗：%w", err)
	}
	if err := checkImageSize(cfg.Width, cfg.Height); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("解碼圖片失敗：%w", err)
	}
	return img, nil
}

// EncodePNG 把圖片編碼成 PNG。
func EncodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("編碼 PNG 失敗：%w", err)
	}
	return buf.Bytes(), nil
}
