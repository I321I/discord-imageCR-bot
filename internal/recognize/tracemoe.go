package recognize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/image/draw"
)

// WorkRecognizer 從圖片（PNG）辨識作品名稱。
type WorkRecognizer interface {
	RecognizeWork(ctx context.Context, png []byte) (string, error)
}

// ErrNotFound 表示沒有可信的辨識結果（沒有結果、相似度低於門檻或沒有任何名稱）。
var ErrNotFound = errors.New("no confident match")

// TraceMoe 是 trace.moe 搜尋 API 的 client。
type TraceMoe struct {
	BaseURL       string
	Client        *http.Client
	MinSimilarity float64       // 相似度低於此值視為 ErrNotFound（TRACEMOE_MIN_SIMILARITY）
	Key           string        // 空 = 不帶 key（TRACEMOE_KEY）
	Timeout       time.Duration // 每張圖的逾時（不含排隊等待）；0 = 只看呼叫端的 ctx
}

// NewTraceMoe 回傳連到 https://api.trace.moe 的 client；key 與 minSimilarity 來自 config。
// 每張圖逾時 30 秒（逾時算辨識失敗）。
func NewTraceMoe(key string, minSimilarity float64) *TraceMoe {
	return &TraceMoe{
		BaseURL:       "https://api.trace.moe",
		Client:        &http.Client{},
		MinSimilarity: minSimilarity,
		Key:           key,
		Timeout:       30 * time.Second,
	}
}

type traceMoeResponse struct {
	Error  string `json:"error"`
	Result []struct {
		Anilist struct {
			Title struct {
				Native  string `json:"native"`
				Romaji  string `json:"romaji"`
				English string `json:"english"`
				Chinese string `json:"chinese"`
			} `json:"title"`
			Synonyms []string `json:"synonyms"`
		} `json:"anilist"`
		Similarity float64 `json:"similarity"`
	} `json:"result"`
}

// RecognizeWork 以 POST {BaseURL}/search?anilistInfo&cutBorders 送出圖片，取第一筆結果的作品名稱（見 workName）。
// 沒有結果、相似度低於門檻或沒有任何名稱 → ErrNotFound；HTTP 非 2xx（402 額度用完、429、5xx 等）、
// JSON 的 error 非空或格式錯誤 → 其他錯誤；超過 Timeout → 包含 context.DeadlineExceeded 的錯誤。
// 圖片應先經 PrepareForTraceMoe 縮小。
func (t *TraceMoe) RecognizeWork(ctx context.Context, png []byte) (string, error) {
	if t.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t.Timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.BaseURL+"/search?anilistInfo&cutBorders", bytes.NewReader(png))
	if err != nil {
		return "", fmt.Errorf("建立 trace.moe 請求失敗：%w", err)
	}
	req.Header.Set("Content-Type", "image/png")
	if t.Key != "" {
		req.Header.Set("x-trace-key", t.Key)
	}
	resp, err := t.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("trace.moe 請求失敗：%w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("讀取 trace.moe 回應失敗：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("trace.moe 回應 HTTP %d：%s", resp.StatusCode, truncateRunes(strings.TrimSpace(string(body)), errorBodyMaxRunes))
	}
	var r traceMoeResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("解析 trace.moe 回應失敗：%w", err)
	}
	if r.Error != "" {
		return "", fmt.Errorf("trace.moe 回應錯誤：%s", r.Error)
	}
	if len(r.Result) == 0 || r.Result[0].Similarity < t.MinSimilarity {
		return "", ErrNotFound
	}
	a := r.Result[0].Anilist
	name := workName(a.Title.Chinese, a.Title.Native, a.Title.English, a.Title.Romaji, a.Synonyms)
	if name == "" {
		return "", ErrNotFound
	}
	return name, nil
}

// workName 組出作品名稱（10/28 使用者決定，取代 Q14）：有中文時只放一個中文名稱——title.chinese 優先，
// 其次 synonyms 中第一個中文別名；沒有中文時放 native、english（以「、」連接、去重，缺一個只放另一個）；
// 兩者都沒有才用 romaji。都沒有回 ""。
func workName(chinese, native, english, romaji string, synonyms []string) string {
	if c := strings.TrimSpace(chinese); c != "" {
		return c
	}
	for _, s := range synonyms {
		if isChinese(s) {
			return strings.TrimSpace(s)
		}
	}
	if name := joinNames([]string{native, english}); name != "" {
		return name
	}
	return strings.TrimSpace(romaji)
}

// errorBodyMaxRunes 是錯誤訊息中保留的回應本文字數上限。
const errorBodyMaxRunes = 300

// truncateRunes 回傳 s 的前 n 個字（rune），有截斷時結尾加「…」。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// joinNames 去除各名稱前後空白、略過空的與重複的，以「、」連接。
// 不拆名稱中的逗號（名稱本身可能含「,」）。
func joinNames(names []string) string {
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return strings.Join(out, "、")
}

// isChinese 回傳 s 是否為中文名稱：含漢字且不含平假名、片假名（含假名的視為日文）。
func isChinese(s string) bool {
	hasHan := false
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Hiragana, r), unicode.Is(unicode.Katakana, r):
			return false
		case unicode.Is(unicode.Han, r):
			hasHan = true
		}
	}
	return hasHan
}

// traceMoeMaxLongSide 是送 trace.moe 前圖片的長邊上限（px）。
const traceMoeMaxLongSide = 1000

// PrepareForTraceMoe 把長邊超過 1000px 的圖等比縮到長邊 1000px；否則原樣回傳。
func PrepareForTraceMoe(img image.Image) image.Image {
	b := img.Bounds()
	if max(b.Dx(), b.Dy()) <= traceMoeMaxLongSide {
		return img
	}
	w, h := shrinkToLongSide(b.Dx(), b.Dy(), traceMoeMaxLongSide)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.BiLinear.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}
