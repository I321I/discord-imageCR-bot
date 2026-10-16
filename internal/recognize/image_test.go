package recognize

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"runtime"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestIsImageAttachment(t *testing.T) {
	cases := []struct {
		name string
		a    *discordgo.MessageAttachment
		want bool
	}{
		{"png", &discordgo.MessageAttachment{ContentType: "image/png", Filename: "a.png"}, true},
		{"jpeg", &discordgo.MessageAttachment{ContentType: "image/jpeg", Filename: "a.jpg"}, true},
		{"webp", &discordgo.MessageAttachment{ContentType: "image/webp", Filename: "a.webp"}, true},
		{"gif", &discordgo.MessageAttachment{ContentType: "image/gif", Filename: "a.gif"}, true},
		{"svg", &discordgo.MessageAttachment{ContentType: "image/svg+xml", Filename: "a.svg"}, false},
		{"video", &discordgo.MessageAttachment{ContentType: "video/mp4", Filename: "a.mp4"}, false},
		{"content type 優先於副檔名", &discordgo.MessageAttachment{ContentType: "text/plain", Filename: "a.png"}, false},
		{"無 content type 看副檔名", &discordgo.MessageAttachment{Filename: "A.JPEG"}, true},
		{"無 content type 副檔名 jpg", &discordgo.MessageAttachment{Filename: "a.jpg"}, true},
		{"無 content type 非圖片副檔名", &discordgo.MessageAttachment{Filename: "a.txt"}, false},
		{"無 content type 無副檔名", &discordgo.MessageAttachment{Filename: "png"}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := IsImageAttachment(c.a); got != c.want {
			t.Errorf("%s：IsImageAttachment = %v，預期 %v", c.name, got, c.want)
		}
	}
}

func TestEligible(t *testing.T) {
	img := &discordgo.MessageAttachment{ContentType: "image/png", Filename: "a.png"}
	img2 := &discordgo.MessageAttachment{ContentType: "image/gif", Filename: "b.gif"}
	video := &discordgo.MessageAttachment{ContentType: "video/mp4", Filename: "a.mp4"}
	svg := &discordgo.MessageAttachment{ContentType: "image/svg+xml", Filename: "a.svg"}
	cases := []struct {
		name string
		m    *discordgo.Message
		want bool
	}{
		{"純圖", &discordgo.Message{Attachments: []*discordgo.MessageAttachment{img, img2}}, true},
		{"圖加影片", &discordgo.Message{Attachments: []*discordgo.MessageAttachment{img, video}}, false},
		{"無附件", &discordgo.Message{Content: "hello"}, false},
		{"只有 embed", &discordgo.Message{Embeds: []*discordgo.MessageEmbed{{URL: "https://example.com/a.png"}}}, false},
		{"svg", &discordgo.Message{Attachments: []*discordgo.MessageAttachment{svg}}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := Eligible(c.m); got != c.want {
			t.Errorf("%s：Eligible = %v，預期 %v", c.name, got, c.want)
		}
	}
}

func solid(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestDecodeFirstFrame_AllFormats(t *testing.T) {
	src := solid(8, 6, color.RGBA{R: 255, A: 255})
	var pngBuf, jpgBuf, gifBuf bytes.Buffer
	if err := png.Encode(&pngBuf, src); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&jpgBuf, src, nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifBuf, src, nil); err != nil {
		t.Fatal(err)
	}
	webpData, err := os.ReadFile("testdata/sample.webp")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"png": pngBuf.Bytes(), "jpeg": jpgBuf.Bytes(), "gif": gifBuf.Bytes()} {
		img, err := DecodeFirstFrame(data)
		if err != nil {
			t.Errorf("%s：解碼失敗：%v", name, err)
			continue
		}
		if b := img.Bounds(); b.Dx() != 8 || b.Dy() != 6 {
			t.Errorf("%s：尺寸 %v，預期 8x6", name, b)
		}
	}
	img, err := DecodeFirstFrame(webpData)
	if err != nil {
		t.Fatalf("webp：解碼失敗：%v", err)
	}
	if b := img.Bounds(); b.Dx() == 0 || b.Dy() == 0 {
		t.Errorf("webp：尺寸為 0：%v", b)
	}
}

func TestDecodeFirstFrame_GIFUsesFirstFrame(t *testing.T) {
	frame := func(idx uint8) *image.Paletted {
		p := image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}})
		for i := range p.Pix {
			p.Pix[i] = idx
		}
		return p
	}
	const red, blue = 0, 1
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, &gif.GIF{Image: []*image.Paletted{frame(red), frame(blue)}, Delay: []int{10, 10}}); err != nil {
		t.Fatal(err)
	}
	img, err := DecodeFirstFrame(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := img.At(1, 1).RGBA()
	if r>>8 < 200 || g>>8 > 50 || b>>8 > 50 {
		t.Errorf("應取第一格（紅色），得到 rgb(%d,%d,%d)", r>>8, g>>8, b>>8)
	}
}

