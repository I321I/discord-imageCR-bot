// Package index 是圖片訊息與舊圖片訊息的記憶體索引（供 /search），以及啟動時讀頻道歷史建立索引與補處理。
// 索引只存文字與 ID，不存圖片與附件網址（貼圖前重新抓訊息取得新網址）。
package index

import (
	"container/heap"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"discordbot/internal/imagemsg"
	"discordbot/internal/recognize"
	"discordbot/internal/snowflake"

	"github.com/bwmarrin/discordgo"
)

// Kind 是索引項目的種類。
type Kind int

const (
	KindImageMessage Kind = iota // bot 發出的圖片訊息（四個欄位）
	KindLegacy                   // 設定為來源頻道之前、使用者發的帶圖訊息（整段文字）
)

// maxKeyLen 是 Key 字串的長度上限（autocomplete choice value 上限 100 字）。
const maxKeyLen = 100

// Key 指向一則訊息中的一張圖。圖片訊息的 Attachment 一律為 0。
type Key struct {
	ChannelID, MessageID string
	Attachment           int
}

// String 回傳 "channelID:messageID:attachment"。
func (k Key) String() string {
	return fmt.Sprintf("%s:%s:%d", k.ChannelID, k.MessageID, k.Attachment)
}

// ErrBadKey 表示字串不是合法的 Key。
var ErrBadKey = errors.New("invalid index key")

// ParseKey 解析 Key.String 的結果；頻道與訊息 ID 必須是數字、附件序號 ≥ 0、總長 ≤ 100。
func ParseKey(s string) (Key, error) {
	if len(s) > maxKeyLen {
		return Key{}, ErrBadKey
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return Key{}, ErrBadKey
	}
	for _, id := range parts[:2] {
		if _, err := strconv.ParseUint(id, 10, 64); err != nil {
			return Key{}, ErrBadKey
		}
	}
	n, err := strconv.Atoi(parts[2])
	if err != nil || n < 0 {
		return Key{}, ErrBadKey
	}
	return Key{ChannelID: parts[0], MessageID: parts[1], Attachment: n}, nil
}

// Entry 是索引中的一張圖。
type Entry struct {
	Key             Key
	GuildID         string
	Kind            Kind
	Fields          imagemsg.Fields // KindImageMessage
	LegacyText      string          // KindLegacy：原訊息整段文字
	AttachmentCount int             // 該訊息的圖片附件數
	Number          int             // 這張圖是該訊息的第幾張圖片（1 起，只計圖片附件）
	AttachmentID    string          // 收錄時的附件 ID（貼圖前比對，確認附件沒被換掉）
}

// Index 是並行安全的記憶體索引。
type Index struct {
	mu       sync.RWMutex
	channels map[string]map[string][]stored // channelID → messageID → 該訊息的項目（依附件順序）
}

// stored 是索引中的一筆項目，連同寫入時正規化好的比對文字（搜尋時不再逐筆正規化）。
type stored struct {
	e      Entry
	fields []normField
}

// normField 是一個參與比對的欄位（已正規化）與它的符合等級。
type normField struct {
	text string
	rank int
}

// newStored 計算 e 的比對文字：舊圖片訊息為整段文字；圖片訊息為作品、角色、關鍵字、圖片文字（空欄與失敗標記不比對）。
func newStored(e Entry) stored {
	var fields []normField
	if e.Kind == KindLegacy {
		fields = []normField{{normalize(e.LegacyText), rankLegacy}}
	} else {
		for _, f := range []normField{{e.Fields.Work, rankMain}, {e.Fields.Characters, rankMain}, {e.Fields.Keywords, rankMain}, {e.Fields.Text, rankText}} {
			if f.text != "" && f.text != imagemsg.FailedMarker {
				fields = append(fields, normField{normalize(f.text), f.rank})
			}
		}
	}
	return stored{e: e, fields: fields}
}

func New() *Index {
	return &Index{channels: map[string]map[string][]stored{}}
}

