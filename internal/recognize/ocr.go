package recognize

import (
	"context"
	"errors"
	"image"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/image/draw"
)

// TextRecognizer 辨識圖片中的文字（可替換；目前為 RapidOCR）。
// 傳入的 img 必須已經過 PrepareForOCR 或 PrepareWholeForOCR。沒讀到任何字時回 ErrNoText（Q16：視為辨識失敗）。
type TextRecognizer interface {
	RecognizeText(ctx context.Context, img image.Image) (string, error)
}

// ErrNoText 表示 OCR 沒讀到任何文字。
var ErrNoText = errors.New("no text recognized")

// OCR 前處理參數（字幕區寬度、整張長邊上限暫定，待 Pi 試驗定案）。
const (
	ocrBottomPercent = 35   // 字幕區域：圖片下方 35%
	ocrSubtitleWidth = 640  // 字幕區域寬度上限（px）
	ocrWholeLongSide = 1280 // 整張辨識時長邊上限（px）
)

// PrepareForOCR 裁下方 35%（字幕區域）；寬度超過 640px 時等比縮到寬 640（只縮不放大、保留顏色）。
func PrepareForOCR(img image.Image) image.Image {
	b := img.Bounds()
	h := b.Dy() * ocrBottomPercent / 100
	if h < 1 && b.Dy() > 0 {
		h = 1
	}
	src := image.Rect(b.Min.X, b.Max.Y-h, b.Max.X, b.Max.Y)
	w, dh := src.Dx(), src.Dy()
	if w > ocrSubtitleWidth {
		w, dh = shrinkToLongSide(w, dh, ocrSubtitleWidth)
	}
	return scaledRGBA(img, src, w, dh)
}

// PrepareWholeForOCR 整張圖；長邊超過 1280px 時等比縮到長邊 1280（只縮不放大、保留顏色）。
// 字幕區域沒有文字時使用（Q57）。
func PrepareWholeForOCR(img image.Image) image.Image {
	b := img.Bounds()
	w, h := shrinkToLongSide(b.Dx(), b.Dy(), ocrWholeLongSide)
	return scaledRGBA(img, b, w, h)
}

// scaledRGBA 把 img 的 src 區域縮放成 w x h 的 *image.RGBA（CatmullRom，縮小時不產生鋸齒）。
func scaledRGBA(img image.Image, src image.Rectangle, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if w == src.Dx() && h == src.Dy() {
		draw.Draw(dst, dst.Bounds(), img, src.Min, draw.Src)
		return dst
	}
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, src, draw.Src, nil)
	return dst
}

