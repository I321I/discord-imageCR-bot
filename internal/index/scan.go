package index

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"sync"

	"discordbot/internal/recognize"
	"discordbot/internal/snowflake"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// API 是掃描需要的 Discord 方法（*discordgo.Session 直接滿足）。
type API interface {
	ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error)
}

// Writer 是掃描寫入索引的方法（*Index 直接滿足；Hooks 掃描時改用會先確認來源頻道的版本）。
type Writer interface {
	PutImageMessage(guildID string, m *discordgo.Message)
	PutLegacy(guildID string, m *discordgo.Message)
}

// Backfiller 接收要補處理的原訊息（*pipeline.Pipeline 直接滿足）：只排入佇列，不等待處理完成。
// 傳入的 ctx 會跟著訊息留在佇列中，輪到時 ctx 已結束就放棄，所以必須是 bot 關機才取消的 ctx。
type Backfiller interface {
	EnqueueBackfill(ctx context.Context, guildID string, m *discordgo.Message) <-chan error
}

const (
	scanPageSize   = 100
	reactionFailed = "❌" // 與 pipeline 的失敗標記反應相同
)

// ScanChannel 由新到舊分頁讀完頻道歷史：
//   - bot 自己的訊息 → PutImageMessage（不是圖片訊息的會被忽略）；
//   - 其他 bot、webhook、沒有圖片（圖片附件或圖片網址）或含非圖片附件的訊息 → 忽略（Q50）；
//   - 使用者的帶圖訊息，不是新訊息（store.IsNewMessage：附件訊息早於 setAt；只含圖片網址的訊息早於
//     max(setAt, urlSince)，U1）→ 舊圖片訊息（PutLegacy）；
//   - 新訊息：帶有 bot 的 ❌（處理失敗過）→ 不補處理也不收錄（決定 10）；只有 gifv（新訊息不處理，U3）→ 忽略；
//     其他 → 讀完後由舊到新交給補處理。
//
// setAt 不合法（空字串、非數字，設定資料損壞）時無法判斷先後：記 log，使用者的帶圖訊息一律當舊圖片訊息收錄、不補處理。
// urlSince 不合法（尚未寫入）時，只含圖片網址的訊息一律當舊圖片訊息。
//
// 每讀完一頁就寫進索引（掃描期間搜尋回傳已建好的部分，Q52）。ctx 結束時停止並回錯誤，不交給補處理。
// 交給 bf 時傳入的也是 ctx；ctx 會在掃描結束後取消時，bf 要自行改用不會隨掃描結束而取消的 ctx（見 Hooks）。
func ScanChannel(ctx context.Context, api API, x Writer, bf Backfiller, botID, guildID, channelID, setAt, urlSince string) error {
	validSetAt := snowflake.Valid(setAt)
	if !validSetAt {
		log.Printf("頻道 %s 的設定時間點 %q 不合法，使用者圖片一律當舊圖片訊息收錄、不補處理", channelID, setAt)
	}
	var backfill []*discordgo.Message
	before := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := api.ChannelMessages(channelID, scanPageSize, before, "", "", discordgo.WithContext(ctx))
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, m := range page {
			switch {
			case m.Author == nil:
			case m.Author.ID == botID:
				x.PutImageMessage(guildID, m)
			case m.Author.Bot || m.WebhookID != "" || !recognize.Eligible(m):
			case !store.IsNewMessage(m.ID, setAt, urlSince, len(m.Attachments) == 0):
				x.PutLegacy(guildID, m)
			case hasOwnReaction(m, reactionFailed):
			case !hasNewMessageSources(m):
			default:
				backfill = append(backfill, m)
			}
		}
		if len(page) < scanPageSize {
			break
		}
		before = page[len(page)-1].ID
	}
	slices.SortFunc(backfill, func(a, b *discordgo.Message) int { return snowflake.Compare(a.ID, b.ID) })
	for _, m := range backfill {
		if err := ctx.Err(); err != nil {
			return err
		}
		bf.EnqueueBackfill(ctx, guildID, m)
	}
	return nil
}

// hasNewMessageSources 回傳訊息是否有新訊息要處理的圖片（不含 gifv，U3）。
func hasNewMessageSources(m *discordgo.Message) bool {
	srcs, ok := recognize.NewMessageSources(m)
	return ok && len(srcs) > 0
}

// hasOwnReaction 回傳訊息上是否有 bot 自己加的 emoji 反應。
func hasOwnReaction(m *discordgo.Message, emoji string) bool {
	for _, r := range m.Reactions {
		if r != nil && r.Me && r.Emoji != nil && r.Emoji.Name == emoji {
			return true
		}
	}
	return false
}

// Hooks 讓索引跟著來源頻道設定與訊息事件更新：滿足 commands.IndexHooks（頻道新增時掃描、移除時清除），
// 並處理訊息刪除與編輯（Q31）；也是 pipeline 的 IndexSink（頻道移除後不再寫入）。
type Hooks struct {
	ctx   context.Context // 關機時取消：停止掃描
	api   API
	x     *Index
	bf    Backfiller
	store *store.Store

	mu    sync.Mutex
	botID string
	scans map[string]*scan // channelID → 進行中的掃描
	wg    sync.WaitGroup

	// putMu 讓「確認仍是來源頻道再寫入」與 ChannelRemoved 的清除互斥：
	// 設定先移除頻道才呼叫 ChannelRemoved，所以寫入要嘛在清除之前（會被清掉），要嘛看得到頻道已移除。
	putMu sync.Mutex
}

