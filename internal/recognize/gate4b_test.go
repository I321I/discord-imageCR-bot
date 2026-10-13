package recognize

// 把關者（階段 4 追加批次）補充測試：兩把鎖下 Close／閒置／請求的競爭、kill 可重複呼叫、
// isHighDepth 對其他色彩模型的判斷、RecognizeWithFallback 與排隊包裝的互動、直書對話框分組邊界。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/png"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// 多個並行請求進行中時 Close：所有請求都要在短時間內返回（成功或錯誤），Close 之後不再啟動 worker，
// 所有 worker 都已回收。
func TestGate4b_RapidOCR_CloseRacesWithManyRequests(t *testing.T) {
	for round := 0; round < 5; round++ {
		r, logPath := gateRapidOCR(t, "echo")
		r.IdleTimeout = time.Millisecond
		var wg sync.WaitGroup
		for g := 0; g < 6; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					_, err := r.RecognizeText(context.Background(), solid(10, 10, color.White))
					if errors.Is(err, errOCRClosed) {
						return
					}
				}
			}()
		}
		time.Sleep(time.Duration(rand.Intn(80)) * time.Millisecond)
		closeStart := time.Now()
		r.Close()
		if d := time.Since(closeStart); d > workerStopGrace+time.Second {
			t.Errorf("第 %d 輪：Close 花了 %v", round, d)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("第 %d 輪：Close 後仍有請求沒有返回", round)
		}
		pidsAfter := len(startedPIDs(t, logPath))
		for _, pid := range startedPIDs(t, logPath) {
			waitFor(t, fmt.Sprintf("第 %d 輪 worker %d 回收", round, pid), func() bool { return !processAlive(pid) })
		}
		if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); !errors.Is(err, errOCRClosed) {
			t.Errorf("第 %d 輪：Close 後應回 errOCRClosed，得到 %v", round, err)
		}
		if n := len(startedPIDs(t, logPath)); n != pidsAfter {
			t.Errorf("第 %d 輪：Close 後又啟動了 worker（%d → %d）", round, pidsAfter, n)
		}
	}
}

// kill 可重複呼叫（含與 cmd.Wait 關閉 stdout 重疊）：不 panic、不卡住；discard 對已不是目前 worker 的 w 也安全。
func TestGate4b_RapidOCR_KillTwiceAndDiscardStale(t *testing.T) {
	r, _ := gateRapidOCR(t, "echo")
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	w := r.w
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); w.kill() }()
		}
		wg.Wait()
		r.discard(w)
		r.discard(w)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("重複 kill／discard 卡住")
	}
	// 被殺掉後下一張重新啟動並成功。
	if got, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil || got != "ok" {
		t.Errorf("重新啟動後得到 %q %v", got, err)
	}
}

// 閒置計時器正在 stop 舊 worker 時 Close：不 panic、不卡住，所有 worker 回收。
func TestGate4b_RapidOCR_CloseWhileIdleStopping(t *testing.T) {
	for i := 0; i < 10; i++ {
		r, logPath := gateRapidOCR(t, "echo")
		r.IdleTimeout = time.Duration(i) * time.Millisecond / 2
		if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(i) * time.Millisecond / 3)
		r.Close()
		for _, pid := range startedPIDs(t, logPath) {
			waitFor(t, fmt.Sprintf("worker %d 回收", pid), func() bool { return !processAlive(pid) })
		}
	}
}

// isHighDepth：Palette（不可比較的 slice 型別）、CMYK、YCbCr、NYCbCrA、Alpha16 等都不 panic；只有三種 16-bit 色彩模型為 true。
func TestGate4b_IsHighDepth_OtherModels(t *testing.T) {
	for _, c := range []struct {
		name string
		m    color.Model
		want bool
	}{
		{"Palette", color.Palette(palette.Plan9), false},
		{"空 Palette", color.Palette{}, false},
		{"CMYK", color.CMYKModel, false},
		{"YCbCr", color.YCbCrModel, false},
		{"NYCbCrA", color.NYCbCrAModel, false},
		{"Alpha16", color.Alpha16Model, false},
		{"Gray", color.GrayModel, false},
		{"RGBA", color.RGBAModel, false},
		{"NRGBA", color.NRGBAModel, false},
		{"nil", nil, false},
		{"RGBA64", color.RGBA64Model, true},
		{"NRGBA64", color.NRGBA64Model, true},
		{"Gray16", color.Gray16Model, true},
	} {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("%s：panic %v", c.name, p)
				}
			}()
			if got := isHighDepth(c.m); got != c.want {
				t.Errorf("%s：isHighDepth = %v，預期 %v", c.name, got, c.want)
			}
		}()
	}
}

