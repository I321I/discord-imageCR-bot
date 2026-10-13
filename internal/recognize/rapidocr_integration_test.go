//go:build rapidocr

// 需要 Python 3 與 ocr/requirements.txt 的套件（Docker 映像的 test 階段）。
// 執行：go test -tags rapidocr ./internal/recognize/
package recognize

import (
	"context"
	"errors"
	"image/color"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
)

// integrationOCR 回傳執行專案內 ocr/ocr_worker.py 的 RapidOCR（其餘設定同 NewRapidOCR）。
func integrationOCR(t *testing.T) *RapidOCR {
	t.Helper()
	worker, err := filepath.Abs("../../ocr/ocr_worker.py")
	if err != nil {
		t.Fatal(err)
	}
	r := NewRapidOCR()
	r.Command = []string{"python3", worker}
	t.Cleanup(func() { r.Close() })
	return r
}

// charHitRate 回傳 want 的字元（不含空白）中有出現在 got 的比例。
func charHitRate(want, got string) float64 {
	total, hit := 0, 0
	for _, r := range want {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if strings.ContainsRune(got, r) {
			hit++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(hit) / float64(total)
}

// 使用者提供的真實截圖，走與自動處理相同的流程：解碼 → RecognizeWithFallback（字幕區；沒字、有直書框或直的圖則整張）→ RapidOCR。
// 圖片不在 repo 中（版權），找不到的圖片略過。
// 字幕與電視節目截圖：命中率（正確字元出現在結果中的比例）須 ≥ 90%（Q64）。
// 漫畫只列部分文字，命中率先記錄、不判定失敗（待 Pi 試驗結果）。
func TestRapidOCR_RealImages(t *testing.T) {
	const minHitRate = 0.9
	r := integrationOCR(t)
	for _, c := range []struct {
		file, want string
		gated      bool
	}{
		{"sub_two_lines_right.png", "我只要能跟女孩子親親我我\n其他都不重要啦", true}, // 1920x1080，靠右兩行
		{"sub_phone_letterbox.webp", "回家吃飯來一發再睡覺", true},           // 手機截圖，左右黑邊
		{"sub_one_line.webp", "工作就是拷問", true},
		{"sub_japanese.webp", "わたしたち兄妹なんだよ", true},                                 // 日文字幕
		{"tv_show.webp", "不應該生活在世界上", true},                                        // 真人電視節目
		{"manga_vertical_baki.webp", "您竟然會自曝其短生怕別人不知您是個外行人", false},                // 漫畫直書（中文）
		{"manga_vertical_ramen.webp", "日式拉麵這種亂敲人竹槓的不入流料理能收掉一間就是台灣吃的文化之福什麼", false}, // 漫畫直書（中文）
		{"manga_4koma_japanese.webp", "前から好きだった螺子さんに告白した初めての夜僕らはひとつになっただがそんな僕らの関係も長くは続かなかった", false}, // 四格漫畫（日文）
		{"manga_page_dense.webp", "我應該教過你不可或缺的三感了吧難道你忘光了嗎所謂的如果女主沒有去追求身體的快樂就完全不及格", false},            // 文字密集的漫畫頁
	} {
		t.Run(c.file, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + c.file)
			if errors.Is(err, fs.ErrNotExist) {
				// 真實截圖有版權，只留在本機（.gitignore 排除），找不到就略過。
				t.Skipf("找不到測試圖片 %s，略過", c.file)
			}
			if err != nil {
				t.Fatal(err)
			}
			img, err := DecodeFirstFrame(data)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			got, err := RecognizeWithFallback(context.Background(), r, PrepareForOCR(img), PrepareWholeForOCR(img))
			if err != nil && !errors.Is(err, ErrNoText) {
				t.Fatal(err)
			}
			rate := charHitRate(c.want, got)
			t.Logf("耗時 %v；命中率 %.0f%%\n辨識結果：%q\n正確文字：%q", time.Since(start).Round(time.Millisecond), rate*100, got, c.want)
			if c.gated && rate < minHitRate {
				t.Errorf("命中率 %.0f%% 低於 %.0f%%", rate*100, minHitRate*100)
			}
		})
	}
}

func TestRapidOCR_RealBlankImageIsNoText(t *testing.T) {
	img := solid(640, 360, color.White)
	r := integrationOCR(t)
	if _, err := RecognizeWithFallback(context.Background(), r, PrepareForOCR(img), PrepareWholeForOCR(img)); !errors.Is(err, ErrNoText) {
		t.Errorf("空白圖應回 ErrNoText，得到 %v", err)
	}
}
