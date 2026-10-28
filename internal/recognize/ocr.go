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

// itemRecognizer 是能回傳文字框的 TextRecognizer（RapidOCR）；RecognizeWithFallback 用它判斷字幕區是否有直書框。
type itemRecognizer interface {
	RecognizeItems(ctx context.Context, img image.Image) ([]OCRItem, error)
}

// fallbackRecognizer 由排隊包裝（SerializeText）實作：佔用一個名額完成整個 RecognizeWithFallback，整張重試不重新排隊。
type fallbackRecognizer interface {
	recognizeWithFallback(ctx context.Context, subtitle, whole image.Image) (string, error)
}

// RecognizeWithFallback 辨識圖片文字：subtitle 為字幕區（PrepareForOCR 的結果），whole 為整張（PrepareWholeForOCR 的結果）。
// 以下情況改辨識整張，並以整張的結果為準：
//   - 圖片高 > 寬（whole 與原圖同比例）：直接辨識整張，不先讀字幕區；
//   - 字幕區沒讀到字（ErrNoText，Q57 (b)）；
//   - 字幕區讀到直書框（高/寬 ≥ 1.5；t 能回傳文字框時才判斷）；
//   - 字幕區有文字框的上緣落在字幕區高度上方 20% 以內（y.lo ≤ subtitleTopRegionRatio × 字幕區高度，10/28；t 能回傳文字框時才判斷）。
//
// 字幕區的其他錯誤直接回傳、不重試。t 為 SerializeText 的包裝時，整個流程只排隊一次（整張重試接續處理）。
func RecognizeWithFallback(ctx context.Context, t TextRecognizer, subtitle, whole image.Image) (string, error) {
	if f, ok := t.(fallbackRecognizer); ok {
		return f.recognizeWithFallback(ctx, subtitle, whole)
	}
	if b := whole.Bounds(); b.Dy() > b.Dx() {
		return t.RecognizeText(ctx, whole)
	}
	text, useWhole, err := recognizeSubtitle(ctx, t, subtitle)
	if !useWhole {
		return text, err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return t.RecognizeText(ctx, whole)
}

// recognizeSubtitle 辨識字幕區；useWhole 為 true 表示要改辨識整張（沒讀到字、讀到直書框或有框落在字幕區上方）。
func recognizeSubtitle(ctx context.Context, t TextRecognizer, subtitle image.Image) (text string, useWhole bool, err error) {
	ir, ok := t.(itemRecognizer)
	if !ok {
		text, err := t.RecognizeText(ctx, subtitle)
		return text, errors.Is(err, ErrNoText), err
	}
	items, err := ir.RecognizeItems(ctx, subtitle)
	if err != nil {
		return "", errors.Is(err, ErrNoText), err
	}
	for _, b := range toBoxes(items) {
		if b.vertical() {
			return "", true, nil
		}
	}
	if subtitleTouchesTop(items, subtitle.Bounds().Dy()) {
		return "", true, nil
	}
	text = JoinOCRItems(items)
	return text, text == "", nil
}

// subtitleTopRegionRatio：字幕區的文字框上緣 y.lo ≤ 此比例 × 字幕區高度（縮放後字幕區像素，y=0 為裁切上緣）
// 視為落在字幕區上方，表示字幕區切到了畫面中的文字（例如直書漫畫的最後一字），改辨識整張（10/28 追加修正）。
// 字幕樣本的字幕位置約在字幕區高度 47%～90%。
const subtitleTopRegionRatio = 0.2

// subtitleTouchesTop 回傳是否有文字框的上緣落在字幕區上方 subtitleTopRegionRatio 以內（略過空白文字，同 toBoxes）。
// height 為字幕區影像高度（框座標與該影像同一座標系）；height ≤ 0 時不判斷。
func subtitleTouchesTop(items []OCRItem, height int) bool {
	if height <= 0 {
		return false
	}
	limit := subtitleTopRegionRatio * float64(height)
	for _, b := range toBoxes(items) {
		if b.y.lo <= limit {
			return true
		}
	}
	return false
}

// OCRItem 是 OCR 讀到的一個文字框：Box 為四個頂點 (x, y)，Text 為框內文字，Score 為信心度。
type OCRItem struct {
	Box   [4][2]float64
	Text  string
	Score float64
}

// verticalRatio：文字框高/寬 ≥ 此值視為直書（RapidOCR 會把這種框轉 90 度再辨識）。
const verticalRatio = 1.5

// bubbleGapRatio：兩個直書欄的水平間距 ≤ 較寬欄寬度的此倍數、且垂直範圍有重疊時，視為同一對話框；
// 同一欄內上下兩框的間距 > 較寬框寬度的此倍數時，視為不同對話框。
const bubbleGapRatio = 1.0

// span 是文字框在某一軸上的範圍。
type span struct{ lo, hi float64 }

func (s span) center() float64 { return (s.lo + s.hi) / 2 }
func (s span) size() float64   { return s.hi - s.lo }

// union 回傳同時涵蓋 s 與 o 的範圍。
func (s span) union(o span) span { return span{min(s.lo, o.lo), max(s.hi, o.hi)} }

// ocrBox 是文字框的外接矩形與文字。
type ocrBox struct {
	x, y span
	text string
}

// vertical 回傳框是否為直書（高 > 0 且高/寬 ≥ 1.5）；零寬高的框不算直書。
func (b ocrBox) vertical() bool {
	return b.y.size() > 0 && b.y.size() >= verticalRatio*b.x.size()
}

// toBoxes 把 OCRItem 轉成外接矩形，略過空白文字。
func toBoxes(items []OCRItem) []ocrBox {
	var boxes []ocrBox
	for _, it := range items {
		if strings.TrimSpace(it.Text) == "" {
			continue
		}
		b := ocrBox{x: span{it.Box[0][0], it.Box[0][0]}, y: span{it.Box[0][1], it.Box[0][1]}, text: it.Text}
		for _, p := range it.Box[1:] {
			b.x = b.x.union(span{p[0], p[0]})
			b.y = b.y.union(span{p[1], p[1]})
		}
		boxes = append(boxes, b)
	}
	return boxes
}

// JoinOCRItems 把文字框組成文字：
// 橫書框依垂直範圍分行（與目前行重疊 ≥ 較矮者 50% 視為同一行），行由上到下、行內由左到右以空白連接；
// 直書框（高/寬 ≥ 1.5）依水平範圍分欄（規則相同），欄內由上到下，上下間距 > 較寬框寬度時拆成不同段；同一對話框的欄（相鄰欄水平間距 ≤ 較寬欄寬度、
// 垂直範圍有重疊）由右至左接成一行（不加空白），不同對話框各一行，依最右欄的位置由右至左排列；
// 橫書行在前、直書在後；最後套用 CleanOCRText。不依信心度過濾（RapidOCR 內建已過濾，Q66）。
func JoinOCRItems(items []OCRItem) string {
	var horizontal, vertical []ocrBox
	for _, b := range toBoxes(items) {
		if b.vertical() {
			vertical = append(vertical, b)
		} else {
			horizontal = append(horizontal, b)
		}
	}
	var lines []string
	// 橫書：依 y 分行，行由上到下，行內依 x 由左到右。
	for _, g := range groupBoxes(horizontal, func(b ocrBox) span { return b.y }, func(b ocrBox) span { return b.x }, false) {
		lines = append(lines, joinTexts(g, " "))
	}
	// 直書：依 x 分欄（由右至左），欄內依 y 由上到下；再把同一對話框的欄接成一行。
	columns := groupBoxes(vertical, func(b ocrBox) span { return b.x }, func(b ocrBox) span { return b.y }, true)
	lines = append(lines, joinColumns(splitColumns(columns))...)
	return CleanOCRText(strings.Join(lines, "\n"))
}

// groupBoxes 依 across 軸把文字框分組（與目前組的範圍重疊 ≥ 較小者 50% 視為同一組），
// 組依 across 中心排序（reverse 為由大到小），組內依 along 軸由小到大。
func groupBoxes(boxes []ocrBox, across, along func(ocrBox) span, reverse bool) [][]ocrBox {
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
				cur = cur.union(r)
				continue
			}
		}
		groups = append(groups, []ocrBox{b})
		cur = r
	}
	for _, g := range groups {
		sort.SliceStable(g, func(i, j int) bool { return along(g[i]).lo < along(g[j]).lo })
	}
	return groups
}

