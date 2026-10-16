package access

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// viewHist 是可用頻道需要的權限：檢視頻道 + 讀取訊息歷史（階段 7 後的決定）。
const viewHist = view | discordgo.PermissionReadMessageHistory

// 有檢視頻道但沒有讀取訊息歷史的頻道不可用（Available 與 CanView 皆同）。
func TestAvailable_RequiresReadMessageHistory(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.set("g2")
	addChannel(t, e.state, "g1", "c1", "頻道1", roleOW("g1", 0, discordgo.PermissionReadMessageHistory))
	if got := available(t, e.r); !slices.Equal(got, []string{"c2"}) {
		t.Errorf("缺讀取訊息歷史的頻道應排除：%v", got)
	}
	if e.r.CanView(context.Background(), "u", "c1") {
		t.Error("CanView：缺讀取訊息歷史應為 false")
	}
	if !e.r.CanView(context.Background(), "u", "c2") {
		t.Error("CanView：兩個權限都有應為 true")
	}
}

// 伺服器只給檢視頻道（沒有讀取訊息歷史）→ 不可用；頻道覆寫補上讀取訊息歷史後可用。
func TestAvailable_ViewOnlyGuildExcluded(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	addGuild(t, e.state, "g1", "伺服器1", view)
	addChannel(t, e.state, "g1", "c1", "頻道1")
	if got := available(t, e.r); len(got) != 0 {
		t.Errorf("只有檢視頻道不應可用：%v", got)
	}
	addChannel(t, e.state, "g1", "c1", "頻道1", roleOW("g1", discordgo.PermissionReadMessageHistory, 0))
	if got := available(t, e.r); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("覆寫補上讀取訊息歷史後應可用：%v", got)
	}
}

// cacheLen 回傳成員快取的項目數。
func cacheLen(r *Resolver) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cache)
}

// TTL ≤ 0：查詢結果不寫入快取。
func TestCache_TTLZeroDoesNotStore(t *testing.T) {
	e := newEnv(t)
	e.r.TTL = 0
	e.members.set("g1")
	available(t, e.r)
	if n := cacheLen(e.r); n != 0 {
		t.Errorf("TTL=0 不應寫入快取，得到 %d 筆", n)
	}
}

// 過期項目在寫入新項目時被清除：其他使用者的過期快取不會一直累積。
func TestCache_ExpiredEntriesEvictedOnWrite(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	for _, u := range []string{"a", "b", "c"} {
		if _, err := e.r.Available(context.Background(), u); err != nil {
			t.Fatal(err)
		}
	}
	if n := cacheLen(e.r); n != 9 { // 3 個使用者 × 3 個伺服器
		t.Fatalf("快取應有 9 筆，得到 %d", n)
	}
	e.clock.advance(6 * time.Minute)
	if _, err := e.r.Available(context.Background(), "u"); err != nil {
		t.Fatal(err)
	}
	if n := cacheLen(e.r); n != 3 {
		t.Errorf("過期項目應被清除、只剩使用者 u 的 3 筆，得到 %d", n)
	}
}

// CanViewMember 用 interaction 自帶的成員身分組判定，不查詢 REST、不受成員快取影響。
func TestCanViewMember_UsesGivenRolesWithoutREST(t *testing.T) {
	e := newEnv(t)
	addGuild(t, e.state, "g1", "伺服器1", 0, &discordgo.Role{ID: "viewer", Permissions: viewHist})
	addChannel(t, e.state, "g1", "c1", "頻道1")
	addChannel(t, e.state, "g1", "notsrc", "非來源")
	cases := []struct {
		guild, ch string
		roles     []string
		want      bool
	}{
		{"g1", "c1", []string{"viewer"}, true},
		{"g1", "c1", nil, false},                    // 沒有權限的身分組
		{"g1", "notsrc", []string{"viewer"}, false}, // 不是來源頻道
		{"g2", "c1", []string{"viewer"}, false},     // 頻道不屬於該伺服器
		{"g1", "c2", []string{"viewer"}, false},     // 別的伺服器的來源頻道
	}
	for _, c := range cases {
		if got, err := e.r.CanViewMember(c.guild, "u", c.ch, c.roles); got != c.want || err != nil {
			t.Errorf("CanViewMember(%s, %s, %v) = %v, %v，預期 %v", c.guild, c.ch, c.roles, got, err, c.want)
		}
	}
	for _, g := range []string{"g1", "g2", "g3"} {
		if n := e.members.count(g); n != 0 {
			t.Errorf("不應查詢成員（%s %d 次）", g, n)
		}
	}
}

// CheckView 區分「沒權限」（false, nil）與「無法確認」（查詢失敗或 ctx 結束 → 回錯誤）。
func TestCheckView_DistinguishesUnknown(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	if ok, err := e.r.CheckView(context.Background(), "u", "c1"); !ok || err != nil {
		t.Errorf("有權限：%v %v", ok, err)
	}
	if ok, err := e.r.CheckView(context.Background(), "u", "c2"); ok || err != nil {
		t.Errorf("非成員應為 (false, nil)：%v %v", ok, err)
	}
	if ok, err := e.r.CheckView(context.Background(), "u", "nope"); ok || err != nil {
		t.Errorf("非來源應為 (false, nil)：%v %v", ok, err)
	}
	e.members.errs["g3"] = errors.New("boom")
	if ok, err := e.r.CheckView(context.Background(), "u", "c3"); ok || !errors.Is(err, ErrLookupFailed) {
		t.Errorf("查詢失敗應回 ErrLookupFailed：%v %v", ok, err)
	}
	e2 := newEnv(t)
	e2.members.set("g1")
	e2.members.delay["g1"] = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if ok, err := e2.r.CheckView(ctx, "u", "c1"); ok || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("逾時應回 ctx 錯誤：%v %v", ok, err)
	}
}

// S4（階段 8 收尾）：gateway state 尚未載入該伺服器或頻道時，CanViewMember 回 ErrStateUnknown（無法確認），
// 不是「沒權限」。
func TestCanViewMember_StateNotLoadedIsUnknown(t *testing.T) {
	e := newEnv(t)
	addSource(t, e.store, "g9", "c9", "200") // 伺服器與頻道都不在 state
	if ok, err := e.r.CanViewMember("g9", "u", "c9", nil); ok || !errors.Is(err, ErrStateUnknown) {
		t.Errorf("伺服器未載入應為無法確認：%v %v", ok, err)
	}
	addGuild(t, e.state, "g8", "伺服器8", viewHist)
	addSource(t, e.store, "g8", "c8", "201") // 伺服器在 state，頻道不在
	if ok, err := e.r.CanViewMember("g8", "u", "c8", nil); ok || !errors.Is(err, ErrStateUnknown) {
		t.Errorf("頻道未載入應為無法確認：%v %v", ok, err)
	}
}