// RecognizeWithFallback 先辨識字幕區域 subtitle（PrepareForOCR 的結果）；沒讀到字（ErrNoText）時
// 自動對整張 whole（PrepareWholeForOCR 的結果）再辨識一次（Q57 (b)）。其他錯誤直接回傳、不重試。
func RecognizeWithFallback(ctx context.Context, t TextRecognizer, subtitle, whole image.Image) (string, error) {
	text, err := t.RecognizeText(ctx, subtitle)
	if !errors.Is(err, ErrNoText) {
		return text, err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return t.RecognizeText(ctx, whole)
}

// OCRItem 是 OCR 讀到的一個文字框：Box 為四個頂點 (x, y)，Text 為框內文字，Score 為信心度。
type OCRItem struct {
	Box   [4][2]float64
	Text  string
	Score float64
}

// verticalRatio：文字框高/寬 ≥ 此值視為直書（RapidOCR 會把這種框轉 90 度再辨識）。
const verticalRatio = 1.5

// span 是文字框在某一軸上的範圍。
type span struct{ lo, hi float64 }

func (s span) center() float64 { return (s.lo + s.hi) / 2 }
func (s span) size() float64   { return s.hi - s.lo }

// ocrBox 是文字框的外接矩形與文字。
type ocrBox struct {
	x, y span
	text string
}

// JoinOCRItems 把文字框組成文字：
// 橫書框依垂直範圍分行（與目前行重疊 ≥ 較矮者 50% 視為同一行），行由上到下、行內由左到右；
// 直書框（高/寬 ≥ 1.5）依水平範圍分欄（規則相同），欄由右至左、欄內由上到下；
// 每行或每欄一行文字，橫書行在前、直書欄在後；最後套用 CleanOCRText。不依信心度過濾（RapidOCR 內建已過濾，Q66）。
func JoinOCRItems(items []OCRItem) string {
	var horizontal, vertical []ocrBox
	for _, it := range items {
		if strings.TrimSpace(it.Text) == "" {
			continue
		}
		b := ocrBox{x: span{it.Box[0][0], it.Box[0][0]}, y: span{it.Box[0][1], it.Box[0][1]}, text: it.Text}
		for _, p := range it.Box[1:] {
			b.x = span{min(b.x.lo, p[0]), max(b.x.hi, p[0])}
			b.y = span{min(b.y.lo, p[1]), max(b.y.hi, p[1])}
		}
		if b.y.size() >= verticalRatio*b.x.size() {
			vertical = append(vertical, b)
		} else {
			horizontal = append(horizontal, b)
		}
	}
	var lines []string
	// 橫書：依 y 分行，行由上到下，行內依 x 由左到右。
	lines = append(lines, groupBoxes(horizontal, func(b ocrBox) span { return b.y }, func(b ocrBox) span { return b.x }, false)...)
	// 直書：依 x 分欄，欄由右至左，欄內依 y 由上到下。
	lines = append(lines, groupBoxes(vertical, func(b ocrBox) span { return b.x }, func(b ocrBox) span { return b.y }, true)...)
	return CleanOCRText(strings.Join(lines, "\n"))
}

// groupBoxes 依 across 軸把文字框分組（與目前組的範圍重疊 ≥ 較小者 50% 視為同一組），
// 組依 across 中心排序（reverse 為由大到小），組內依 along 軸由小到大，以空白連接成一行。
func groupBoxes(boxes []ocrBox, across, along func(ocrBox) span, reverse bool) []string {
	sort.SliceStable(boxes, func(i, j int) bool {
		if reverse {
			return across(boxes[i]).center() > across(boxes[j]).center()
		}
		return across(boxes[i]).center() < across(boxes[j]).center()
	})
	var groups [][]ocrBox
	var cur span
	for _, b := range boxes {
		r := across(b)
		if len(groups) > 0 {
			overlap := min(cur.hi, r.hi) - max(cur.lo, r.lo)
			if overlap >= 0.5*min(cur.size(), r.size()) {
				last := len(groups) - 1
				groups[last] = append(groups[last], b)
				cur = span{min(cur.lo, r.lo), max(cur.hi, r.hi)}
				continue
			}
		}
		groups = append(groups, []ocrBox{b})
		cur = r
	}
	lines := make([]string, 0, len(groups))
	for _, g := range groups {
		sort.SliceStable(g, func(i, j int) bool { return along(g[i]).lo < along(g[j]).lo })
		texts := make([]string, len(g))
		for i, b := range g {
			texts[i] = b.text
		}
		lines = append(lines, strings.Join(texts, " "))
	}
	return lines
}

// CleanOCRText 清理 OCR 結果（Q18）：移除中日文字之間的空白、修剪每行前後空白、刪除空行。
// 英文等其他文字之間的空白保留；不依信心度過濾。
func CleanOCRText(s string) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(removeCJKSpaces(line))
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// removeCJKSpaces 移除前後都是中日文字（含全形標點）的空白。
func removeCJKSpaces(line string) string {
	r := []rune(line)
	var out []rune
	for i := 0; i < len(r); i++ {
		if !unicode.IsSpace(r[i]) {
			out = append(out, r[i])
			continue
		}
		j := i
		for j < len(r) && unicode.IsSpace(r[j]) {
			j++
		}
		if len(out) > 0 && j < len(r) && isCJK(out[len(out)-1]) && isCJK(r[j]) {
			i = j - 1
			continue
		}
		out = append(out, r[i:j]...)
		i = j - 1
	}
	return string(out)
}

// isCJK 回傳 r 是否為中日文字：漢字、平假名、片假名、CJK 標點（U+3000–303F）或全形字元（U+FF00–FFEF）。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) ||
		r == 'ー' || (r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF)
}

// shrinkToLongSide 回傳 w x h 等比縮到長邊 maxLong 後的尺寸；長邊不超過 maxLong 時原樣回傳（不放大）。
// 原本非 0 的邊至少為 1。
func shrinkToLongSide(w, h, maxLong int) (int, int) {
	long := max(w, h)
	if long <= maxLong {
		return w, h
	}
	fit := func(n int) int {
		if n == 0 {
			return 0
		}
		return max(n*maxLong/long, 1)
	}
	return fit(w), fit(h)
}