// splitColumns 把每個直書欄（欄內已由上到下排序）在上下兩框間距 > 較寬框寬度 × bubbleGapRatio 處拆開，
// 拆開的各段視為不同欄（之後可各自與相鄰欄組成對話框）。順序維持：欄由右至左，同欄各段由上到下。
func splitColumns(columns [][]ocrBox) [][]ocrBox {
	var out [][]ocrBox
	for _, g := range columns {
		start := 0
		for i := 1; i < len(g); i++ {
			gap := g[i].y.lo - g[i-1].y.hi
			if gap > bubbleGapRatio*max(g[i].x.size(), g[i-1].x.size()) {
				out = append(out, g[start:i])
				start = i
			}
		}
		out = append(out, g[start:])
	}
	return out
}

// joinTexts 以 sep 連接各框的文字。
func joinTexts(g []ocrBox, sep string) string {
	texts := make([]string, len(g))
	for i, b := range g {
		texts[i] = b.text
	}
	return strings.Join(texts, sep)
}

// joinColumns 把直書欄（已由右至左排序）依對話框分組：兩欄水平間距 ≤ 較寬欄寬度 × bubbleGapRatio
// 且垂直範圍有重疊時相連，相連的欄（可經由其他欄間接相連）屬於同一對話框。
// 每個對話框一行：欄由右至左直接連接；對話框依其最右欄的順序排列。
func joinColumns(columns [][]ocrBox) []string {
	type column struct {
		x, y span
		text string
	}
	cols := make([]column, len(columns))
	for i, g := range columns {
		c := column{x: g[0].x, y: g[0].y, text: joinTexts(g, " ")}
		for _, b := range g[1:] {
			c.x, c.y = c.x.union(b.x), c.y.union(b.y)
		}
		cols[i] = c
	}
	// union-find：parent[i] 為欄 i 所屬對話框的代表（取最右，也就是索引最小的欄）。
	parent := make([]int, len(cols))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	for i := range cols {
		for j := i + 1; j < len(cols); j++ {
			a, b := cols[i], cols[j]
			gap := max(a.x.lo, b.x.lo) - min(a.x.hi, b.x.hi)
			yOverlap := min(a.y.hi, b.y.hi) - max(a.y.lo, b.y.lo)
			if gap <= bubbleGapRatio*max(a.x.size(), b.x.size()) && yOverlap > 0 {
				ri, rj := find(i), find(j)
				parent[max(ri, rj)] = min(ri, rj)
			}
		}
	}
	var order []int
	texts := map[int]string{}
	for i, c := range cols {
		r := find(i)
		if _, ok := texts[r]; !ok {
			order = append(order, r)
		}
		texts[r] += c.text
	}
	lines := make([]string, len(order))
	for i, r := range order {
		lines[i] = texts[r]
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