func TestDecodeFirstFrame_Garbage(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("not an image"), {0x89, 'P', 'N', 'G'}} {
		if _, err := DecodeFirstFrame(data); err == nil {
			t.Errorf("%q 應回錯誤", data)
		}
	}
}

func TestEncodePNG_RoundTrip(t *testing.T) {
	src := solid(3, 2, color.RGBA{G: 255, A: 255})
	data, err := EncodePNG(src)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("輸出應為 PNG：%v", err)
	}
	if b := img.Bounds(); b.Dx() != 3 || b.Dy() != 2 {
		t.Errorf("尺寸 %v，預期 3x2", b)
	}
}

// pngWithDeclaredSize 回傳 1x1 PNG，但 IHDR 宣告的尺寸改為 w x h（並修正 CRC）。
func pngWithDeclaredSize(t *testing.T, w, h uint32) []byte {
	t.Helper()
	return pngOfWithDeclaredSize(t, solid(1, 1, color.RGBA{A: 255}), w, h)
}

// pngOfWithDeclaredSize 把 src（1x1）編碼成 PNG，再把 IHDR 宣告的尺寸改為 w x h（並修正 CRC）。
func pngOfWithDeclaredSize(t *testing.T, src image.Image, w, h uint32) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	// 8 bytes 簽章 + 4 bytes 長度 + "IHDR" 之後是寬、高；IHDR 資料 13 bytes，接著 CRC。
	binary.BigEndian.PutUint32(data[16:20], w)
	binary.BigEndian.PutUint32(data[20:24], h)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	return data
}

func TestDecodeFirstFrame_TooLargeRejectedBeforeDecode(t *testing.T) {
	for _, size := range [][2]uint32{{12000, 12000}, {10001, 1}, {1, 10001}, {8000, 6000}, {5000, 4000}} {
		data := pngWithDeclaredSize(t, size[0], size[1])
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := DecodeFirstFrame(data)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ErrImageTooLarge) {
			t.Errorf("%dx%d：應回 ErrImageTooLarge，得到 %v", size[0], size[1], err)
		}
		if d := after.TotalAlloc - before.TotalAlloc; d > 8<<20 {
			t.Errorf("%dx%d：不應解碼像素，卻分配了 %d bytes", size[0], size[1], d)
		}
	}
}

func TestCheckImageSize(t *testing.T) {
	for _, c := range []struct {
		w, h int
		ok   bool
	}{
		{10000, 1600, true}, // 剛好 16M 像素、長邊剛好 10000
		{1600, 10000, true},
		{4000, 4000, true}, // 剛好 16M 像素
		{1920, 1080, true},
		{10001, 1, false},
		{1, 10001, false},
		{4001, 4000, false}, // 16,004,000 像素
		{6000, 4000, false}, // 24M 像素
	} {
		err := checkImageSize(c.w, c.h)
		if c.ok && err != nil {
			t.Errorf("%dx%d 應允許，得到 %v", c.w, c.h, err)
		}
		if !c.ok && !errors.Is(err, ErrImageTooLarge) {
			t.Errorf("%dx%d 應回 ErrImageTooLarge，得到 %v", c.w, c.h, err)
		}
	}
}