// set 以 entries 取代該訊息原有的項目（呼叫時須持有寫鎖）。
func (x *Index) set(channelID, messageID string, entries []stored) {
	msgs := x.channels[channelID]
	if msgs == nil {
		msgs = map[string][]stored{}
		x.channels[channelID] = msgs
	}
	msgs[messageID] = entries
}

// PutImageMessage 收錄（或更新）一則 bot 發出的圖片訊息；內容解析失敗則忽略。
func (x *Index) PutImageMessage(guildID string, m *discordgo.Message) {
	if m == nil {
		return
	}
	f, _, ok := imagemsg.Parse(m.Content)
	if !ok {
		return
	}
	e := Entry{
		Key:     Key{ChannelID: m.ChannelID, MessageID: m.ID},
		GuildID: guildID, Kind: KindImageMessage, Fields: f, AttachmentCount: len(m.Attachments), Number: 1,
	}
	if len(m.Attachments) > 0 && m.Attachments[0] != nil {
		e.AttachmentID = m.Attachments[0].ID
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.set(m.ChannelID, m.ID, []stored{newStored(e)})
}

// PutLegacy 收錄（或更新）一則舊圖片訊息：每張圖一筆，文字為原訊息整段文字；沒有文字則不收錄。
// 張數與編號只計圖片附件；Key.Attachment 是附件在訊息中的位置（含非圖片附件）。
func (x *Index) PutLegacy(guildID string, m *discordgo.Message) {
	if m == nil {
		return
	}
	text := strings.TrimSpace(m.Content)
	if text == "" {
		return
	}
	var entries []stored
	for i, a := range m.Attachments {
		if !recognize.IsImageAttachment(a) {
			continue
		}
		entries = append(entries, newStored(Entry{
			Key:     Key{ChannelID: m.ChannelID, MessageID: m.ID, Attachment: i},
			GuildID: guildID, Kind: KindLegacy, LegacyText: text, Number: len(entries) + 1, AttachmentID: a.ID,
		}))
	}
	if len(entries) == 0 {
		return
	}
	for i := range entries {
		entries[i].e.AttachmentCount = len(entries)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.set(m.ChannelID, m.ID, entries)
}

// UpdateLegacyText 只更新已收錄的舊圖片訊息的文字（保留附件項目）；沒有收錄或不是舊圖片訊息時不動。
func (x *Index) UpdateLegacyText(channelID, messageID, text string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	entries := x.channels[channelID][messageID]
	for i, s := range entries {
		if s.e.Kind == KindLegacy {
			s.e.LegacyText = text
			entries[i] = newStored(s.e)
		}
	}
}

// RemoveMessage 移除一則訊息的所有項目。
func (x *Index) RemoveMessage(channelID, messageID string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.removeMessage(channelID, messageID)
}

// removeMessage 呼叫時須持有寫鎖。
func (x *Index) removeMessage(channelID, messageID string) {
	if msgs := x.channels[channelID]; msgs != nil {
		delete(msgs, messageID)
		if len(msgs) == 0 {
			delete(x.channels, channelID)
		}
	}
}

// RemoveAttachment 只移除 k 指向的一張圖；該訊息沒有其他項目時整則移除。
func (x *Index) RemoveAttachment(k Key) {
	x.mu.Lock()
	defer x.mu.Unlock()
	entries := x.channels[k.ChannelID][k.MessageID]
	kept := slices.DeleteFunc(slices.Clone(entries), func(s stored) bool { return s.e.Key == k })
	switch {
	case len(kept) == len(entries):
	case len(kept) == 0:
		x.removeMessage(k.ChannelID, k.MessageID)
	default:
		x.channels[k.ChannelID][k.MessageID] = kept
	}
}

// RemoveChannel 移除一個頻道的所有項目。
func (x *Index) RemoveChannel(channelID string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.channels, channelID)
}

// Get 回傳 k 指向的項目。不檢查權限：k 可能來自 client 偽造的值，
// 呼叫端必須自行確認 Entry 的 GuildID／Key.ChannelID 在使用者可用的範圍內。
func (x *Index) Get(k Key) (Entry, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	for _, s := range x.channels[k.ChannelID][k.MessageID] {
		if s.e.Key == k {
			return s.e, true
		}
	}
	return Entry{}, false
}

// 符合等級（Q37）：數字越小越前面。
const (
	rankMain   = iota // 作品、角色、關鍵字
	rankText          // 圖片文字
	rankLegacy        // 舊訊息文字
	rankNone
)

// hit 是一筆符合的項目與它的等級。
type hit struct {
	e    *Entry
	rank int
}

// before 回傳 a 是否排在 b 前面（Q37）：等級 → 訊息由新到舊 → 頻道 ID → 附件序號。
func (a hit) before(b hit) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	if c := snowflake.Compare(a.e.Key.MessageID, b.e.Key.MessageID); c != 0 {
		return c > 0
	}
	if a.e.Key.ChannelID != b.e.Key.ChannelID {
		return a.e.Key.ChannelID < b.e.Key.ChannelID
	}
	return a.e.Key.Attachment < b.e.Key.Attachment
}

