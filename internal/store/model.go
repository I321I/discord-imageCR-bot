// Package store 管理存在 Discord 儲存頻道中的設定資料。
package store

import (
	"slices"
	"strconv"
)

const FormatVersion = 1

// Data 是儲存頻道中的完整設定。
type Data struct {
	FormatVersion int                          `json:"format_version"`
	Guilds        map[string]map[string]string `json:"guilds"` // guildID → channelID → 設定時間點 snowflake
	// UserSelections：userID → channelIDs；key 不存在 = 沒選過（預設全選）。
	// （欄位名稱與方法 UserChannels 不能相同，所以叫 UserSelections。）
	UserSelections map[string][]string `json:"user_channels"`
}

func NewData() *Data {
	return &Data{
		FormatVersion:  FormatVersion,
		Guilds:         map[string]map[string]string{},
		UserSelections: map[string][]string{},
	}
}

// AddChannel 把頻道設為來源；已存在時回 false 且不改 setAt。
func (d *Data) AddChannel(guildID, channelID, setAt string) (added bool) {
	if _, _, ok := d.SourceChannel(channelID); ok {
		return false
	}
	if d.Guilds[guildID] == nil {
		d.Guilds[guildID] = map[string]string{}
	}
	d.Guilds[guildID][channelID] = setAt
	return true
}

// RemoveChannel 從指定伺服器移除來源頻道，同時從所有使用者勾選中移除。
// 勾選因此變成 0 個的使用者視為沒選過（刪除 key）；使用者自己選 0 個的不受影響。
func (d *Data) RemoveChannel(guildID, channelID string) (removed bool) {
	chs, ok := d.Guilds[guildID]
	if !ok {
		return false
	}
	if _, ok := chs[channelID]; !ok {
		return false
	}
	delete(chs, channelID)
	if len(chs) == 0 {
		delete(d.Guilds, guildID)
	}
	for u, ids := range d.UserSelections {
		if !slices.Contains(ids, channelID) {
			continue
		}
		ids = slices.DeleteFunc(ids, func(id string) bool { return id == channelID })
		if len(ids) == 0 {
			delete(d.UserSelections, u)
		} else {
			d.UserSelections[u] = ids
		}
	}
	return true
}

// Channels 回傳伺服器的來源頻道，依設定時間點（snowflake 數值）由舊到新排序。
func (d *Data) Channels(guildID string) []string {
	chs := d.Guilds[guildID]
	out := make([]string, 0, len(chs))
	for id := range chs {
		out = append(out, id)
	}
	slices.SortFunc(out, func(a, b string) int {
		if c := compareSnowflake(chs[a], chs[b]); c != 0 {
			return c
		}
		return compareSnowflake(a, b)
	})
	return out
}

// SourceChannel 查詢頻道是否為來源頻道。
func (d *Data) SourceChannel(channelID string) (guildID, setAt string, ok bool) {
	for g, chs := range d.Guilds {
		if s, ok := chs[channelID]; ok {
			return g, s, true
		}
	}
	return "", "", false
}

func (d *Data) SetUserChannels(userID string, channelIDs []string) {
	d.UserSelections[userID] = append([]string{}, channelIDs...)
}

// UserChannels 回傳使用者勾選的頻道；chosen=false 表示沒選過。
func (d *Data) UserChannels(userID string) (ids []string, chosen bool) {
	ids, chosen = d.UserSelections[userID]
	if !chosen {
		return nil, false
	}
	return append([]string{}, ids...), true
}

func (d *Data) Clone() *Data {
	c := &Data{
		FormatVersion:  d.FormatVersion,
		Guilds:         make(map[string]map[string]string, len(d.Guilds)),
		UserSelections: make(map[string][]string, len(d.UserSelections)),
	}
	for g, chs := range d.Guilds {
		m := make(map[string]string, len(chs))
		for k, v := range chs {
			m[k] = v
		}
		c.Guilds[g] = m
	}
	for u, ids := range d.UserSelections {
		c.UserSelections[u] = append([]string{}, ids...)
	}
	return c
}

// compareSnowflake 以數值比較 snowflake；無法解析時退回字串比較。
func compareSnowflake(a, b string) int {
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	if errA != nil || errB != nil {
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
		return 0
	}
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}
