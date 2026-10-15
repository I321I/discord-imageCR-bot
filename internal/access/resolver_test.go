package access

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// fakeMembers 是可設定延遲與錯誤的 GuildMember。沒設定的成員回 404。
type fakeMembers struct {
	mu      sync.Mutex
	members map[string]*discordgo.Member // guildID → 成員
	errs    map[string]error             // guildID → 錯誤
	delay   map[string]time.Duration     // guildID → 延遲
	calls   map[string]int               // guildID → 呼叫次數
}

func newFakeMembers() *fakeMembers {
	return &fakeMembers{members: map[string]*discordgo.Member{}, errs: map[string]error{}, delay: map[string]time.Duration{}, calls: map[string]int{}}
}

func (f *fakeMembers) set(guildID string, roles ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[guildID] = &discordgo.Member{GuildID: guildID, User: &discordgo.User{ID: "u"}, Roles: roles}
}

func (f *fakeMembers) count(guildID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[guildID]
}

func (f *fakeMembers) GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error) {
	f.mu.Lock()
	f.calls[guildID]++
	d := f.delay[guildID]
	f.mu.Unlock()
	time.Sleep(d)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[guildID]; err != nil {
		return nil, err
	}
	m, ok := f.members[guildID]
	if !ok || userID != "u" {
		return nil, &discordgo.RESTError{
			Response: &http.Response{StatusCode: http.StatusNotFound},
			Message:  &discordgo.APIErrorMessage{Code: 10007, Message: "Unknown Member"},
		}
	}
	c := *m
	return &c, nil
}

// clock 是可推進的假時鐘。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	members *fakeMembers
	state   *discordgo.State
	store   *store.Store
	clock   *clock
	r       *Resolver
}

// newEnv：伺服器 g1、g2、g3（@everyone 可檢視），各有來源頻道 c1、c2、c3（setAt 依序）。
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{members: newFakeMembers(), state: discordgo.NewState(), clock: &clock{t: time.Unix(1_000_000, 0)}}
	e.store = store.New(discordtest.NewFake(), "storage")
	e.store.UseEmpty()
	for i := 1; i <= 3; i++ {
		n := strconv.Itoa(i)
		addGuild(t, e.state, "g"+n, "伺服器"+n, view)
		addChannel(t, e.state, "g"+n, "c"+n, "頻道"+n)
		addSource(t, e.store, "g"+n, "c"+n, strconv.Itoa(100+i))
	}
	e.r = &Resolver{Members: e.members, State: e.state, Store: e.store, TTL: 5 * time.Minute, Now: e.clock.now, Deadline: 2 * time.Second}
	return e
}

func addGuild(t *testing.T, s *discordgo.State, id, name string, everyone int64, roles ...*discordgo.Role) {
	t.Helper()
	g := &discordgo.Guild{ID: id, Name: name, OwnerID: "owner", Roles: append([]*discordgo.Role{{ID: id, Permissions: everyone}}, roles...)}
	if err := s.GuildAdd(g); err != nil {
		t.Fatal(err)
	}
}

func addChannel(t *testing.T, s *discordgo.State, guildID, id, name string, ows ...*discordgo.PermissionOverwrite) {
	t.Helper()
	if err := s.ChannelAdd(&discordgo.Channel{ID: id, GuildID: guildID, Name: name, Type: discordgo.ChannelTypeGuildText, PermissionOverwrites: ows}); err != nil {
		t.Fatal(err)
	}
}

func addSource(t *testing.T, st *store.Store, guildID, channelID, setAt string) {
	t.Helper()
	if err := st.Update(context.Background(), func(d *store.Data) error { d.AddChannel(guildID, channelID, setAt); return nil }); err != nil {
		t.Fatal(err)
	}
}

func ids(refs []ChannelRef) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.ChannelID)
	}
	return out
}

func available(t *testing.T, r *Resolver) []string {
	t.Helper()
	refs, err := r.Available(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	return ids(refs)
}

func TestAvailable_MemberChannelsWithNames(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.set("g2")
	refs, err := e.r.Available(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	want := []ChannelRef{
		{GuildID: "g1", GuildName: "伺服器1", ChannelID: "c1", ChannelName: "頻道1"},
		{GuildID: "g2", GuildName: "伺服器2", ChannelID: "c2", ChannelName: "頻道2"},
	}
	if !slices.Equal(refs, want) {
		t.Errorf("得到 %+v，預期 %+v", refs, want)
	}
}

// 非成員（404）被過濾並快取：第二次呼叫不再查詢。
func TestAvailable_FiltersNonMember404AndCaches(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	if got := available(t, e.r); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("得到 %v", got)
	}
	if got := available(t, e.r); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("第二次得到 %v", got)
	}
	for _, g := range []string{"g1", "g2", "g3"} {
		if n := e.members.count(g); n != 1 {
			t.Errorf("%s 應只查詢一次（成員與 404 都快取），得到 %d", g, n)
		}
	}
}