// shutdownBackfiller 把補處理排入佇列時改用 bot 關機 ctx：掃描用的 ctx 在掃描結束時就會取消，
// 若沿用它，佇列輪到時會當成關機而放棄。頻道移除後的放棄由佇列輪到時的來源頻道檢查負責。
type shutdownBackfiller struct {
	ctx context.Context
	bf  Backfiller
}

func (b shutdownBackfiller) EnqueueBackfill(_ context.Context, guildID string, m *discordgo.Message) <-chan error {
	return b.bf.EnqueueBackfill(b.ctx, guildID, m)
}

type scan struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func NewHooks(ctx context.Context, api API, x *Index, bf Backfiller, st *store.Store) *Hooks {
	return &Hooks{ctx: ctx, api: api, x: x, bf: bf, store: st, scans: map[string]*scan{}}
}

// SetBotID 設定 bot 自己的使用者 ID（連線後才知道）。
func (h *Hooks) SetBotID(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.botID = id
}

// BotID 回傳 SetBotID 設定的 bot 使用者 ID；尚未連線時為空字串。
func (h *Hooks) BotID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.botID
}

// ChannelAdded 在背景掃描新設定的來源頻道（伺服器與 setAt 取自目前設定）。
func (h *Hooks) ChannelAdded(guildID, channelID string) {
	h.startScan(channelID)
}

// ChannelRemoved 停止該頻道進行中的掃描並等它結束，再清除該頻道的索引。
// 呼叫可能遲到（頻道已被重新加入）：頻道仍是來源時不停止掃描；取得 putMu 後再確認一次，仍是來源就不清除。
func (h *Hooks) ChannelRemoved(channelID string) {
	if !h.stopScan(channelID) {
		return
	}
	h.putMu.Lock()
	defer h.putMu.Unlock()
	if h.isSource(channelID) {
		return
	}
	h.x.RemoveChannel(channelID)
}

func (h *Hooks) isSource(channelID string) bool {
	_, _, ok := h.store.Snapshot().SourceChannel(channelID)
	return ok
}

// putIfSource 在 putMu 下確認 channelID 仍是來源頻道才執行 put（寫入索引）。
func (h *Hooks) putIfSource(channelID string, put func()) {
	h.putMu.Lock()
	defer h.putMu.Unlock()
	if !h.isSource(channelID) {
		return
	}
	put()
}

// PutImageMessage 收錄 pipeline 發出的圖片訊息；該頻道已不是來源頻道時不寫入。
func (h *Hooks) PutImageMessage(guildID string, m *discordgo.Message) {
	if m == nil {
		return
	}
	h.putIfSource(m.ChannelID, func() { h.x.PutImageMessage(guildID, m) })
}

// sourceWriter 是 Hooks 掃描時使用的 Writer：每次寫入都先確認頻道仍是來源（同 PutImageMessage）。
type sourceWriter struct{ h *Hooks }

func (w sourceWriter) PutImageMessage(guildID string, m *discordgo.Message) {
	w.h.PutImageMessage(guildID, m)
}

func (w sourceWriter) PutLegacy(guildID string, m *discordgo.Message) {
	if m == nil {
		return
	}
	w.h.putIfSource(m.ChannelID, func() { w.h.x.PutLegacy(guildID, m) })
}

// ScanAll 掃描 d 中所有來源頻道（各頻道各一個 goroutine），全部結束才返回；錯誤記 log。
// d 可能是過期的快照（例如 READY 當下）：每個頻道開始掃描前改讀目前設定，已非來源就略過，是來源就用目前的 setAt。
func (h *Hooks) ScanAll(d *store.Data) {
	var dones []chan struct{}
	for _, chs := range d.Guilds {
		for channelID := range chs {
			dones = append(dones, h.startScan(channelID))
		}
	}
	for _, done := range dones {
		<-done
	}
}

// Wait 等待所有進行中的掃描結束。
func (h *Hooks) Wait() { h.wg.Wait() }

