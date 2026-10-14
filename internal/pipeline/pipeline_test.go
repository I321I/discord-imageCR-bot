package pipeline

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"discordbot/internal/imagemsg"
	"discordbot/internal/recognize"

	"github.com/bwmarrin/discordgo"
)

// pngBytes 回傳 w x h 的單色 PNG。
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.White)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// workFunc、textFunc 是以函式實作的假辨識器。
type workFunc func(ctx context.Context, png []byte) (string, error)

func (f workFunc) RecognizeWork(ctx context.Context, png []byte) (string, error) { return f(ctx, png) }

type textFunc func(ctx context.Context, img image.Image) (string, error)

func (f textFunc) RecognizeText(ctx context.Context, img image.Image) (string, error) {
	return f(ctx, img)
}

func okWork(string) workFunc {
	return func(context.Context, []byte) (string, error) { return "葬送のフリーレン", nil }
}

func okText() textFunc {
	return func(context.Context, image.Image) (string, error) { return "這就是人類啊", nil }
}

func TestRecognize_BothOK(t *testing.T) {
	p := &Pipeline{Work: okWork(""), Text: okText()}
	got := p.Recognize(context.Background(), pngBytes(t, 64, 36))
	want := imagemsg.Fields{Work: "葬送のフリーレン", Text: "這就是人類啊"}
	if got != want {
		t.Errorf("得到 %+v，預期 %+v", got, want)
	}
}

func TestRecognize_WorkFails_TextOK(t *testing.T) {
	p := &Pipeline{
		Work: workFunc(func(context.Context, []byte) (string, error) { return "", recognize.ErrNotFound }),
		Text: okText(),
	}
	got := p.Recognize(context.Background(), pngBytes(t, 64, 36))
	if got.Work != imagemsg.FailedMarker || got.Text != "這就是人類啊" {
		t.Errorf("得到 %+v", got)
	}
}

func TestRecognize_TextFails_WorkOK(t *testing.T) {
	p := &Pipeline{
		Work: okWork(""),
		Text: textFunc(func(context.Context, image.Image) (string, error) { return "", errors.New("worker 掛了") }),
	}
	got := p.Recognize(context.Background(), pngBytes(t, 64, 36))
	if got.Work != "葬送のフリーレン" || got.Text != imagemsg.FailedMarker {
		t.Errorf("得到 %+v", got)
	}
}

// Q16：OCR 沒讀到任何字 → 辨識失敗。
func TestRecognize_NoTextIsFailed(t *testing.T) {
	p := &Pipeline{
		Work: okWork(""),
		Text: textFunc(func(context.Context, image.Image) (string, error) { return "", recognize.ErrNoText }),
	}
	if got := p.Recognize(context.Background(), pngBytes(t, 64, 36)); got.Text != imagemsg.FailedMarker {
		t.Errorf("沒讀到字應為辨識失敗，得到 %+v", got)
	}
}

func TestRecognize_DecodeFails(t *testing.T) {
	var called atomic.Bool
	p := &Pipeline{
		Work: workFunc(func(context.Context, []byte) (string, error) { called.Store(true); return "x", nil }),
		Text: textFunc(func(context.Context, image.Image) (string, error) { called.Store(true); return "x", nil }),
	}
	got := p.Recognize(context.Background(), []byte("not an image"))
	want := imagemsg.Fields{Work: imagemsg.FailedMarker, Text: imagemsg.FailedMarker}
	if got != want {
		t.Errorf("得到 %+v，預期兩欄都失敗", got)
	}
	if called.Load() {
		t.Error("解碼失敗不應呼叫辨識器")
	}
}

// 尺寸超過上限（ErrImageTooLarge）→ 兩欄都失敗。
func TestRecognize_TooLarge(t *testing.T) {
	p := &Pipeline{Work: okWork(""), Text: okText()}
	got := p.Recognize(context.Background(), pngBytes(t, 10001, 1))
	if got.Work != imagemsg.FailedMarker || got.Text != imagemsg.FailedMarker {
		t.Errorf("超過尺寸上限應兩欄都失敗，得到 %+v", got)
	}
}

