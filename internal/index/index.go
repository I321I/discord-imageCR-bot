// Package index 是圖片訊息與舊圖片訊息的記憶體索引（供 /search），以及啟動時讀頻道歷史建立索引與補處理。
// 索引只存文字與 ID，不存圖片與附件網址（貼圖前重新抓訊息取得新網址）。
package index

import (
	"errors"
	"fmt"
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
	AttachmentCount int             // 該訊息的附件數
}

// Index 是並行安全的記憶體索引。
type Index struct {
	mu       sync.RWMutex
	channels map[string]map[string][]Entry // channelID → messageID → 該訊息的項目（依附件順序）
}

func New() *Index {
	return &Index{channels: map[string]map[string][]Entry{}}
}

// set 以 entries 取代該訊息原有的項目（呼叫時須持有寫鎖）。
func (x *Index) set(channelID, messageID string, entries []Entry) {
	msgs := x.channels[channelID]
	if msgs == nil {
		msgs = map[string][]Entry{}
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
		GuildID: guildID, Kind: KindImageMessage, Fields: f, AttachmentCount: len(m.Attachments),
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.set(m.ChannelID, m.ID, []Entry{e})
}

// PutLegacy 收錄（或更新）一則舊圖片訊息：每張圖一筆，文字為原訊息整段文字；沒有文字則不收錄。
func (x *Index) PutLegacy(guildID string, m *discordgo.Message) {
	if m == nil {
		return
	}
	text := strings.TrimSpace(m.Content)
	if text == "" {
		return
	}
	var entries []Entry
	for i, a := range m.Attachments {
		if !recognize.IsImageAttachment(a) {
			continue
		}
		entries = append(entries, Entry{
			Key:     Key{ChannelID: m.ChannelID, MessageID: m.ID, Attachment: i},
			GuildID: guildID, Kind: KindLegacy, LegacyText: text, AttachmentCount: len(m.Attachments),
		})
	}
	if len(entries) == 0 {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.set(m.ChannelID, m.ID, entries)
}

// UpdateLegacyText 只更新已收錄的舊圖片訊息的文字（保留附件項目）；沒有收錄或不是舊圖片訊息時不動。
func (x *Index) UpdateLegacyText(channelID, messageID, text string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for i, e := range x.channels[channelID][messageID] {
		if e.Kind == KindLegacy {
			x.channels[channelID][messageID][i].LegacyText = text
		}
	}
}

// RemoveMessage 移除一則訊息的所有項目。
func (x *Index) RemoveMessage(channelID, messageID string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if msgs := x.channels[channelID]; msgs != nil {
		delete(msgs, messageID)
		if len(msgs) == 0 {
			delete(x.channels, channelID)
		}
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
	for _, e := range x.channels[k.ChannelID][k.MessageID] {
		if e.Key == k {
			return e, true
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

// Search 回傳 allowed 頻道中符合 query 的項目，最多 limit 筆（limit ≤ 0 不限）。
// 比對（Q36）：不分大小寫、全形半形視為相同、以空白分隔的多個詞全部符合（各詞可在不同欄位）；
// 上傳者與失敗標記不參與比對。
// 排序（Q37）：有詞符合作品／角色／關鍵字 > 只符合圖片文字 > 舊訊息文字；同級依訊息由新到舊。
// 空查詢（Q33）：全部項目由新到舊。
func (x *Index) Search(query string, allowed map[string]bool, limit int) []Entry {
	terms := strings.Fields(normalize(query))
	type hit struct {
		e    Entry
		rank int
	}
	var hits []hit
	x.mu.RLock()
	for ch, msgs := range x.channels {
		if !allowed[ch] {
			continue
		}
		for _, entries := range msgs {
			for _, e := range entries {
				if r := match(e, terms); r != rankNone {
					hits = append(hits, hit{e, r})
				}
			}
		}
	}
	x.mu.RUnlock()
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
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
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Entry, len(hits))
	for i, h := range hits {
		out[i] = h.e
	}
	return out
}

// match 回傳項目對 terms 的符合等級；有任一詞不符合時回 rankNone。terms 為空時一律符合（等級相同）。
func match(e Entry, terms []string) int {
	type field struct {
		text string
		rank int
	}
	var fields []field
	if e.Kind == KindLegacy {
		fields = []field{{normalize(e.LegacyText), rankLegacy}}
	} else {
		for _, f := range []struct {
			v    string
			rank int
		}{{e.Fields.Work, rankMain}, {e.Fields.Characters, rankMain}, {e.Fields.Keywords, rankMain}, {e.Fields.Text, rankText}} {
			if f.v != "" && f.v != imagemsg.FailedMarker {
				fields = append(fields, field{normalize(f.v), f.rank})
			}
		}
	}
	if len(terms) == 0 {
		return rankMain
	}
	best := rankNone
	for _, t := range terms {
		found := false
		for _, f := range fields {
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