// worstFirst 是以「排最後的」為頂端的 heap，用來只保留前 N 名。
type worstFirst []hit

func (h worstFirst) Len() int           { return len(h) }
func (h worstFirst) Less(i, j int) bool { return h[j].before(h[i]) }
func (h worstFirst) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *worstFirst) Push(v any)        { *h = append(*h, v.(hit)) }
func (h *worstFirst) Pop() any {
	old := *h
	v := old[len(old)-1]
	*h = old[:len(old)-1]
	return v
}

// Search 回傳 allowed 頻道中符合 query 的項目，最多 limit 筆（limit ≤ 0 不限）。
// 比對（Q36）：不分大小寫、全形半形視為相同、以空白分隔的多個詞全部符合（各詞可在不同欄位）；
// 上傳者與失敗標記不參與比對。
// 排序（Q37）：有詞符合作品／角色／關鍵字 > 只符合圖片文字 > 舊訊息文字；同級依訊息由新到舊。
// 空查詢（Q33）：全部項目由新到舊。
// limit > 0 時只保留前 limit 名（heap），不為全部符合的項目排序。
func (x *Index) Search(query string, allowed map[string]bool, limit int) []Entry {
	terms := strings.Fields(normalize(query))
	var hits worstFirst
	if limit > 0 {
		hits = make(worstFirst, 0, limit)
	}
	x.mu.RLock()
	for ch, msgs := range x.channels {
		if !allowed[ch] {
			continue
		}
		for _, entries := range msgs {
			for i := range entries {
				r := entries[i].match(terms)
				if r == rankNone {
					continue
				}
				h := hit{&entries[i].e, r}
				switch {
				case limit <= 0 || len(hits) < limit:
					hits = append(hits, h)
					if limit > 0 && len(hits) == limit {
						heap.Init(&hits)
					}
				case h.before(hits[0]):
					hits[0] = h
					heap.Fix(&hits, 0)
				}
			}
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].before(hits[j]) })
	out := make([]Entry, len(hits))
	for i, h := range hits {
		out[i] = *h.e
	}
	x.mu.RUnlock()
	return out
}

// match 回傳項目對 terms 的符合等級；有任一詞不符合時回 rankNone。terms 為空時一律符合（等級相同）。
func (s *stored) match(terms []string) int {
	if len(terms) == 0 {
		return rankMain
	}
	best := rankNone
	for _, t := range terms {
		found := false
		for _, f := range s.fields {
			if strings.Contains(f.text, t) {
				found = true
				best = min(best, f.rank)
			}
		}
		if !found {
			return rankNone
		}
	}
	return best
}

// normalize 把全形英數符號與全形空白轉成半形，再轉小寫。
func normalize(s string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			return r - 0xFEE0
		case r == 0x3000:
			return ' '
		}
		return r
	}, s))
}
