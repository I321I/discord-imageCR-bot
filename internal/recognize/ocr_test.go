package recognize

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"slices"
	"sync"
	"testing"
)

// topWhiteBottomBlack 回傳上方 (h-bottom) 列為白色、下方 bottom 列為黑色的圖。
func topWhiteBottomBlack(w, h, bottom int) *image.RGBA {
	img := solid(w, h, color.White)
	for y := h - bottom; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.Black)
		}
	}
	return img
}

func TestPrepareForOCR_CropsBottom35AndShrinksTo640(t *testing.T) {
	// 1920x1080 → 裁下方 35%（1920x378）→ 等比縮到寬 640（640x126）。
	out := PrepareForOCR(topWhiteBottomBlack(1920, 1080, 378))
	if b := out.Bounds(); b.Dx() != 640 || b.Dy() < 125 || b.Dy() > 127 {
		t.Fatalf("1920x1080 應輸出 640x126（±1），得到 %v", b)
	}
	if _, ok := out.(*image.RGBA); !ok {
		t.Errorf("輸出應為 *image.RGBA（彩色、不灰階），得到 %T", out)
	}
	// 只取下方 35%（全部黑色）。
	b := out.Bounds()
	for _, p := range []image.Point{{b.Min.X, b.Min.Y}, {b.Max.X - 1, b.Max.Y - 1}, {b.Min.X + b.Dx()/2, b.Min.Y + b.Dy()/2}} {
		if g := color.GrayModel.Convert(out.At(p.X, p.Y)).(color.Gray).Y; g > 30 {
			t.Errorf("(%d,%d) 應為黑色（下方區域），得到灰階 %d", p.X, p.Y, g)
		}
	}
}

func TestPrepareForOCR_KeepsColor(t *testing.T) {
	out := PrepareForOCR(solid(100, 100, color.RGBA{R: 255, A: 255}))
	if r, g, b, _ := out.At(50, 20).RGBA(); r>>8 < 250 || g>>8 > 5 || b>>8 > 5 {
		t.Errorf("應保留顏色（紅色），得到 %v %v %v", r>>8, g>>8, b>>8)
	}
}

func TestPrepareForOCR_NoUpscale(t *testing.T) {
	if b := PrepareForOCR(solid(400, 300, color.White)).Bounds(); b.Dx() != 400 || b.Dy() != 105 {
		t.Errorf("400x300 應輸出 400x105（不放大），得到 %v", b)
	}
}

func TestPrepareForOCR_NonZeroOrigin(t *testing.T) {
	src := topWhiteBottomBlack(100, 200, 70).SubImage(image.Rect(10, 0, 60, 200))
	out := PrepareForOCR(src)
	if b := out.Bounds(); b.Dx() != 50 || b.Dy() != 70 {
		t.Errorf("50x200 應輸出 50x70，得到 %v", b)
	}
	if g := color.GrayModel.Convert(out.At(out.Bounds().Min.X, out.Bounds().Min.Y)).(color.Gray).Y; g > 30 {
		t.Errorf("應只取下方 35%%（黑色），得到灰階 %d", g)
	}
}

func TestPrepareWholeForOCR_Size(t *testing.T) {
	for _, c := range []struct{ w, h, wantW, wantH int }{
		{1920, 1080, 1280, 720}, // 長邊縮到 1280
		{1080, 1920, 720, 1280},
		{3000, 10, 1280, 4},
		{1280, 720, 1280, 720}, // 剛好，不縮
		{400, 300, 400, 300},   // 不放大
	} {
		out := PrepareWholeForOCR(solid(c.w, c.h, color.White))
		if b := out.Bounds(); b.Dx() != c.wantW || b.Dy() != c.wantH {
			t.Errorf("%dx%d 應輸出 %dx%d，得到 %v", c.w, c.h, c.wantW, c.wantH, b)
		}
		if _, ok := out.(*image.RGBA); !ok {
			t.Errorf("輸出應為 *image.RGBA，得到 %T", out)
		}
	}
}