// 404 以外的錯誤不快取，該伺服器這次視為不可用。
func TestAvailable_OtherErrorNotCached(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.set("g2")
	e.members.errs["g2"] = discordtest.ErrInjected
	if got := available(t, e.r); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("錯誤的伺服器應視為不可用：%v", got)
	}
	delete(e.members.errs, "g2")
	if got := available(t, e.r); !slices.Equal(got, []string{"c1", "c2"}) {
		t.Errorf("錯誤不應快取，再次查詢應成功：%v", got)
	}
	if n := e.members.count("g2"); n != 2 {
		t.Errorf("g2 應查詢兩次，得到 %d", n)
	}
}

// 快取過期（推進 Now 超過 TTL）後重新查詢。
func TestAvailable_CacheExpires(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	available(t, e.r)
	e.clock.advance(4 * time.Minute)
	available(t, e.r)
	if n := e.members.count("g1"); n != 1 {
		t.Errorf("TTL 內不應重新查詢，得到 %d", n)
	}
	e.members.set("g2") // 期間加入 g2：快取（404）未過期前看不到
	if got := available(t, e.r); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("TTL 內沿用快取：%v", got)
	}
	e.clock.advance(2 * time.Minute)
	if got := available(t, e.r); !slices.Equal(got, []string{"c1", "c2"}) {
		t.Errorf("過期後應重新查詢：%v", got)
	}
	if n := e.members.count("g1"); n != 2 {
		t.Errorf("過期後應重新查詢，得到 %d", n)
	}
}

// 頻道覆寫拒絕 ViewChannel 的頻道不可用；同伺服器其他頻道不受影響。
func TestAvailable_HiddenChannelExcluded(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1", "r")
	addChannel(t, e.state, "g1", "c1b", "隱藏", roleOW("r", 0, view))
	addSource(t, e.store, "g1", "c1b", "110")
	if got := available(t, e.r); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("得到 %v", got)
	}
}

// 頻道權限不快取：改 state 中的覆寫，成員快取未過期也立即生效。
func TestAvailable_PermissionChangeImmediate(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	if got := available(t, e.r); !slices.Equal(got, []string{"c1"}) {
		t.Fatalf("得到 %v", got)
	}
	addChannel(t, e.state, "g1", "c1", "頻道1", roleOW("g1", 0, view)) // CHANNEL_UPDATE：@everyone 拒絕
	if got := available(t, e.r); len(got) != 0 {
		t.Errorf("覆寫改為拒絕後應立即不可用：%v", got)
	}
	addChannel(t, e.state, "g1", "c1", "頻道1", roleOW("g1", view, 0))
	if got := available(t, e.r); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("覆寫改回允許後應立即可用：%v", got)
	}
	if n := e.members.count("g1"); n != 1 {
		t.Errorf("成員快取未過期，不應重新查詢，得到 %d", n)
	}
}

// 伺服器擁有者、身分組權限也從 state 現算。
func TestAvailable_RolesFromMember(t *testing.T) {
	e := newEnv(t)
	addGuild(t, e.state, "g1", "伺服器1", 0, &discordgo.Role{ID: "viewer", Permissions: view})
	addChannel(t, e.state, "g1", "c1", "頻道1")
	e.members.set("g1")
	if got := available(t, e.r); len(got) != 0 {
		t.Errorf("沒有檢視權限的身分組時不可用：%v", got)
	}
	e2 := newEnv(t)
	addGuild(t, e2.state, "g1", "伺服器1", 0, &discordgo.Role{ID: "viewer", Permissions: view})
	addChannel(t, e2.state, "g1", "c1", "頻道1")
	e2.members.set("g1", "viewer")
	if got := available(t, e2.r); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("有檢視權限的身分組時應可用：%v", got)
	}
}

// state 中找不到伺服器或頻道（bot 已離開、尚未收到 GUILD_CREATE）→ 不可用，也不查詢成員。
func TestAvailable_MissingStateExcluded(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.set("g2")
	addSource(t, e.store, "g9", "c9", "120")  // state 中沒有 g9
	addSource(t, e.store, "g2", "c2x", "121") // state 中沒有 c2x
	if got := available(t, e.r); !slices.Equal(got, []string{"c1", "c2"}) {
		t.Errorf("得到 %v", got)
	}
	if n := e.members.count("g9"); n != 0 {
		t.Errorf("state 中沒有的伺服器不應查詢成員，得到 %d", n)
	}
}

// 未命中的伺服器並行查詢：3 個伺服器各延遲 300ms，總耗時 < 600ms。
func TestAvailable_ParallelFetch(t *testing.T) {
	e := newEnv(t)
	for _, g := range []string{"g1", "g2", "g3"} {
		e.members.set(g)
		e.members.delay[g] = 300 * time.Millisecond
	}
	start := time.Now()
	got := available(t, e.r)
	if d := time.Since(start); d >= 600*time.Millisecond {
		t.Errorf("應並行查詢，耗時 %v", d)
	}
	if len(got) != 3 {
		t.Errorf("得到 %v", got)
	}
}

