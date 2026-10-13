// Package imagemsg 處理圖片訊息的文字格式：格式化、解析、分隔符號正規化。
package imagemsg

import (
	"slices"
	"strings"
	"unicode/utf8"
)

const FailedMarker = "⚠️ 辨識失敗"

// MaxLen 是 Discord 訊息內容上限（字數）。
const MaxLen = 2000

type Field int

const (
	FieldWork Field = iota
	FieldText
	FieldCharacters
	FieldKeywords
)

// allFields 是固定的欄位順序。
var allFields = []Field{FieldWork, FieldText, FieldCharacters, FieldKeywords}

// Fields 是圖片訊息的四個欄位。Text 可多行，其他欄位必須是單行。
type Fields struct {
	Work, Text, Characters, Keywords string
}

const uploaderLabel = "上傳者"

func Label(f Field) string {
	switch f {
	case FieldWork:
		return "作品"
	case FieldText:
		return "圖片文字"
	case FieldCharacters:
		return "角色"
	case FieldKeywords:
		return "關鍵字"
	}
	return ""
}

func prefix(label string) string { return label + "：" }

func (f Fields) Get(field Field) string {
	switch field {
	case FieldWork:
		return f.Work
	case FieldText:
		return f.Text
	case FieldCharacters:
		return f.Characters
	case FieldKeywords:
		return f.Keywords
	}
	return ""
}

func (f *Fields) Set(field Field, v string) {
	switch field {
	case FieldWork:
		f.Work = v
	case FieldText:
		f.Text = v
	case FieldCharacters:
		f.Characters = v
	case FieldKeywords:
		f.Keywords = v
	}
}

// Failed 回傳值等於 FailedMarker 的欄位（依固定順序）。
func (f Fields) Failed() []Field {
	var out []Field
	for _, field := range allFields {
		if f.Get(field) == FailedMarker {
			out = append(out, field)
		}
	}
	return out
}

// Format 產生圖片訊息內容：非空欄位依固定順序在前、空欄位在後，最後一行為上傳者。
// 單行欄位（作品、角色、關鍵字）含換行時，以換行拆開、修剪前後空白、去除空段與重複後以「、」連接
// （與 NormalizeList 一致，但只以換行分隔）。
// 超過 2000 字時平均截：先把最長的欄位截到與次長一樣長，不夠再一起截，直到不超過上限；
// 被截的欄位截完等長（含結尾的「…」），最短只剩「…」。
func Format(f Fields, uploaderID string) string {
	for _, field := range []Field{FieldWork, FieldCharacters, FieldKeywords} {
		f.Set(field, joinLines(f.Get(field)))
	}
	out := format(f, uploaderID)
	excess := utf8.RuneCountInString(out) - MaxLen
	if excess <= 0 {
		return out
	}
	limit := truncateLimit(f, excess)
	for _, field := range allFields {
		if utf8.RuneCountInString(f.Get(field)) > limit {
			f.Set(field, truncate(f.Get(field), limit))
		}
	}
	return format(f, uploaderID)
}

// truncateLimit 回傳欄位長度上限 L：把所有超過 L 的欄位截成 L 字後，總共至少少 excess 字。
// 取滿足條件的最大 L（至少 1）。
func truncateLimit(f Fields, excess int) int {
	lens := make([]int, 0, len(allFields))
	for _, field := range allFields {
		lens = append(lens, utf8.RuneCountInString(f.Get(field)))
	}
	saved := func(limit int) int {
		n := 0
		for _, l := range lens {
			if l > limit {
				n += l - limit
			}
		}
		return n
	}
	limit := slices.Max(lens)
	for limit > 1 && saved(limit) < excess {
		limit--
	}
	return limit
}

// joinLines 以換行拆開，修剪前後空白、去除空段與重複後以「、」連接；沒有換行時原樣回傳。
func joinLines(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		part = strings.TrimSpace(part)
		if part == "" || slices.Contains(out, part) {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, "、")
}