func TestPrepareWholeForOCR_NonZeroOrigin(t *testing.T) {
	src := topWhiteBottomBlack(100, 200, 70).SubImage(image.Rect(10, 130, 60, 200))
	out := PrepareWholeForOCR(src)
	if b := out.Bounds(); b.Dx() != 50 || b.Dy() != 70 {
		t.Fatalf("50x70 應輸出 50x70，得到 %v", b)
	}
	if g := color.GrayModel.Convert(out.At(out.Bounds().Min.X, out.Bounds().Min.Y)).(color.Gray).Y; g > 30 {
		t.Errorf("應取子圖的內容（黑色），得到灰階 %d", g)
	}
}

// 極小或零尺寸的圖不應 panic。
func TestPrepareForOCR_TinyDoesNotPanic(t *testing.T) {
	for _, r := range []image.Rectangle{image.Rect(0, 0, 1, 1), image.Rect(0, 0, 1, 2), image.Rect(0, 0, 5, 0), image.Rect(0, 0, 0, 5)} {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("%v：不應 panic：%v", r, p)
				}
			}()
			if b := PrepareForOCR(image.NewRGBA(r)).Bounds(); b.Dx() > r.Dx() || b.Dy() > r.Dy() {
				t.Errorf("%v：輸出 %v 不應大於原圖", r, b)
			}
			if b := PrepareWholeForOCR(image.NewRGBA(r)).Bounds(); b.Dx() != r.Dx() || b.Dy() != r.Dy() {
				t.Errorf("%v：整張輸出 %v 應與原圖同尺寸", r, b)
			}
		}()
	}
}

func TestCleanOCRText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"這 就 是 人 類 啊", "這就是人類啊"},
		{"Hello world", "Hello world"},
		{"這是 ABC 測試", "這是 ABC 測試"},
		{"「你 好 」 。", "「你好」。"},
		{"これ は テスト", "これはテスト"},
		{"第一行\n\n   \n第 二 行\f\n", "第一行\n第二行"},
		{"  前後空白  ", "前後空白"},
		{"中\t文", "中文"},
		{"", ""},
		{" \n\f\n", ""},
	}
	for _, c := range cases {
		if got := CleanOCRText(c.in); got != c.want {
			t.Errorf("CleanOCRText(%q) = %q，預期 %q", c.in, got, c.want)
		}
	}
}

func pngSize(t *testing.T, data []byte) image.Point {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("stdin 應為 PNG：%v", err)
	}
	return img.Bounds().Size()
}

// rectItem 回傳左上角 (x, y)、寬 w、高 h 的文字框（RapidOCR 的頂點順序：左上、右上、右下、左下）。
func rectItem(text string, x, y, w, h float64) OCRItem {
	return OCRItem{Box: [4][2]float64{{x, y}, {x + w, y}, {x + w, y + h}, {x, y + h}}, Text: text, Score: 0.9}
}

