package store

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// ErrNotLoaded：還沒成功載入過資料，不可寫入（避免用空資料蓋掉儲存頻道中的舊設定）。
var ErrNotLoaded = errors.New("設定資料尚未載入")

const pageSize = 100

// API 是 Store 需要的 Discord 方法，*discordgo.Session 直接滿足。
type API interface {
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// Store 保存目前的設定資料，並負責與儲存頻道同步。可並行使用。
//
// 鎖的分工：writeMu 讓所有修改（Load 的替換、UseEmpty、Save、Update）依序進行，
// 寫入儲存頻道的網路呼叫期間只持有 writeMu；mu 只在讀取或替換 data 時短暫持有，
// 所以 Snapshot 不會被網路呼叫阻擋。
type Store struct {
	writeMu     sync.Mutex
	lastVersion int64  // 由 writeMu 保護
	generation  uint64 // 每次資料替換或寫入成功就加一，由 writeMu 保護

	mu     sync.RWMutex
	data   *Data
	loaded bool

	api       API
	channelID string
	now       func() time.Time
}

func New(api API, storageChannelID string) *Store {
	return &Store{data: NewData(), api: api, channelID: storageChannelID, now: time.Now}
}

func (s *Store) ChannelID() string { return s.channelID }

// Load 從儲存頻道往回分頁讀取，直到找到完整版本或讀完。
// 頻道中沒有任何設定資料時以空資料載入成功。失敗時不改變目前的資料與狀態。
//
// 讀取期間不持有鎖；若讀取期間有其他寫入完成（Update、Save 等），讀到的可能是較舊的版本，
// 此時捨棄讀到的資料、保留目前資料（目前資料就是儲存頻道中最新寫入的版本）。
func (s *Store) Load(ctx context.Context) error {
	_, err := s.Reload(ctx)
	return err
}

// Reload 同 Load，另外回報讀到的資料是否被套用；讀取期間有其他寫入完成而捨棄讀到的資料時 applied=false。
func (s *Store) Reload(ctx context.Context) (applied bool, err error) {
	s.writeMu.Lock()
	gen := s.generation
	s.writeMu.Unlock()

	var msgs []*discordgo.Message
	before := ""
	var d *Data
	for {
		page, err := s.api.ChannelMessages(s.channelID, pageSize, before, "", "", discordgo.WithContext(ctx))
		if err != nil {
			return false, err
		}
		msgs = append(msgs, page...)
		d, err = DecodeLatest(msgs)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrNoData) && !errors.Is(err, ErrNoCompleteVersion) {
			return false, err
		}
		if len(page) < pageSize {
			if errors.Is(err, ErrNoData) {
				d = NewData()
				break
			}
			return false, err
		}
		before = page[len(page)-1].ID
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.generation != gen {
		return false, nil
	}
	s.replace(d)
	return true, nil
}

// replace 替換目前資料並設為已載入。呼叫者必須持有 writeMu。
func (s *Store) replace(d *Data) {
	s.mu.Lock()
	s.data = d
	s.loaded = true
	s.mu.Unlock()
	s.generation++
}

// UseEmpty 以空資料作為目前資料並允許寫入。用於啟動時讀取失敗、仍要以空資料啟動的情況。
func (s *Store) UseEmpty() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.replace(NewData())
}

// Save 把目前資料寫成儲存頻道中的新版本。
func (s *Store) Save(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// 持有 writeMu 時沒有其他人會修改 data、loaded，可以直接讀取。
	if !s.loaded {
		return ErrNotLoaded
	}
	if err := s.save(ctx, s.data); err != nil {
		return err
	}
	s.generation++
	return nil
}

// save 呼叫者必須持有 writeMu。
func (s *Store) save(ctx context.Context, d *Data) error {
	v := s.now().UnixNano()
	if v <= s.lastVersion {
		v = s.lastVersion + 1
	}
	s.lastVersion = v
	contents, err := Encode(d, strconv.FormatInt(v, 10))
	if err != nil {
		return err
	}
	for _, c := range contents {
		if _, err := s.api.ChannelMessageSendComplex(s.channelID, &discordgo.MessageSend{Content: c}, discordgo.WithContext(ctx)); err != nil {
			return err
		}
	}
	return nil
}

// Update 修改資料的複本並寫入儲存頻道，成功後才替換目前資料；fn 或寫入失敗時記憶體中的資料不變。
// 多個 Update 依序進行。
//
// fn 執行期間持有 writeMu（不可重入），fn 內不可呼叫 Update、Save、Load、Reload、UseEmpty，否則會死鎖。
func (s *Store) Update(ctx context.Context, fn func(d *Data) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.loaded {
		return ErrNotLoaded
	}
	c := s.data.Clone()
	if err := fn(c); err != nil {
		return err
	}
	if err := s.save(ctx, c); err != nil {
		return err
	}
	s.replace(c)
	return nil
}

// Snapshot 回傳目前資料的複本。
func (s *Store) Snapshot() *Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Clone()
}
