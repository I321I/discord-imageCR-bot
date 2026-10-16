package access

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"discordbot/internal/snowflake"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// DefaultDeadline 是 Available 等待未快取成員查詢的預設截止時間（Q51：autocomplete 須在 3 秒內回應）。
const DefaultDeadline = 2 * time.Second

// MemberFetcher 查詢伺服器成員（*discordgo.Session 直接滿足）。不開 Server Members intent，所以用 REST 查詢。
type MemberFetcher interface {
	GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error)
}

// StateReader 讀取 gateway state（*discordgo.State 直接滿足）。
// state 會在鎖內原地改寫伺服器與頻道物件，所以讀取它們的欄位時要持有讀鎖（RLock）。
type StateReader interface {
	Guild(guildID string) (*discordgo.Guild, error)
	Channel(channelID string) (*discordgo.Channel, error)
	RLock()
	RUnlock()
}

// ChannelRef 是一個使用者可用的來源頻道。
type ChannelRef struct {
	GuildID, GuildName, ChannelID, ChannelName string
}

// Resolver 判定使用者可用的來源頻道：被設為來源 ∧ 使用者是該伺服器成員 ∧ 對該頻道有 View Channel 與 Read Message History。
// 成員資格（含身分組）以 REST 查詢並快取 TTL（404 = 非成員，也快取）；頻道權限不快取，每次從 gateway state 現算。
// 可並行使用。
type Resolver struct {
	Members MemberFetcher
	State   StateReader
	Store   *store.Store
	// TTL 是成員快取時間（MEMBER_CACHE_TTL）；≤ 0 表示不快取、不寫入（進行中的查詢仍會共用）。
	// 過期項目在每次寫入快取時清除。
	TTL time.Duration
	// Now 取得目前時間；nil 時用 time.Now。
	Now func() time.Time
	// Deadline 是 Available 等待未快取查詢的時間上限，超過的伺服器先略過（查詢在背景繼續並寫入快取）；≤ 0 表示不設上限。
	Deadline time.Duration

	mu       sync.Mutex
	cache    map[memberKey]memberEntry
	inflight map[memberKey]*memberCall
}

// NewResolver 回傳使用預設截止時間（DefaultDeadline）與 time.Now 的 Resolver。
func NewResolver(members MemberFetcher, state StateReader, st *store.Store, ttl time.Duration) *Resolver {
	return &Resolver{Members: members, State: state, Store: st, TTL: ttl, Now: time.Now, Deadline: DefaultDeadline}
}

type memberKey struct{ guildID, userID string }

type memberEntry struct {
	member bool
	roles  []string
	at     time.Time
}