// Q51：超過截止時間的伺服器先略過、用已查到的結果回傳；查詢在背景繼續並寫入快取。
func TestAvailable_Deadline(t *testing.T) {
	e := newEnv(t)
	e.r.Deadline = 100 * time.Millisecond
	e.members.set("g1")
	e.members.set("g2")
	e.members.delay["g2"] = 400 * time.Millisecond
	start := time.Now()
	got := available(t, e.r)
	if d := time.Since(start); d >= 300*time.Millisecond {
		t.Errorf("應在截止時間回傳，耗時 %v", d)
	}
	if !slices.Equal(got, []string{"c1"}) {
		t.Errorf("截止時應只有已查到的伺服器：%v", got)
	}
	time.Sleep(500 * time.Millisecond) // 背景查詢完成並寫入快取
	if got := available(t, e.r); !slices.Equal(got, []string{"c1", "c2"}) {
		t.Errorf("背景查詢完成後應可用：%v", got)
	}
	if n := e.members.count("g2"); n != 1 {
		t.Errorf("背景查詢結果應寫入快取，g2 查詢 %d 次", n)
	}
}

// 並發未命中同一（伺服器, 使用者）時只查詢一次（共用進行中的查詢）。
func TestAvailable_ConcurrentMissesShareFetch(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.delay["g1"] = 100 * time.Millisecond
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refs, err := e.r.Available(context.Background(), "u")
			if err != nil || !slices.Equal(ids(refs), []string{"c1"}) {
				t.Errorf("得到 %v（%v）", ids(refs), err)
			}
		}()
	}
	wg.Wait()
	if n := e.members.count("g1"); n != 1 {
		t.Errorf("並發未命中應共用查詢，得到 %d 次", n)
	}
}

// ctx 先結束時回傳 ctx 的錯誤。
func TestAvailable_ContextCanceled(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.delay["g1"] = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := e.r.Available(ctx, "u"); err == nil {
		t.Error("ctx 結束時應回錯誤")
	}
}

func TestSelected_DefaultAll(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.set("g2")
	refs, err := e.r.Selected(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(refs); !slices.Equal(got, []string{"c1", "c2"}) {
		t.Errorf("沒選過應為全部可用頻道：%v", got)
	}
}

func setSelection(t *testing.T, st *store.Store, userID string, chs ...string) {
	t.Helper()
	if err := st.Update(context.Background(), func(d *store.Data) error { d.SetUserChannels(userID, chs); return nil }); err != nil {
		t.Fatal(err)
	}
}

// 選過 → 可用 ∩ 勾選（勾選中看不到的頻道不出現）。
func TestSelected_Intersection(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.set("g2")
	setSelection(t, e.store, "u", "c2", "c3") // c3：不是 g3 成員
	refs, err := e.r.Selected(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(refs); !slices.Equal(got, []string{"c2"}) {
		t.Errorf("應為交集：%v", got)
	}
}

// 使用者自己選 0 個 → 沒有頻道；勾選因頻道移除而變成 0 個 → 視為沒選過（store 處理）。
func TestSelected_ZeroSelection(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.set("g2")
	setSelection(t, e.store, "u")
	refs, err := e.r.Selected(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("自己選 0 個應沒有頻道：%v", ids(refs))
	}

	setSelection(t, e.store, "u", "c2")
	if err := e.store.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel("g2", "c2"); return nil }); err != nil {
		t.Fatal(err)
	}
	refs, err = e.r.Selected(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(refs); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("勾選因移除變成 0 個應視為沒選過：%v", got)
	}
}

func TestCanView(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1", "r")
	e.members.set("g2")
	addChannel(t, e.state, "g1", "c1b", "隱藏", roleOW("r", 0, view))
	addSource(t, e.store, "g1", "c1b", "110")
	addChannel(t, e.state, "g1", "notsrc", "非來源")
	cases := []struct {
		ch   string
		want bool
	}{
		{"c1", true}, {"c2", true},
		{"c1b", false},    // 覆寫拒絕
		{"c3", false},     // 不是成員
		{"notsrc", false}, // 不是來源頻道
		{"nope", false},
	}
	for _, c := range cases {
		if got := e.r.CanView(context.Background(), "u", c.ch); got != c.want {
			t.Errorf("CanView(%s) = %v，預期 %v", c.ch, got, c.want)
		}
	}
}

// 可用頻道計算與 gateway state 更新（CHANNEL_UPDATE、GUILD_UPDATE 原地改寫）並行時沒有 data race（-race）。
func TestAvailable_ConcurrentStateUpdatesNoRace(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = e.state.ChannelAdd(&discordgo.Channel{ID: "c1", GuildID: "g1", Name: "頻道1", PermissionOverwrites: []*discordgo.PermissionOverwrite{roleOW("g1", int64(i%2)*view, 0)}})
			_ = e.state.GuildAdd(&discordgo.Guild{ID: "g1", Name: "伺服器1", OwnerID: "owner", Roles: []*discordgo.Role{{ID: "g1", Permissions: view}}})
		}
	}()
	for i := 0; i < 200; i++ {
		if _, err := e.r.Available(context.Background(), "u"); err != nil {
			t.Fatal(err)
		}
		e.r.CanView(context.Background(), "u", "c1")
	}
	close(stop)
	wg.Wait()
}