// trace.moe 與 OCR 並行：兩個假辨識器互相等待對方開始，循序執行會逾時。
func TestRecognize_WorkAndTextParallel(t *testing.T) {
	workStarted, textStarted := make(chan struct{}), make(chan struct{})
	wait := func(ch chan struct{}) error {
		select {
		case <-ch:
			return nil
		case <-time.After(2 * time.Second):
			return errors.New("等不到另一個辨識器開始")
		}
	}
	p := &Pipeline{
		Work: workFunc(func(context.Context, []byte) (string, error) {
			close(workStarted)
			if err := wait(textStarted); err != nil {
				return "", err
			}
			return "作品", nil
		}),
		Text: textFunc(func(context.Context, image.Image) (string, error) {
			close(textStarted)
			if err := wait(workStarted); err != nil {
				return "", err
			}
			return "文字", nil
		}),
	}
	got := p.Recognize(context.Background(), pngBytes(t, 64, 36))
	if got.Work != "作品" || got.Text != "文字" {
		t.Errorf("trace.moe 與 OCR 應並行，得到 %+v", got)
	}
}

// 假 OCR 收到的是前處理後的字幕區（寬 ≤ 640），trace.moe 收到長邊 ≤ 1000 的 PNG。
func TestRecognize_RecognizersGetPreparedImages(t *testing.T) {
	var ocrSize, workSize image.Point
	p := &Pipeline{
		Work: workFunc(func(_ context.Context, data []byte) (string, error) {
			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				return "", err
			}
			workSize = img.Bounds().Size()
			return "作品", nil
		}),
		Text: textFunc(func(_ context.Context, img image.Image) (string, error) {
			ocrSize = img.Bounds().Size()
			return "文字", nil
		}),
	}
	p.Recognize(context.Background(), pngBytes(t, 1920, 1080))
	if ocrSize.X > 640 || ocrSize.X == 0 || ocrSize.Y >= 1080/2 {
		t.Errorf("OCR 應收到寬 ≤ 640 的字幕區，得到 %v", ocrSize)
	}
	if max(workSize.X, workSize.Y) != 1000 {
		t.Errorf("trace.moe 應收到長邊 1000 的 PNG，得到 %v", workSize)
	}
}

// 第一張的辨識阻塞時，第二張仍能完成解碼並進入辨識（解碼位置在辨識前就釋放）。
func TestRecognize_ReleasesDecodeSlotBeforeRecognizing(t *testing.T) {
	var mu sync.Mutex
	entered := 0
	bothIn := make(chan struct{})
	p := &Pipeline{
		Work: workFunc(func(context.Context, []byte) (string, error) {
			mu.Lock()
			entered++
			if entered == 2 {
				close(bothIn)
			}
			mu.Unlock()
			select {
			case <-bothIn:
				return "作品", nil
			case <-time.After(2 * time.Second):
				return "", errors.New("第二張沒有進入辨識")
			}
		}),
		Text: okText(),
	}
	var wg sync.WaitGroup
	results := make([]imagemsg.Fields, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = p.Recognize(context.Background(), pngBytes(t, 64, 36))
		}()
	}
	wg.Wait()
	for i, r := range results {
		if r.Work != "作品" {
			t.Errorf("第 %d 張：得到 %+v（解碼位置應在辨識前釋放）", i+1, r)
		}
	}
}

// 同一時間只解碼一張：解碼位置被佔用時，Recognize 等待；ctx 結束則兩欄都失敗、不呼叫辨識器。
func TestRecognize_WaitsForDecodeSlot(t *testing.T) {
	var called atomic.Bool
	p := &Pipeline{
		Work: workFunc(func(context.Context, []byte) (string, error) { called.Store(true); return "x", nil }),
		Text: textFunc(func(context.Context, image.Image) (string, error) { called.Store(true); return "x", nil }),
	}
	release := p.acquireDecode(context.Background())
	if release == nil {
		t.Fatal("應取得解碼位置")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got := p.Recognize(ctx, pngBytes(t, 64, 36))
	release()
	if got.Work != imagemsg.FailedMarker || got.Text != imagemsg.FailedMarker || called.Load() {
		t.Errorf("等待解碼位置時 ctx 結束應兩欄失敗且不辨識，得到 %+v called=%v", got, called.Load())
	}
	if got := p.Recognize(context.Background(), pngBytes(t, 64, 36)); got.Work != "x" {
		t.Errorf("釋放後應可再解碼，得到 %+v", got)
	}
}

var (
	_ API     = (*discordgo.Session)(nil)
	_ Fetcher = (*HTTPFetcher)(nil)
)