// truncate 把 s 截成 limit 字（含結尾的「…」，limit 至少為 1）。
func truncate(s string, limit int) string {
	r := []rune(s)
	return string(r[:limit-1]) + "…"
}

func format(f Fields, uploaderID string) string {
	var filled, empty []string
	for _, field := range allFields {
		line := prefix(Label(field)) + f.Get(field)
		if f.Get(field) != "" {
			filled = append(filled, line)
		} else {
			empty = append(empty, line)
		}
	}
	lines := append(filled, empty...)
	lines = append(lines, prefix(uploaderLabel)+"<@"+uploaderID+">")
	return strings.Join(lines, "\n")
}

// Parse 解析 Format 產生的內容；不是圖片訊息時 ok=false。
//
// 解析方式：最後一行必須是上傳者；「圖片文字：」取第一次出現在行首的那一行
// （它之前只會有單行欄位）。它之前的行必須都是其他欄位的標籤行，其餘欄位則是
// 上傳者之前的最後幾行；中間全部屬於圖片文字。所以圖片文字中出現像標籤的行也不會切錯。
// 最後要求內容與 Format 的輸出完全一致（欄位順序、上傳者寫法）。
func Parse(content string) (f Fields, uploaderID string, ok bool) {
	lines := strings.Split(content, "\n")
	if len(lines) < len(allFields)+1 {
		return Fields{}, "", false
	}
	uploaderID, ok = parseUploader(lines[len(lines)-1])
	if !ok {
		return Fields{}, "", false
	}
	body := lines[:len(lines)-1]

	textIdx := -1
	for i, l := range body {
		if strings.HasPrefix(l, prefix(Label(FieldText))) {
			textIdx = i
			break
		}
	}
	if textIdx < 0 {
		return Fields{}, "", false
	}

	seen := map[Field]bool{}
	takeSingle := func(line string) bool {
		for _, field := range []Field{FieldWork, FieldCharacters, FieldKeywords} {
			p := prefix(Label(field))
			if strings.HasPrefix(line, p) {
				if seen[field] {
					return false
				}
				seen[field] = true
				f.Set(field, strings.TrimPrefix(line, p))
				return true
			}
		}
		return false
	}

	for _, l := range body[:textIdx] {
		if !takeSingle(l) {
			return Fields{}, "", false
		}
	}
	k := len(allFields) - 1 - textIdx // 圖片文字之後的單行欄位數
	textEnd := len(body) - k
	if textEnd <= textIdx {
		return Fields{}, "", false
	}
	for _, l := range body[textEnd:] {
		if !takeSingle(l) {
			return Fields{}, "", false
		}
	}
	text := strings.Join(body[textIdx:textEnd], "\n")
	f.Text = strings.TrimPrefix(text, prefix(Label(FieldText)))
	// 欄位順序必須與 Format 產生的一致（非空在前、空在後），否則不是圖片訊息。
	if format(f, uploaderID) != content {
		return Fields{}, "", false
	}
	return f, uploaderID, true
}

func parseUploader(line string) (string, bool) {
	v, ok := strings.CutPrefix(line, prefix(uploaderLabel))
	if !ok {
		return "", false
	}
	v, ok = strings.CutPrefix(v, "<@")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(v, ">")
	if !ok || id == "" || strings.ContainsAny(id, "<>@ ") {
		return "", false
	}
	return id, true
}

// isSeparator 回傳是否為多值欄位的分隔符號：，,、；; 與換行。
func isSeparator(r rune) bool {
	switch r {
	case '，', ',', '、', '；', ';', '\n', '\r':
		return true
	}
	return false
}

// NormalizeList 以分隔符號拆開，去除前後空白、空值與重複值後，以「、」連接。
func NormalizeList(s string) string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(s, isSeparator) {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return strings.Join(out, "、")
}