func TestJoinOCRItems(t *testing.T) {
	cases := []struct {
		name  string
		items []OCRItem
		want  string
	}{
		{"上下兩行（給的順序顛倒）", []OCRItem{
			rectItem("其他都不重要啦", 300, 80, 200, 30),
			rectItem("我只要能跟女孩子親親我我", 250, 30, 300, 30),
		}, "我只要能跟女孩子親親我我\n其他都不重要啦"},
		{"同一行左右兩個（x 順序顛倒）", []OCRItem{
			rectItem("來一發再睡覺", 300, 52, 200, 30),
			rectItem("回家吃飯", 100, 50, 150, 30),
		}, "回家吃飯來一發再睡覺"},
		{"英文同行以空白連接", []OCRItem{
			rectItem("WORLD", 200, 10, 100, 20),
			rectItem("HELLO", 50, 12, 100, 20),
		}, "HELLO WORLD"},
		{"空", nil, ""},
		{"垂直重疊不足一半視為不同行", []OCRItem{
			rectItem("下", 0, 18, 40, 20), // 與上一行重疊 2px（< 較矮者 50%）
			rectItem("上", 100, 0, 40, 20),
		}, "上\n下"},
		{"垂直重疊剛好一半視為同一行", []OCRItem{
			rectItem("右", 100, 10, 40, 20),
			rectItem("左", 0, 0, 40, 20),
		}, "左右"},
		{"空白文字略過", []OCRItem{
			rectItem("  ", 0, 0, 40, 20),
			rectItem("字", 0, 50, 40, 20),
		}, "字"},
		{"中日文字間空白移除（CleanOCRText）", []OCRItem{
			rectItem("這 就 是", 0, 0, 100, 20),
		}, "這就是"},
		{"直書：由右至左，每欄一行", []OCRItem{
			rectItem("中間", 200, 0, 30, 100),
			rectItem("最左", 100, 10, 30, 100),
			rectItem("最右", 300, 5, 30, 100),
		}, "最右\n中間\n最左"},
		{"直書：同一欄上下兩個框由上到下連接", []OCRItem{
			rectItem("下半", 102, 130, 30, 100),
			rectItem("左欄", 0, 0, 30, 100),
			rectItem("上半", 100, 0, 30, 100),
		}, "上半下半\n左欄"},
		{"高寬比剛好 1.5 視為直書", []OCRItem{
			rectItem("左", 0, 0, 20, 30),
			rectItem("右", 100, 0, 20, 30),
		}, "右\n左"},
		{"高寬比小於 1.5 視為橫書", []OCRItem{
			rectItem("右", 100, 0, 20, 29),
			rectItem("左", 0, 0, 20, 29),
		}, "左右"},
		{"橫書與直書混合：橫書行在前、直書欄在後", []OCRItem{
			rectItem("直左", 100, 100, 30, 100),
			rectItem("橫書", 0, 300, 100, 20),
			rectItem("直右", 200, 100, 30, 100),
		}, "橫書\n直右\n直左"},
		{"直書：同一對話框相鄰兩欄接成一行（不加空白）", []OCRItem{
			rectItem("左欄", 100, 10, 30, 80),
			rectItem("右欄", 140, 0, 30, 100),
		}, "右欄左欄"},
		{"直書：同一對話框三欄依序接成一行", []OCRItem{
			rectItem("三", 120, 0, 30, 60),
			rectItem("一", 200, 0, 30, 100),
			rectItem("二", 160, 20, 30, 100),
		}, "一二三"},
		{"直書：欄間距剛好等於欄寬仍是同一對話框", []OCRItem{
			rectItem("左", 100, 0, 30, 100),
			rectItem("右", 160, 0, 30, 100), // 間距 30 = 欄寬
		}, "右左"},
		{"直書：欄間距大於欄寬是不同對話框", []OCRItem{
			rectItem("左", 100, 0, 30, 100),
			rectItem("右", 161, 0, 30, 100), // 間距 31 > 欄寬
		}, "右\n左"},
		{"直書：水平相鄰但垂直範圍不重疊是不同對話框", []OCRItem{
			rectItem("左下", 100, 150, 30, 100),
			rectItem("右上", 140, 0, 30, 100),
		}, "右上\n左下"},
		{"直書：兩個對話框各兩欄，對話框由右至左", []OCRItem{
			rectItem("乙二", 160, 0, 30, 100),
			rectItem("甲一", 400, 0, 30, 100),
			rectItem("乙一", 200, 0, 30, 100),
			rectItem("甲二", 360, 0, 30, 100),
		}, "甲一甲二\n乙一乙二"},
		{"直書：另一對話框的欄夾在中間也不會拆散", []OCRItem{
			rectItem("甲二", 250, 0, 30, 100),
			rectItem("乙", 275, 200, 30, 100), // 水平位置在甲一、甲二之間，但垂直不重疊
			rectItem("甲一", 300, 0, 30, 100),
		}, "甲一甲二\n乙"},
		{"直書：同一欄上下間距剛好等於框寬仍是同一對話框", []OCRItem{
			rectItem("下", 100, 130, 30, 100), // 間距 30 = 框寬
			rectItem("上", 100, 0, 30, 100),
		}, "上下"},
		{"直書：同一欄上下間距大於框寬是不同對話框", []OCRItem{
			rectItem("下", 100, 131, 30, 100), // 間距 31 > 框寬
			rectItem("上", 100, 0, 30, 100),
		}, "上\n下"},
		{"直書：上下間距以較寬的框為準", []OCRItem{
			rectItem("下", 100, 145, 20, 100), // 間距 45：> 窄框 20、≤ 寬框 50
			rectItem("上", 90, 0, 50, 100),
		}, "上下"},
		{"直書：同一欄拆成上下兩段後各自與相鄰欄組成對話框", []OCRItem{
			rectItem("右上", 200, 0, 30, 100),
			rectItem("右下", 200, 200, 30, 100),
			rectItem("左上", 160, 0, 30, 100),
			rectItem("左下", 160, 200, 30, 100),
		}, "右上左上\n右下左下"},
		{"零寬高的框不算直書", []OCRItem{
			{Box: [4][2]float64{{50, 10}, {50, 10}, {50, 10}, {50, 10}}, Text: "點"},
			rectItem("左", 0, 0, 40, 20),
		}, "左點"},
		{"頂點順序不同也能取得範圍", []OCRItem{
			{Box: [4][2]float64{{200, 52}, {100, 52}, {100, 22}, {200, 22}}, Text: "下"},
			{Box: [4][2]float64{{100, 20}, {0, 20}, {0, 0}, {100, 0}}, Text: "上"},
		}, "上\n下"},
	}
	for _, c := range cases {
		if got := JoinOCRItems(c.items); got != c.want {
			t.Errorf("%s：得到 %q，預期 %q", c.name, got, c.want)
		}
	}
}