// highDepthImages 回傳 1x1 的 16-bit 圖片：PNG 解碼後分別為 *image.Gray16、*image.RGBA64、*image.NRGBA64。
func highDepthImages() map[string]image.Image {
	opaque := image.NewRGBA64(image.Rect(0, 0, 1, 1))
	opaque.Set(0, 0, color.RGBA64{R: 0xffff, A: 0xffff})
	translucent := image.NewNRGBA64(image.Rect(0, 0, 1, 1))
	translucent.Set(0, 0, color.NRGBA64{R: 0xffff, A: 0x8000})
	return map[string]image.Image{
		"Gray16":  image.NewGray16(image.Rect(0, 0, 1, 1)),
		"RGBA64":  opaque,
		"NRGBA64": translucent,
	}
}

// 高色深（16-bit）圖片像素上限減半為 8M：解碼前就以 DecodeConfig 的 ColorModel 拒絕。
func TestDecodeFirstFrame_HighBitDepthLimitBeforeDecode(t *testing.T) {
	for name, src := range highDepthImages() {
		data := pngOfWithDeclaredSize(t, src, 3000, 3000) // 9M 像素：一般圖片允許，16-bit 不允許
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := DecodeFirstFrame(data)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ErrImageTooLarge) {
			t.Errorf("%s 3000x3000：應回 ErrImageTooLarge，得到 %v", name, err)
		}
		if d := after.TotalAlloc - before.TotalAlloc; d > 8<<20 {
			t.Errorf("%s：不應解碼像素，卻分配了 %d bytes", name, d)
		}
	}
}

func TestDecodeFirstFrame_HighBitDepthSmallImageDecodes(t *testing.T) {
	for name, src := range highDepthImages() {
		var buf bytes.Buffer
		if err := png.Encode(&buf, src); err != nil {
			t.Fatal(err)
		}
		img, err := DecodeFirstFrame(buf.Bytes())
		if err != nil {
			t.Errorf("%s：小圖應可解碼，得到 %v", name, err)
			continue
		}
		if !isHighDepth(img.ColorModel()) {
			t.Errorf("%s：解碼結果 %T 應為高色深", name, img)
		}
	}
}

func TestCheckImageConfig(t *testing.T) {
	for _, c := range []struct {
		name  string
		model color.Model
		w, h  int
		ok    bool
	}{
		{"16-bit 剛好 8M", color.Gray16Model, 4000, 2000, true},
		{"16-bit 超過 8M", color.Gray16Model, 4000, 2001, false},
		{"RGBA64 超過 8M", color.RGBA64Model, 3000, 3000, false},
		{"NRGBA64 超過 8M", color.NRGBA64Model, 3000, 3000, false},
		{"16-bit 長邊超過 10000", color.NRGBA64Model, 10001, 1, false},
		{"8-bit 剛好 16M", color.NRGBAModel, 4000, 4000, true},
		{"8-bit 超過 16M", color.RGBAModel, 4000, 4001, false},
		{"YCbCr（JPEG）9M", color.YCbCrModel, 3000, 3000, true},
		{"Gray 9M", color.GrayModel, 3000, 3000, true},
	} {
		err := checkImageConfig(image.Config{ColorModel: c.model, Width: c.w, Height: c.h})
		if c.ok && err != nil {
			t.Errorf("%s：應允許，得到 %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrImageTooLarge) {
			t.Errorf("%s：應回 ErrImageTooLarge，得到 %v", c.name, err)
		}
	}
}