// startScan 開始掃描 channelID，回傳掃描結束時關閉的 channel。
// 在 h.mu 內讀目前設定：已非來源頻道就不掃描（回傳已關閉的 channel）；是來源就用目前的伺服器與 setAt。
// stopScan 也在 h.mu 內確認來源，所以頻道移除後不會再登記新的掃描。
// 登記新掃描與取出舊掃描在同一把鎖內完成，並發呼叫時舊的一定會被取消；新掃描等舊的結束才開始。
func (h *Hooks) startScan(channelID string) chan struct{} {
	ctx, cancel := context.WithCancel(h.ctx)
	s := &scan{cancel: cancel, done: make(chan struct{})}
	h.mu.Lock()
	d := h.store.Snapshot()
	urlSince := d.URLSince
	guildID, setAt, ok := d.SourceChannel(channelID)
	if !ok {
		h.mu.Unlock()
		cancel()
		close(s.done)
		return s.done
	}
	old := h.scans[channelID]
	h.scans[channelID] = s
	if old != nil {
		old.cancel() // 登記時就取消，舊掃描不會在新掃描的 goroutine 開始前跑完
	}
	botID := h.botID
	h.wg.Add(1)
	h.mu.Unlock()
	bf := shutdownBackfiller{ctx: h.ctx, bf: h.bf}
	go func() {
		defer h.wg.Done()
		defer close(s.done)
		defer cancel()
		if old != nil {
			<-old.done
		}
		if err := ScanChannel(ctx, h.api, sourceWriter{h}, bf, botID, guildID, channelID, setAt, urlSince); err != nil && ctx.Err() == nil {
			log.Printf("掃描頻道 %s 失敗：%v", channelID, err)
		}
		h.mu.Lock()
		if h.scans[channelID] == s {
			delete(h.scans, channelID)
		}
		h.mu.Unlock()
	}()
	return s.done
}

// stopScan 取消 channelID 進行中的掃描並等待結束，回傳 true。
// 頻道仍是來源（ChannelRemoved 遲到、頻道已被重新加入）時不停止，回傳 false。
// 在 h.mu 內確認：設定先更新才呼叫 ChannelAdded 登記掃描，所以這裡看到的登記中掃描若屬於重新加入，一定也看得到頻道是來源。
func (h *Hooks) stopScan(channelID string) bool {
	h.mu.Lock()
	if h.isSource(channelID) {
		h.mu.Unlock()
		return false
	}
	s := h.scans[channelID]
	delete(h.scans, channelID)
	h.mu.Unlock()
	if s != nil {
		s.cancel()
		<-s.done
	}
	return true
}

// MessageDeleted 移除被刪除訊息的索引項目。
func (h *Hooks) MessageDeleted(channelID, messageID string) {
	h.x.RemoveMessage(channelID, messageID)
}

// MessagesDeleted 移除批次刪除的訊息的索引項目。
func (h *Hooks) MessagesDeleted(channelID string, messageIDs []string) {
	for _, id := range messageIDs {
		h.x.RemoveMessage(channelID, id)
	}
}

// messageGetter 是重讀單則訊息的方法（*discordgo.Session、discordtest.Fake 都有）。
type messageGetter interface {
	ChannelMessage(channelID, messageID string, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// MessageUpdated 依編輯後的訊息更新索引：來源頻道中 bot 的圖片訊息重新收錄；
// 舊圖片訊息（不是新訊息的使用者帶圖訊息，見 store.IsNewMessage）重新收錄，沒有文字或不再有圖片則移除。
// 設定時間點不合法時，使用者訊息一律視為舊圖片訊息（同掃描）。
// 其他訊息（含新訊息：等待處理或處理失敗的原訊息）不動。
// 使用者訊息的更新是部分的（Attachments 或 Embeds 為 nil）時，在 putMu 之外重讀完整訊息再判斷：
// 404 → 移除；其他錯誤 → 記 log、不動。
// 寫入在 putMu 下重新確認頻道仍是來源（同 PutImageMessage）。
func (h *Hooks) MessageUpdated(m *discordgo.Message) {
	if m == nil || m.Author == nil {
		return
	}
	if botID := h.BotID(); botID != "" && m.Author.ID == botID {
		h.putMu.Lock()
		defer h.putMu.Unlock()
		if guildID, _, ok := h.store.Snapshot().SourceChannel(m.ChannelID); ok {
			h.x.PutImageMessage(guildID, m)
		}
		return
	}
	if m.Author.Bot || m.WebhookID != "" {
		return
	}
	d := h.store.Snapshot()
	_, setAt, ok := d.SourceChannel(m.ChannelID)
	if !ok || store.IsNewMessage(m.ID, setAt, d.URLSince, true) {
		return // 不是來源頻道，或不論有沒有附件都是新訊息
	}
	if m.Attachments == nil || m.Embeds == nil {
		getter, ok := h.api.(messageGetter)
		if !ok {
			return
		}
		fresh, err := getter.ChannelMessage(m.ChannelID, m.ID)
		if err != nil {
			var rest *discordgo.RESTError
			if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusNotFound {
				h.x.RemoveMessage(m.ChannelID, m.ID)
				return
			}
			log.Printf("重讀訊息 %s/%s 失敗，索引不更新：%v", m.ChannelID, m.ID, err)
			return
		}
		m = fresh
	}
	h.putMu.Lock()
	defer h.putMu.Unlock()
	d = h.store.Snapshot()
	guildID, setAt, ok := d.SourceChannel(m.ChannelID)
	if !ok || store.IsNewMessage(m.ID, setAt, d.URLSince, len(m.Attachments) == 0) {
		return
	}
	h.x.RemoveMessage(m.ChannelID, m.ID)
	if recognize.Eligible(m) {
		h.x.PutLegacy(guildID, m)
	}
}