// scriptedText 依序回傳 results，並記錄每次收到的圖片尺寸。
type scriptedText struct {
	results []textResult
	sizes   []image.Point
}

type textResult struct {
	text string
	err  error
}

func (s *scriptedText) RecognizeText(ctx context.Context, img image.Image) (string, error) {
	s.sizes = append(s.sizes, img.Bounds().Size())
	r := s.results[0]
	s.results = s.results[1:]
	return r.text, r.err
}

func TestRecognizeWithFallback_SubtitleFound(t *testing.T) {
	s := &scriptedText{results: []textResult{{text: "字幕"}}}
	got, err := RecognizeWithFallback(context.Background(), s, solid(64, 12, color.White), solid(128, 72, color.White))
	if err != nil || got != "字幕" {
		t.Fatalf("得到 %q %v", got, err)
	}
	if len(s.sizes) != 1 || s.sizes[0] != (image.Point{64, 12}) {
		t.Errorf("字幕區有字時只應辨識字幕區，得到 %v", s.sizes)
	}
}

func TestRecognizeWithFallback_NoSubtitleRetriesWhole(t *testing.T) {
	// Q57 (b)：字幕區沒讀到字 → 自動對整張再辨識一次。
	s := &scriptedText{results: []textResult{{err: ErrNoText}, {text: "整張"}}}
	got, err := RecognizeWithFallback(context.Background(), s, solid(64, 12, color.White), solid(128, 72, color.White))
	if err != nil || got != "整張" {
		t.Fatalf("得到 %q %v", got, err)
	}
	if len(s.sizes) != 2 || s.sizes[1] != (image.Point{128, 72}) {
		t.Errorf("第二次應辨識整張，得到 %v", s.sizes)
	}
}

func TestRecognizeWithFallback_BothEmptyIsErrNoText(t *testing.T) {
	s := &scriptedText{results: []textResult{{err: ErrNoText}, {err: ErrNoText}}}
	_, err := RecognizeWithFallback(context.Background(), s, solid(64, 12, color.White), solid(128, 72, color.White))
	if !errors.Is(err, ErrNoText) {
		t.Errorf("兩次都沒字應回 ErrNoText，得到 %v", err)
	}
}