// 實際的 Paletted PNG／GIF 走 DecodeConfig → checkImageConfig 不 panic，且適用一般上限（9M 像素允許）。
func TestGate4b_DecodeConfig_PalettedUsesNormalLimit(t *testing.T) {
	src := image.NewPaletted(image.Rect(0, 0, 3000, 3000), palette.Plan9)
	var pngBuf, gifBuf bytes.Buffer
	if err := png.Encode(&pngBuf, src); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifBuf, src, nil); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"png": pngBuf.Bytes(), "gif": gifBuf.Bytes()} {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cfg.ColorModel.(color.Palette); !ok {
			t.Errorf("%s：ColorModel %T，預期 color.Palette", name, cfg.ColorModel)
		}
		if err := checkImageConfig(cfg); err != nil {
			t.Errorf("%s 3000x3000 paletted：應允許，得到 %v", name, err)
		}
	}
}

// itemOnly 只實作 TextRecognizer + itemRecognizer，記錄每次收到的圖寬。
type itemOnly struct {
	mu     sync.Mutex
	widths []int
	items  map[int][]OCRItem // 依圖寬回傳的文字框
}

func (f *itemOnly) RecognizeItems(ctx context.Context, img image.Image) ([]OCRItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.widths = append(f.widths, img.Bounds().Dx())
	return f.items[img.Bounds().Dx()], nil
}

func (f *itemOnly) RecognizeText(ctx context.Context, img image.Image) (string, error) {
	items, err := f.RecognizeItems(ctx, img)
	if err != nil {
		return "", err
	}
	if s := JoinOCRItems(items); s != "" {
		return s, nil
	}
	return "", ErrNoText
}

func hbox(x, y, w, h float64, text string) OCRItem {
	return OCRItem{Box: [4][2]float64{{x, y}, {x + w, y}, {x + w, y + h}, {x, y + h}}, Text: text, Score: 0.9}
}

// 排隊包裝下仍會判斷字幕區直書框（SerializeText 包住 itemRecognizer 時不能遺失直書判斷）。
func TestGate4b_SerializedFallbackStillDetectsVerticalBox(t *testing.T) {
	f := &itemOnly{items: map[int][]OCRItem{
		64:  {hbox(0, 0, 10, 40, "直")},
		128: {hbox(0, 0, 50, 10, "整張")},
	}}
	q := SerializeText(f)
	got, err := RecognizeWithFallback(context.Background(), q, solid(64, 10, color.White), solid(128, 72, color.White))
	if err != nil || got != "整張" {
		t.Errorf("得到 %q %v，預期 整張", got, err)
	}
	if want := []int{64, 128}; fmt.Sprint(f.widths) != fmt.Sprint(want) {
		t.Errorf("辨識順序 %v，預期 %v", f.widths, want)
	}
}

// 排隊包裝下高 > 寬只辨識整張一次。
func TestGate4b_SerializedFallbackTallImageWholeOnly(t *testing.T) {
	f := &itemOnly{items: map[int][]OCRItem{50: {hbox(0, 0, 40, 10, "整張")}}}
	q := SerializeText(f)
	got, err := RecognizeWithFallback(context.Background(), q, solid(64, 10, color.White), solid(50, 100, color.White))
	if err != nil || got != "整張" {
		t.Errorf("得到 %q %v", got, err)
	}
	if want := []int{50}; fmt.Sprint(f.widths) != fmt.Sprint(want) {
		t.Errorf("辨識順序 %v，預期 %v", f.widths, want)
	}
}

// 排隊包裝：排隊時 ctx 取消 → 不辨識、回 ctx 錯誤。
func TestGate4b_SerializedFallbackCanceledWhileQueued(t *testing.T) {
	block := make(chan struct{})
	g := &gatedText{entered: make(chan struct{}), release: block}
	q := SerializeText(g)
	go q.RecognizeText(context.Background(), solid(10, 10, color.White))
	<-g.entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := RecognizeWithFallback(ctx, q, solid(10, 5, color.White), solid(20, 10, color.White))
	close(block)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("排隊中逾時應回 DeadlineExceeded，得到 %v", err)
	}
}

// 直書對話框：三欄經由中間欄間接相連 → 同一行（union-find 遞移）；遠處另一對話框另起一行，由右至左。
func TestGate4b_JoinOCRItems_VerticalBubbleTransitive(t *testing.T) {
	items := []OCRItem{
		// 右側對話框：三欄，欄寬 10、間距 8（≤ 欄寬），y 範圍重疊
		hbox(300, 0, 10, 60, "一"),
		hbox(282, 10, 10, 60, "二"),
		hbox(264, 20, 10, 60, "三"),
		// 左側對話框：間距遠大於欄寬
		hbox(100, 0, 10, 60, "左甲"),
		hbox(84, 0, 10, 60, "左乙"),
	}
	got := JoinOCRItems(items)
	if want := "一二三\n左甲左乙"; got != want {
		t.Errorf("得到 %q，預期 %q", got, want)
	}
}

// 直書欄與橫書框混合、零寬高框：不 panic，橫書在前。
func TestGate4b_JoinOCRItems_MixedAndDegenerate(t *testing.T) {
	items := []OCRItem{
		hbox(0, 0, 0, 0, "點"),
		hbox(200, 0, 10, 60, "直"),
		hbox(0, 100, 50, 10, "橫"),
	}
	got := JoinOCRItems(items)
	if want := "點\n橫\n直"; got != want {
		t.Errorf("得到 %q，預期 %q", got, want)
	}
}
