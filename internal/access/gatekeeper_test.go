package access

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// 把關者補充：權限計算的套用順序與邊界（越權是最嚴重的錯誤）。
func TestGK_Perms_OverwriteOrder(t *testing.T) {
	admin := &discordgo.Role{ID: "adm", Permissions: discordgo.PermissionAdministrator}
	cases := []struct {
		name  string
		g     *discordgo.Guild
		ch    *discordgo.Channel
		user  string
		roles []string
		want  bool
	}{
		// @everyone 覆寫允許、成員角色覆寫拒絕 → 拒絕（角色覆寫在 @everyone 之後）
		{"everyone allow, role deny", testGuild(0), testChannel(roleOW("g", view, 0), roleOW("r", 0, view)), "u", []string{"r"}, false},
		// 沒持有的角色的覆寫不影響
		{"overwrite of role not held", testGuild(view), testChannel(roleOW("other", 0, view)), "u", []string{"r"}, true},
		// 成員覆寫拒絕勝過角色覆寫允許
		{"member deny beats role allow", testGuild(0), testChannel(roleOW("r", view, 0), memberOW("u", 0, view)), "u", []string{"r"}, false},
		// 成員覆寫允許勝過 @everyone 與角色覆寫拒絕
		{"member allow beats role deny", testGuild(view), testChannel(roleOW("g", 0, view), roleOW("r", 0, view), memberOW("u", view, 0)), "u", []string{"r"}, true},
		// 其他成員的成員覆寫不影響
		{"other member overwrite", testGuild(view), testChannel(memberOW("x", 0, view)), "u", nil, true},
		// 型別為成員、ID 等於伺服器 ID 的覆寫不能當成 @everyone 覆寫
		{"member-type overwrite with guild id", testGuild(view), testChannel(memberOW("g", 0, view)), "u", nil, true},
		// Administrator 不受頻道覆寫影響（含成員覆寫拒絕）
		{"admin ignores member deny", testGuild(0, admin), testChannel(roleOW("g", 0, view), memberOW("u", 0, view)), "u", []string{"adm"}, true},
		// Administrator 經 @everyone 角色
		{"admin via everyone", testGuild(discordgo.PermissionAdministrator), testChannel(roleOW("g", 0, view)), "u", nil, true},
		// 擁有者不受覆寫影響
		{"owner ignores deny", testGuild(0), testChannel(roleOW("g", 0, view), memberOW("owner", 0, view)), "owner", nil, true},
		// 成員身分組中有伺服器中不存在的角色（已刪除）→ 不給權限
		{"unknown role", testGuild(0), testChannel(), "u", []string{"deleted"}, false},
		// state 中沒有角色資料（伺服器尚未 GUILD_CREATE）→ 不給權限
		{"no roles in state", &discordgo.Guild{ID: "g", OwnerID: "owner"}, testChannel(), "u", nil, false},
	}
	for _, c := range cases {
		if got := has(ChannelPermissions(c.g, c.ch, c.user, c.roles), view); got != c.want {
			t.Errorf("%s：View = %v，預期 %v", c.name, got, c.want)
		}
	}
}

// 設定說頻道屬於 g2，但 state 中頻道屬於 g1：不可列為可用（伺服器資料不可混用）。
func TestGK_Available_ChannelGuildMismatchExcluded(t *testing.T) {
	e := newEnv(t)
	e.members.set("g2")
	addChannel(t, e.state, "g1", "x", "其他伺服器的頻道")
	addSource(t, e.store, "g2", "x", "120")
	if got := available(t, e.r); !slices.Equal(got, []string{"c2"}) {
		t.Errorf("頻道實際屬於 g1，不應因 g2 成員資格而可用：%v", got)
	}
	if e.r.CanView(context.Background(), "u", "x") {
		t.Error("CanView 不應通過伺服器不符的頻道")
	}
}

// 共用查詢失敗時，所有等待者都視為不可用，且失敗不快取（下次重新查詢）。
func TestGK_SharedFetchErrorPropagatesAndNotCached(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.errs["g1"] = errors.New("boom")
	e.members.delay["g1"] = 100 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refs, err := e.r.Available(context.Background(), "u")
			if err != nil || len(refs) != 0 {
				t.Errorf("查詢失敗應視為不可用：%v（%v）", ids(refs), err)
			}
			if e.r.CanView(context.Background(), "u", "c1") {
				t.Error("查詢失敗時 CanView 應為 false")
			}
		}()
	}
	wg.Wait()
	n := e.members.count("g1")
	e.members.mu.Lock()
	delete(e.members.errs, "g1")
	e.members.delay["g1"] = 0
	e.members.mu.Unlock()
	if got := available(t, e.r); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("錯誤排除後應可用：%v", got)
	}
	if e.members.count("g1") != n+1 {
		t.Errorf("失敗不應快取：前 %d 次、後 %d 次", n, e.members.count("g1"))
	}
}

// TTL ≤ 0：不使用快取，每次都重新查詢。
func TestGK_TTLZeroNoCache(t *testing.T) {
	e := newEnv(t)
	e.r.TTL = 0
	e.members.set("g1")
	available(t, e.r)
	available(t, e.r)
	if n := e.members.count("g1"); n != 2 {
		t.Errorf("TTL=0 應每次查詢，得到 %d 次", n)
	}
}

// CanView 不套用 Deadline：查詢慢於 Deadline 仍等結果。ctx 結束時回 false。
func TestGK_CanViewIgnoresDeadlineButHonorsCtx(t *testing.T) {
	e := newEnv(t)
	e.r.Deadline = 20 * time.Millisecond
	e.members.set("g1")
	e.members.delay["g1"] = 150 * time.Millisecond
	if !e.r.CanView(context.Background(), "u", "c1") {
		t.Error("CanView 不應受 Deadline 影響")
	}
	e.members.set("g2")
	e.members.delay["g2"] = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if e.r.CanView(ctx, "u", "c2") {
		t.Error("ctx 結束時 CanView 應為 false")
	}
}

// 另一個使用者的快取不能被沿用（快取 key 含 userID）。
func TestGK_CacheKeyedByUser(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1") // 只有使用者 u 是成員
	available(t, e.r)
	refs, err := e.r.Available(context.Background(), "intruder")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("非成員不應沿用他人的快取：%v", ids(refs))
	}
}