func TestRecognizeWithFallback_OtherErrorDoesNotRetry(t *testing.T) {
	boom := errors.New("boom")
	s := &scriptedText{results: []textResult{{err: boom}}}
	_, err := RecognizeWithFallback(context.Background(), s, solid(64, 12, color.White), solid(128, 72, color.White))
	if !errors.Is(err, boom) {
		t.Errorf("應回原錯誤，得到 %v", err)
	}
	if len(s.sizes) != 1 {
		t.Errorf("非 ErrNoText 的錯誤不應重試，得到 %d 次", len(s.sizes))
	}
}

func TestRecognizeWithFallback_CanceledBeforeRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &scriptedText{results: []textResult{{err: ErrNoText}, {text: "不應執行"}}}
	cancel()
	_, err := RecognizeWithFallback(ctx, s, solid(64, 12, color.White), solid(128, 72, color.White))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 已取消時不應重試，應回 ctx 錯誤，得到 %v", err)
	}
}

func TestRecognizeWithFallback_TallImageUsesWholeOnly(t *testing.T) {
	// 圖片高 > 寬（whole 與原圖同比例）：直接辨識整張，不先讀字幕區。
	s := &scriptedText{results: []textResult{{text: "整張"}}}
	got, err := RecognizeWithFallback(context.Background(), s, solid(64, 12, color.White), solid(72, 128, color.White))
	if err != nil || got != "整張" {
		t.Fatalf("得到 %q %v", got, err)
	}
	if len(s.sizes) != 1 || s.sizes[0] != (image.Point{72, 128}) {
		t.Errorf("直的圖應只辨識整張，得到 %v", s.sizes)
	}
}

func TestRecognizeWithFallback_SquareImageTriesSubtitleFirst(t *testing.T) {
	s := &scriptedText{results: []textResult{{text: "字幕"}}}
	if got, err := RecognizeWithFallback(context.Background(), s, solid(64, 12, color.White), solid(100, 100, color.White)); err != nil || got != "字幕" {
		t.Fatalf("得到 %q %v", got, err)
	}
	if len(s.sizes) != 1 || s.sizes[0] != (image.Point{64, 12}) {
		t.Errorf("正方形圖應先辨識字幕區，得到 %v", s.sizes)
	}
}

// scriptedItems 是能回傳文字框的辨識器（如 RapidOCR）：依序回傳 results，記錄每次收到的圖片尺寸。
type scriptedItems struct {
	results [][]OCRItem
	sizes   []image.Point
}

func (s *scriptedItems) RecognizeItems(ctx context.Context, img image.Image) ([]OCRItem, error) {
	s.sizes = append(s.sizes, img.Bounds().Size())
	r := s.results[0]
	s.results = s.results[1:]
	return r, nil
}

func (s *scriptedItems) RecognizeText(ctx context.Context, img image.Image) (string, error) {
	items, err := s.RecognizeItems(ctx, img)
	if err != nil {
		return "", err
	}
	if text := JoinOCRItems(items); text != "" {
		return text, nil
	}
	return "", ErrNoText
}

func TestRecognizeWithFallback_HorizontalSubtitleKept(t *testing.T) {
	// 10/28 Task 4：框改為 y=10（原本 y=0 會碰到上緣而改辨識整張）。
	s := &scriptedItems{results: [][]OCRItem{{rectItem("字幕", 0, 10, 60, 20)}}}
	got, err := RecognizeWithFallback(context.Background(), s, solid(64, 12, color.White), solid(128, 72, color.White))
	if err != nil || got != "字幕" {
		t.Fatalf("得到 %q %v", got, err)
	}
	if len(s.sizes) != 1 {
		t.Errorf("字幕區只有橫書框時不應辨識整張，得到 %v", s.sizes)
	}
}

