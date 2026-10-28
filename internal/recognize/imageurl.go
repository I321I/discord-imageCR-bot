package recognize

import (
	"net/url"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// Link 是訊息文字中的一個 http/https 連結。
type Link struct {
	URL        string // 網址本身（去掉包裝）
	Start, End int    // 在 content 中要被移除的位元組範圍（含 <>、||、[文字](...) 等包裝，U7）
	Keep       string // 移除後要留下的文字（遮罩連結 [文字](網址) 留「文字」，其他為空）
	Spoiler    bool   // 在 || || 劇透中（U7）
	Suppressed bool   // <網址>：Discord 不會為它產生預覽
}

// Links 回傳 content 中的連結（依出現順序；同一網址只留第一個，U6）。
// 抽取比照 Discord 的自動連結：http:// 或 https:// 之後到空白或「<」為止，去掉結尾的標點。
func Links(content string) []Link {
	var out []Link
	seen := map[string]bool{}
	for _, l := range allLinks(content) {
		if seen[l.URL] {
			continue
		}
		seen[l.URL] = true
		out = append(out, l)
	}
	return out
}

// allLinks 回傳 content 中所有連結（不去重，依出現順序，範圍不重疊）。
func allLinks(content string) []Link {
	var out []Link
	prevEnd := 0
	i := 0
	for i < len(content) {
		start := indexScheme(content, i)
		if start < 0 {
			break
		}
		l, next, ok := linkAt(content, start, prevEnd)
		if !ok {
			i = next
			continue
		}
		out = append(out, l)
		prevEnd = l.End
		i = max(next, l.End)
	}
	return out
}

// indexScheme 回傳 content[from:] 中下一個 "http://" 或 "https://"（不分大小寫）的位置；沒有時回 -1。
func indexScheme(content string, from int) int {
	for i := from; i+7 <= len(content); i++ {
		if c := content[i]; c != 'h' && c != 'H' {
			continue
		}
		rest := content[i:]
		if hasPrefixFold(rest, "https://") || hasPrefixFold(rest, "http://") {
			return i
		}
	}
	return -1
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// linkAt 解析從 start 開始的連結；回傳下一次搜尋的位置。prevEnd 是前一個連結的結尾（包裝不可與它重疊）。
func linkAt(content string, start, prevEnd int) (Link, int, bool) {
	schemeEnd := strings.Index(content[start:], "://") + start + 3
	var l Link
	switch {
	case start >= 1 && content[start-1] == '<' && start-1 >= prevEnd:
		// <網址>：到「>」為止（中間不可有空白）。
		k := strings.IndexByte(content[start:], '>')
		if k > 0 && !strings.ContainsFunc(content[start:start+k], unicode.IsSpace) {
			l = Link{URL: content[start : start+k], Start: start - 1, End: start + k + 1, Suppressed: true}
		}
	case start >= 2 && content[start-2:start] == "](":
		// [文字](網址)：到對應的「)」為止。
		lb := strings.LastIndexByte(content[:start-2], '[')
		if lb >= prevEnd && lb >= 0 && !strings.ContainsAny(content[lb+1:start-2], "]\n") {
			if k, ok := maskedEnd(content, start); ok {
				l = Link{URL: content[start:k], Start: lb, End: k + 1, Keep: content[lb+1 : start-2]}
			}
		}
	}
	if l.URL == "" {
		end := start
		for end < len(content) {
			r, size := utf8.DecodeRuneInString(content[end:])
			if unicode.IsSpace(r) || r == '<' {
				break
			}
			end += size
		}
		end = trimTrailing(content, start, end)
		l = Link{URL: content[start:end], Start: start, End: end}
	}
	if len(l.URL) <= schemeEnd-start {
		return Link{}, schemeEnd, false // 只有 "https://"
	}
	next := l.End
	l.Spoiler = strings.Count(content[:l.Start], "||")%2 == 1
	if l.Spoiler && l.Keep == "" && l.Start-2 >= prevEnd && strings.HasPrefix(content[l.Start-2:], "||") && strings.HasPrefix(content[l.End:], "||") {
		l.Start -= 2
		l.End += 2
	}
	return l, next, true
}

// maskedEnd 回傳遮罩連結中網址結尾「)」的位置（括號需成對；遇到空白則不是遮罩連結）。
func maskedEnd(content string, start int) (int, bool) {
	depth := 0
	for k := start; k < len(content); {
		r, size := utf8.DecodeRuneInString(content[k:])
		switch {
		case unicode.IsSpace(r):
			return 0, false
		case r == '(':
			depth++
		case r == ')':
			if depth == 0 {
				return k, k > start
			}
			depth--
		}
		k += size
	}
	return 0, false
}

// trimTrailing 去掉連結結尾的標點：.,:;"'!?|]*~ 、不成對的「)」與全形標點。
func trimTrailing(content string, start, end int) int {
	for end > start {
		r, size := utf8.DecodeLastRuneInString(content[start:end])
		switch {
		case strings.ContainsRune(".,:;\"'!?|]*~", r):
		case r == ')':
			s := content[start:end]
			if strings.Count(s, "(") >= strings.Count(s, ")") {
				return end
			}
		case r >= utf8.RuneSelf && unicode.IsPunct(r):
		default:
			return end
		}
		end -= size
	}
	return end
}

// ImageSource 是訊息中的一張圖，依編號順序：附件在前（依附件順序）、網址在後（依文字順序，U8）。
type ImageSource struct {
	Attachment *discordgo.MessageAttachment // 附件圖；網址圖為 nil
	Index      int                          // 附件圖：在 m.Attachments 的位置；網址圖：在 Links(m.Content) 的位置
	URL        string                       // 網址圖：訊息中的網址（/search 原樣貼出）
	FetchURL   string                       // 網址圖：要下載的網址（預覽的圖片在別的網址時為該網址，U4）
	Spoiler    bool                         // 劇透（附件檔名以 SPOILER_ 開頭，或網址在 || || 中）
	GIFV       bool                         // 網址圖的預覽為 gifv（動圖網頁或影片；新訊息不處理，U3）
}

// IsLink 回傳是否為網址圖。
func (s ImageSource) IsLink() bool { return s.Attachment == nil }

// ImageSources 回傳訊息中所有圖片（附件圖與圖片網址）；訊息含非圖片附件時 ok=false（整則不處理）。
// 圖片網址：網址路徑副檔名為 .png/.jpg/.jpeg/.webp/.gif（不分大小寫），或 Discord 對它的預覽為圖片
// （type image／gifv 且 URL 相同，或預覽的 image／thumbnail 就是該網址）。
func ImageSources(m *discordgo.Message) (srcs []ImageSource, ok bool) {
	if m == nil {
		return nil, false
	}
	for i, a := range m.Attachments {
		if !IsImageAttachment(a) {
			return nil, false
		}
		srcs = append(srcs, ImageSource{Attachment: a, Index: i, Spoiler: strings.HasPrefix(a.Filename, "SPOILER_")})
	}
	return append(srcs, LinkSources(m)...), true
}

// NewMessageSources 同 ImageSources，但不含 gifv（新訊息的規則，U3）。
func NewMessageSources(m *discordgo.Message) ([]ImageSource, bool) {
	srcs, ok := ImageSources(m)
	if !ok {
		return nil, false
	}
	out := srcs[:0:0]
	for _, s := range srcs {
		if !s.GIFV {
			out = append(out, s)
		}
	}
	return out, true
}

// linkSources 回傳訊息文字中的圖片網址（不看附件）。
func LinkSources(m *discordgo.Message) []ImageSource {
	var out []ImageSource
	for i, l := range Links(m.Content) {
		if ok, fetch, gifv := classifyLink(l.URL, m.Embeds); ok {
			out = append(out, ImageSource{Index: i, URL: l.URL, FetchURL: fetch, Spoiler: l.Spoiler, GIFV: gifv})
		}
	}
	return out
}

// classifyLink 判斷 raw 是否為圖片網址，並回傳要下載的網址與是否為 gifv。
func classifyLink(raw string, embeds []*discordgo.MessageEmbed) (ok bool, fetch string, gifv bool) {
	if !validWebURL(raw) {
		return false, "", false
	}
	if u, _ := url.Parse(raw); imageExtensions[strings.ToLower(path.Ext(u.Path))] {
		return true, raw, false
	}
	for _, e := range embeds {
		if e == nil {
			continue
		}
		if (e.Image != nil && sameURL(e.Image.URL, raw)) || (e.Thumbnail != nil && sameURL(e.Thumbnail.URL, raw)) {
			return true, raw, e.Type == discordgo.EmbedTypeGifv && sameURL(e.URL, raw)
		}
	}
	for _, e := range embeds {
		if e == nil || !sameURL(e.URL, raw) || (e.Type != discordgo.EmbedTypeImage && e.Type != discordgo.EmbedTypeGifv) {
			continue
		}
		fetch = raw
		switch {
		case e.Image != nil && e.Image.URL != "":
			fetch = e.Image.URL
		case e.Thumbnail != nil && e.Thumbnail.URL != "":
			fetch = e.Thumbnail.URL
		}
		return true, fetch, e.Type == discordgo.EmbedTypeGifv
	}
	return false, "", false
}

// validWebURL 回傳 raw 是否為有 host 的 http/https 網址。
func validWebURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// sameURL 回傳 a、b 是否為同一網址：字串相同，或解析後（host 不分大小寫、百分比編碼正規化）相同。
func sameURL(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	ua.Host = strings.ToLower(ua.Host)
	ub.Host = strings.ToLower(ub.Host)
	return ua.String() == ub.String()
}

// HasImagePreview 回傳 Discord 是否對 raw 產生了圖片預覽：type image／gifv 的預覽，其 URL、image 或 thumbnail
// 就是 raw（皆以 sameURL 比對）。只靠副檔名判定的圖片網址不算；其他類型（article／link 的 og:image 由網頁指定、
// rich 由 bot／webhook 自訂）不算。
func HasImagePreview(raw string, embeds []*discordgo.MessageEmbed) bool {
	for _, e := range embeds {
		if e == nil || e.Type == discordgo.EmbedTypeRich {
			continue
		}
		if e.Type != discordgo.EmbedTypeImage && e.Type != discordgo.EmbedTypeGifv {
			continue // article／link 的 og:image 是網頁自己指定的，rich 是 bot 自訂的，都不算
		}
		if sameURL(e.URL, raw) || (e.Image != nil && sameURL(e.Image.URL, raw)) || (e.Thumbnail != nil && sameURL(e.Thumbnail.URL, raw)) {
			return true
		}
	}
	return false
}

// PreviewImageURL 回傳 Discord 對 raw 的 type image 預覽（URL、image 或 thumbnail 與 raw 相同，皆以 sameURL 比對）
// 中的圖片網址：image 有值時用 image，否則 thumbnail；該網址與 raw 相同（重抓同一網頁沒有意義）時改用對應的
// ProxyURL（Discord 的 media.discordapp.net 代理，PM 決定）。ProxyURL 也沒有或也與 raw 相同時找後面符合的預覽，
// 都沒有回 ""。gifv 不算（動圖網頁或影片，Q4）。用於看圖網頁網址改抓預覽圖（10/28）。
func PreviewImageURL(raw string, embeds []*discordgo.MessageEmbed) string {
	for _, e := range embeds {
		if e == nil || e.Type != discordgo.EmbedTypeImage {
			continue
		}
		if !sameURL(e.URL, raw) && (e.Image == nil || !sameURL(e.Image.URL, raw)) && (e.Thumbnail == nil || !sameURL(e.Thumbnail.URL, raw)) {
			continue
		}
		var cand, proxy string
		switch {
		case e.Image != nil && e.Image.URL != "":
			cand, proxy = e.Image.URL, e.Image.ProxyURL
		case e.Thumbnail != nil && e.Thumbnail.URL != "":
			cand, proxy = e.Thumbnail.URL, e.Thumbnail.ProxyURL
		}
		if cand != "" && sameURL(cand, raw) {
			cand = proxy
		}
		if cand != "" && !sameURL(cand, raw) {
			return cand
		}
	}
	return ""
}

// embedFor 回傳是否有任何預覽屬於 raw（URL、image 或 thumbnail 相同）。
func embedFor(raw string, embeds []*discordgo.MessageEmbed) bool {
	for _, e := range embeds {
		if e == nil {
			continue
		}
		if sameURL(e.URL, raw) || (e.Image != nil && sameURL(e.Image.URL, raw)) || (e.Thumbnail != nil && sameURL(e.Thumbnail.URL, raw)) {
			return true
		}
	}
	return false
}

// UnresolvedLinks 回傳訊息中是否有「尚無法判定」的連結：不是副檔名圖片、也沒有任何預覽與它相符
// （Discord 可能還沒產生預覽）。<網址>、訊息關閉預覽、含非圖片附件（整則不處理）時不算。
func UnresolvedLinks(m *discordgo.Message) bool {
	if m == nil || m.Flags&discordgo.MessageFlagsSuppressEmbeds != 0 {
		return false
	}
	if _, ok := ImageSources(m); !ok {
		return false
	}
	for _, l := range Links(m.Content) {
		if l.Suppressed || !validWebURL(l.URL) {
			continue
		}
		if u, _ := url.Parse(l.URL); imageExtensions[strings.ToLower(path.Ext(u.Path))] {
			continue
		}
		if !embedFor(l.URL, m.Embeds) {
			return true
		}
	}
	return false
}

// TextWithoutImageURLs 回傳訊息文字移除所有圖片網址（含包裝；遮罩連結留下文字）後的結果（去除前後空白）；
// 一般網頁連結保留。
func TextWithoutImageURLs(m *discordgo.Message) string {
	if m == nil {
		return ""
	}
	return TextWithoutLinks(m.Content, LinkSources(m))
}

// TextWithoutLinks 回傳 content 移除 srcs 中網址圖的所有出現（含包裝；遮罩連結留下文字）後的結果（去除前後空白）。
func TextWithoutLinks(content string, srcs []ImageSource) string {
	remove := map[string]bool{}
	for _, s := range srcs {
		if s.IsLink() {
			remove[s.URL] = true
		}
	}
	var b strings.Builder
	last := 0
	for _, l := range allLinks(content) {
		if !remove[l.URL] || l.Start < last {
			continue
		}
		b.WriteString(content[last:l.Start])
		b.WriteString(l.Keep)
		last = l.End
		// 網址兩側都是空格時只留一個（換行保留）。
		if l.Keep == "" && strings.HasSuffix(b.String(), " ") {
			for last < len(content) && (content[last] == ' ' || content[last] == '\t') {
				last++
			}
		}
	}
	b.WriteString(content[last:])
	return strings.TrimSpace(b.String())
}