// memberCall 是一個進行中的成員查詢；done 關閉後 entry、ok 可讀。
type memberCall struct {
	done  chan struct{}
	entry memberEntry
	ok    bool // false：查詢失敗（404 以外的錯誤），視為不可用
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// lookup 回傳快取中未過期的結果，或共用／開始一個查詢。
func (r *Resolver) lookup(guildID, userID string) (memberEntry, *memberCall, bool) {
	k := memberKey{guildID, userID}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.cache[k]; ok && r.now().Sub(e.at) < r.TTL {
		return e, nil, true
	}
	if c, ok := r.inflight[k]; ok {
		return memberEntry{}, c, false
	}
	if r.inflight == nil {
		r.inflight = map[memberKey]*memberCall{}
	}
	c := &memberCall{done: make(chan struct{})}
	r.inflight[k] = c
	go r.fetch(k, c)
	return memberEntry{}, c, false
}

// fetch 以 REST 查詢成員。不帶呼叫端的 ctx：呼叫端截止後查詢仍在背景完成並寫入快取（Q51）。
func (r *Resolver) fetch(k memberKey, c *memberCall) {
	m, err := r.Members.GuildMember(k.guildID, k.userID)
	var rest *discordgo.RESTError
	switch {
	case err == nil:
		c.entry, c.ok = memberEntry{member: true, roles: slices.Clone(m.Roles)}, true
	case errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusNotFound:
		c.ok = true // 非成員
	default:
		log.Printf("查詢伺服器 %s 的成員 %s 失敗：%v", k.guildID, k.userID, err)
	}
	r.mu.Lock()
	if c.ok {
		c.entry.at = r.now()
		if r.TTL > 0 {
			r.evictExpired(c.entry.at)
			if r.cache == nil {
				r.cache = map[memberKey]memberEntry{}
			}
			r.cache[k] = c.entry
		}
	}
	delete(r.inflight, k)
	r.mu.Unlock()
	close(c.done)
}

// evictExpired 清除過期的快取項目（寫入時順便清，避免快取隨使用者數無限成長）。呼叫端須持有 r.mu。
func (r *Resolver) evictExpired(now time.Time) {
	for k, e := range r.cache {
		if now.Sub(e.at) >= r.TTL {
			delete(r.cache, k)
		}
	}
}

// members 查詢 userID 在 guildIDs 各伺服器的成員資格（快取未命中的並行查詢），回傳是成員的伺服器 → 身分組。
// 等到全部完成、deadline（> 0 時）或 ctx 結束；未完成或查詢失敗的伺服器不列入。
func (r *Resolver) members(ctx context.Context, userID string, guildIDs []string, deadline time.Duration) (map[string][]string, error) {
	out := map[string][]string{}
	type pending struct {
		guildID string
		call    *memberCall
	}
	var waits []pending
	for _, g := range guildIDs {
		e, c, ok := r.lookup(g, userID)
		switch {
		case ok && e.member:
			out[g] = e.roles
		case !ok:
			waits = append(waits, pending{g, c})
		}
	}
	var timeout <-chan time.Time
	if deadline > 0 && len(waits) > 0 {
		t := time.NewTimer(deadline)
		defer t.Stop()
		timeout = t.C
	}
	for _, w := range waits {
		select {
		case <-w.call.done:
			if w.call.ok && w.call.entry.member {
				out[w.guildID] = w.call.entry.roles
			}
		case <-timeout:
			return out, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return out, nil
}

// requiredPerms 是可用頻道需要的權限：檢視頻道 + 讀取訊息歷史（搜尋、勾選列表、右鍵指令皆同）。
const requiredPerms = discordgo.PermissionViewChannel | discordgo.PermissionReadMessageHistory

// view 從 gateway state 現算 userID 能否使用 channelID（須屬於 guildID，且有 requiredPerms）。
func (r *Resolver) view(guildID, channelID, userID string, roles []string) (ChannelRef, bool) {
	g, err := r.State.Guild(guildID)
	if err != nil {
		return ChannelRef{}, false
	}
	ch, err := r.State.Channel(channelID)
	if err != nil {
		return ChannelRef{}, false
	}
	r.State.RLock()
	defer r.State.RUnlock()
	if ch.GuildID != guildID {
		return ChannelRef{}, false
	}
	if ChannelPermissions(g, ch, userID, roles)&requiredPerms != requiredPerms {
		return ChannelRef{}, false
	}
	return ChannelRef{GuildID: guildID, GuildName: g.Name, ChannelID: channelID, ChannelName: ch.Name}, true
}

// available 依設定 d 計算可用頻道：伺服器依 ID（snowflake 數值）排序，同伺服器的頻道依設定時間點由舊到新。
// state 中沒有的伺服器不查詢成員。
func (r *Resolver) available(ctx context.Context, d *store.Data, userID string) ([]ChannelRef, error) {
	var guildIDs []string
	for g, chs := range d.Guilds {
		if len(chs) == 0 {
			continue
		}
		if _, err := r.State.Guild(g); err != nil {
			continue
		}
		guildIDs = append(guildIDs, g)
	}
	slices.SortFunc(guildIDs, func(a, b string) int {
		if c := snowflake.Compare(a, b); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	roles, err := r.members(ctx, userID, guildIDs, r.Deadline)
	if err != nil {
		return nil, err
	}
	var out []ChannelRef
	for _, g := range guildIDs {
		rs, ok := roles[g]
		if !ok {
			continue
		}
		for _, ch := range d.Channels(g) {
			if ref, ok := r.view(g, ch, userID, rs); ok {
				out = append(out, ref)
			}
		}
	}
	return out, nil
}

// Available 回傳 userID 可用的來源頻道。未快取的伺服器超過 Deadline 仍未查到時略過（結果可能少了這些伺服器）。
// ctx 結束時回傳 ctx 的錯誤。
func (r *Resolver) Available(ctx context.Context, userID string) ([]ChannelRef, error) {
	return r.available(ctx, r.Store.Snapshot(), userID)
}

// Selected 回傳 userID 要搜尋的頻道：沒選過 = 全部可用頻道（Q46、Q47）；選過 = 可用 ∩ 勾選（選 0 個 = 沒有頻道）。
// 勾選因頻道移除而變成 0 個時 store 已視為沒選過。
func (r *Resolver) Selected(ctx context.Context, userID string) ([]ChannelRef, error) {
	d := r.Store.Snapshot()
	refs, err := r.available(ctx, d, userID)
	if err != nil {
		return nil, err
	}
	chosen, ok := d.UserChannels(userID)
	if !ok {
		return refs, nil
	}
	return slices.DeleteFunc(refs, func(ref ChannelRef) bool { return !slices.Contains(chosen, ref.ChannelID) }), nil
}

// ErrLookupFailed 表示成員查詢失敗（404 以外的錯誤），無法確認權限。
var ErrLookupFailed = errors.New("member lookup failed")

// CanView 回傳 userID 能否使用來源頻道 channelID（用於檢查 client 送來、可被偽造的值）。
// 無法確認（查詢失敗或 ctx 結束）時回 false；需要區分時用 CheckView。
func (r *Resolver) CanView(ctx context.Context, userID, channelID string) bool {
	ok, err := r.CheckView(ctx, userID, channelID)
	return ok && err == nil
}

// CheckView 同 CanView，但區分「沒權限」與「無法確認」：
// 不是來源頻道、不是成員、頻道權限不足 → (false, nil)；成員查詢失敗 → ErrLookupFailed；ctx 結束 → ctx 的錯誤。
// 不套用 Deadline，等成員查詢完成或 ctx 結束。
func (r *Resolver) CheckView(ctx context.Context, userID, channelID string) (bool, error) {
	guildID, _, ok := r.Store.Snapshot().SourceChannel(channelID)
	if !ok {
		return false, nil
	}
	if _, err := r.State.Guild(guildID); err != nil {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	e, call, ok := r.lookup(guildID, userID)
	if !ok {
		select {
		case <-call.done:
		case <-ctx.Done():
			return false, ctx.Err()
		}
		if !call.ok {
			return false, ErrLookupFailed
		}
		e = call.entry
	}
	if !e.member {
		return false, nil
	}
	_, ok = r.view(guildID, channelID, userID, e.roles)
	return ok, nil
}

// ErrStateUnknown 表示 gateway state 尚未載入該伺服器或頻道，無法確認權限。
var ErrStateUnknown = errors.New("guild or channel not in gateway state")

// CanViewMember 以呼叫端提供的成員身分組（interaction 自帶的 Member.Roles）判定 userID 能否使用來源頻道 channelID，
// 不查詢 REST、不使用成員快取。channelID 必須是 guildID 的來源頻道（roles 只對該伺服器有效），否則 (false, nil)。
// gateway state 尚未載入該伺服器或頻道時回 ErrStateUnknown（無法確認，不是沒權限）。
func (r *Resolver) CanViewMember(guildID, userID, channelID string, roles []string) (bool, error) {
	g, _, ok := r.Store.Snapshot().SourceChannel(channelID)
	if !ok || g != guildID {
		return false, nil
	}
	if _, err := r.State.Guild(guildID); err != nil {
		return false, ErrStateUnknown
	}
	if _, err := r.State.Channel(channelID); err != nil {
		return false, ErrStateUnknown
	}
	_, ok = r.view(guildID, channelID, userID, roles)
	return ok, nil
}