func TestRecognizeWithFallback_VerticalBoxInSubtitleUsesWhole(t *testing.T) {
	s := &scriptedItems{results: [][]OCRItem{
		{rectItem("字幕", 0, 0, 60, 20), rectItem("直", 100, 0, 20, 30)}, // 高/寬 = 1.5
		{rectItem("整張", 0, 0, 60, 20)},
	}}
	got, err := RecognizeWithFallback(context.Background(), s, solid(64, 12, color.White), solid(128, 72, color.White))
	if err != nil || got != "整張" {
		t.Fatalf("字幕區有直書框應改用整張的結果，得到 %q %v", got, err)
	}
	if len(s.sizes) != 2 || s.sizes[1] != (image.Point{128, 72}) {
		t.Errorf("第二次應辨識整張，得到 %v", s.sizes)
	}
}

func TestRecognizeWithFallback_ZeroSizeBoxIsNotVertical(t *testing.T) {
	point := OCRItem{Box: [4][2]float64{{3, 3}, {3, 3}, {3, 3}, {3, 3}}, Text: "點"}
	// 10/28 Task 4：字幕框改為 y=10（原本 y=0 會碰到上緣）；點在 y=3，未碰到上緣。
	s := &scriptedItems{results: [][]OCRItem{{rectItem("字幕", 0, 10, 60, 20), point}}}
	if _, err := RecognizeWithFallback(context.Background(), s, solid(64, 12, color.White), solid(128, 72, color.White)); err != nil {
		t.Fatal(err)
	}
	if len(s.sizes) != 1 {
		t.Errorf("零寬高的框不算直書，不應辨識整張，得到 %v", s.sizes)
	}
}

// 10/28 Task 4：字幕區有文字框碰到上緣（y.lo ≤ subtitleTopEdgePx）→ 字幕區切到畫面中的文字（例如直書漫畫的半欄），改辨識整張。
func TestRecognizeWithFallback_BoxTouchingTopUsesWhole(t *testing.T) {
	s := &scriptedItems{results: [][]OCRItem{
		{rectItem("是", 100, 0, 30, 30)}, // 正方形框（不是直書）碰到上緣
		{rectItem("整張", 0, 10, 60, 20)},
	}}
	got, err := RecognizeWithFallback(context.Background(), s, solid(640, 152, color.White), solid(1280, 874, color.White))
	if err != nil || got != "整張" {
		t.Fatalf("碰到上緣應改用整張的結果，得到 %q %v", got, err)
	}
	if len(s.sizes) != 2 || s.sizes[1] != (image.Point{1280, 874}) {
		t.Errorf("第二次應辨識整張，得到 %v", s.sizes)
	}
}

func TestRecognizeWithFallback_BoxNearTopWithinThreshold(t *testing.T) {
	if subtitleTopEdgePx != 2 {
		t.Errorf("門檻 %v，預期 2（Q6）", subtitleTopEdgePx)
	}
	s := &scriptedItems{results: [][]OCRItem{
		{rectItem("字幕", 0, 40, 60, 20), rectItem("是", 100, 2, 30, 30)},
		{rectItem("整張", 0, 10, 60, 20)},
	}}
	if got, err := RecognizeWithFallback(context.Background(), s, solid(640, 152, color.White), solid(1280, 874, color.White)); err != nil || got != "整張" {
		t.Fatalf("y.lo = 2 應改用整張，得到 %q %v", got, err)
	}
}

func TestRecognizeWithFallback_BoxBelowTopKept(t *testing.T) {
	s := &scriptedItems{results: [][]OCRItem{{rectItem("字幕", 0, 3, 60, 20)}}}
	got, err := RecognizeWithFallback(context.Background(), s, solid(640, 152, color.White), solid(1280, 874, color.White))
	if err != nil || got != "字幕" {
		t.Fatalf("y.lo = 3 應保留字幕區結果，得到 %q %v", got, err)
	}
	if len(s.sizes) != 1 {
		t.Errorf("不應辨識整張，得到 %v", s.sizes)
	}
}

