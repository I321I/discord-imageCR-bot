package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

// 儲存格式：每則訊息第一行 "imgbot-store <版本ID> <i>/<n>"，第二行起是 JSON 片段。
const (
	maxMessageLen = 2000
	headerPrefix  = "imgbot-store "
)

var (
	// ErrNoData：儲存頻道中沒有任何 store 訊息。
	ErrNoData = errors.New("儲存頻道中沒有設定資料")
	// ErrNoCompleteVersion：有 store 訊息，但沒有任何完整的版本。
	ErrNoCompleteVersion = errors.New("儲存頻道中沒有完整的設定版本")
)

func header(versionID string, i, n int) string {
	return fmt.Sprintf("%s%s %d/%d\n", headerPrefix, versionID, i, n)
}

// Encode 把資料編成一或多則訊息內容（依發送順序），每則不超過 2000 字。
func Encode(d *Data, versionID string) ([]string, error) {
	if strings.ContainsAny(versionID, " \n") || versionID == "" {
		return nil, fmt.Errorf("版本 ID 不合法：%q", versionID)
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	runes := []rune(string(b))

	// 標頭長度與總則數有關：先假設 n，算出需要幾則，不合就重算。
	n := 1
	for {
		size := maxMessageLen - utf8.RuneCountInString(header(versionID, n, n))
		if size <= 0 {
			return nil, fmt.Errorf("版本 ID 太長：%q", versionID)
		}
		need := (len(runes) + size - 1) / size
		if need == 0 {
			need = 1
		}
		if need <= n {
			out := make([]string, 0, need)
			for i := 0; i < need; i++ {
				end := min((i+1)*size, len(runes))
				out = append(out, header(versionID, i+1, need)+string(runes[i*size:end]))
			}
			return out, nil
		}
		n = need
	}
}

type part struct {
	version string
	index   int
	total   int
	body    string
}

func parsePart(content string) (part, bool) {
	if !strings.HasPrefix(content, headerPrefix) {
		return part{}, false
	}
	first, body, ok := strings.Cut(content, "\n")
	if !ok {
		return part{}, false
	}
	fields := strings.Fields(strings.TrimPrefix(first, headerPrefix))
	if len(fields) != 2 {
		return part{}, false
	}
	is, ns, ok := strings.Cut(fields[1], "/")
	if !ok {
		return part{}, false
	}
	i, err1 := strconv.Atoi(is)
	n, err2 := strconv.Atoi(ns)
	if err1 != nil || err2 != nil || n < 1 || i < 1 || i > n {
		return part{}, false
	}
	return part{version: fields[0], index: i, total: n, body: body}, true
}

// DecodeLatest 從訊息（由新到舊）中找出最新的完整版本。
// 完全沒有 store 訊息 → ErrNoData；有 store 訊息但沒有任何完整版本 → ErrNoCompleteVersion。
func DecodeLatest(msgs []*discordgo.Message) (*Data, error) {
	type group struct {
		total int
		parts map[int]string
		bad   bool
	}
	groups := map[string]*group{}
	var order []string // 版本由新到舊（依第一次看到的位置）
	for _, m := range msgs {
		p, ok := parsePart(m.Content)
		if !ok {
			continue
		}
		g := groups[p.version]
		if g == nil {
			g = &group{total: p.total, parts: map[int]string{}}
			groups[p.version] = g
			order = append(order, p.version)
		}
		if g.total != p.total {
			g.bad = true
		}
		g.parts[p.index] = p.body
	}
	if len(order) == 0 {
		return nil, ErrNoData
	}
	for _, v := range order {
		g := groups[v]
		if g.bad || len(g.parts) != g.total {
			continue
		}
		var sb strings.Builder
		for i := 1; i <= g.total; i++ {
			sb.WriteString(g.parts[i])
		}
		return decodeJSON(sb.String())
	}
	return nil, ErrNoCompleteVersion
}

func decodeJSON(s string) (*Data, error) {
	d := &Data{}
	if err := json.Unmarshal([]byte(s), d); err != nil {
		return nil, fmt.Errorf("設定資料 JSON 解析失敗：%w", err)
	}
	if d.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("不支援的設定格式版本 %d", d.FormatVersion)
	}
	if d.Guilds == nil {
		d.Guilds = map[string]map[string]string{}
	}
	if d.UserSelections == nil {
		d.UserSelections = map[string][]string{}
	}
	return d, nil
}