// 頂端的框是空白文字時不算（同 toBoxes 略過空白）。
func TestRecognizeWithFallback_BlankBoxAtTopIgnored(t *testing.T) {
	s := &scriptedItems{results: [][]OCRItem{{rectItem(" ", 0, 0, 60, 20), rectItem("字幕", 0, 40, 60, 20)}}}
	got, err := RecognizeWithFallback(context.Background(), s, solid(640, 152, color.White), solid(1280, 874, color.White))
	if err != nil || got != "字幕" {
		t.Fatalf("空白框不算碰到上緣，得到 %q %v", got, err)
	}
	if len(s.sizes) != 1 {
		t.Errorf("不應辨識整張，得到 %v", s.sizes)
	}
}

func TestSubtitleTouchesTop(t *testing.T) {
	cases := []struct {
		name  string
		items []OCRItem
		want  bool
	}{
		{"沒有框", nil, false},
		{"y.lo = 0", []OCRItem{rectItem("a", 0, 0, 10, 10)}, true},
		{"y.lo = 2", []OCRItem{rectItem("a", 0, 2, 10, 10)}, true},
		{"y.lo = 2.5", []OCRItem{rectItem("a", 0, 2.5, 10, 10)}, false},
		{"y.lo = 3", []OCRItem{rectItem("a", 0, 3, 10, 10)}, false},
		{"負座標", []OCRItem{rectItem("a", 0, -1, 10, 10)}, true},
		{"空白文字", []OCRItem{rectItem("  ", 0, 0, 10, 10)}, false},
		{"任一框碰到", []OCRItem{rectItem("a", 0, 30, 10, 10), rectItem("b", 20, 1, 10, 10)}, true},
	}
	for _, c := range cases {
		if got := subtitleTouchesTop(c.items); got != c.want {
			t.Errorf("%s：subtitleTouchesTop = %v，預期 %v", c.name, got, c.want)
		}
	}
}

func TestRecognizeWithFallback_NoItemsInSubtitleUsesWhole(t *testing.T) {
	s := &scriptedItems{results: [][]OCRItem{nil, {rectItem("整張", 0, 0, 60, 20)}}}
	if got, err := RecognizeWithFallback(context.Background(), s, solid(64, 12, color.White), solid(128, 72, color.White)); err != nil || got != "整張" {
		t.Fatalf("得到 %q %v", got, err)
	}
}

// gatedText 第一次呼叫時通知 entered 並等 release；記錄每次呼叫收到的圖片寬度。
type gatedText struct {
	mu      sync.Mutex
	widths  []int
	entered chan struct{}
	release chan struct{}
}

func (g *gatedText) RecognizeText(ctx context.Context, img image.Image) (string, error) {
	g.mu.Lock()
	g.widths = append(g.widths, img.Bounds().Dx())
	first := len(g.widths) == 1
	g.mu.Unlock()
	if first {
		close(g.entered)
		<-g.release
		return "", ErrNoText
	}
	return "ok", nil
}

// 整張重試接續處理、不重新排隊：字幕區沒字後，即使有優先請求在排隊，整張也緊接著辨識。
func TestRecognizeWithFallback_SerializedRetryDoesNotRequeue(t *testing.T) {
	g := &gatedText{entered: make(chan struct{}), release: make(chan struct{})}
	q := SerializeText(g)
	done := make(chan struct{}, 2)
	go func() {
		RecognizeWithFallback(context.Background(), q, solid(10, 5, color.White), solid(20, 10, color.White))
		done <- struct{}{}
	}()
	<-g.entered
	go func() {
		q.RecognizeText(WithPriority(context.Background()), solid(30, 10, color.White))
		done <- struct{}{}
	}()
	sem := q.(*serializedText).sem
	waitFor(t, "優先請求排隊", func() bool {
		sem.mu.Lock()
		defer sem.mu.Unlock()
		return len(sem.queues[0]) == 1
	})
	close(g.release)
	<-done
	<-done
	g.mu.Lock()
	defer g.mu.Unlock()
	if want := []int{10, 20, 30}; !slices.Equal(g.widths, want) {
		t.Errorf("辨識順序（寬度）%v，預期 %v（整張緊接字幕區）", g.widths, want)
	}
}
