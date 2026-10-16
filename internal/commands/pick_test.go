package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"discordbot/internal/access"
	"discordbot/internal/discordtest"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

const pkUser = "u1"

// fakeAvailable 是假的可用頻道判定：refs[userID] 為使用者可用的頻道。
type fakeAvailable struct {
	refs  map[string][]access.ChannelRef
	err   error
	users []string
}

func (a *fakeAvailable) Available(ctx context.Context, userID string) ([]access.ChannelRef, error) {
	a.users = append(a.users, userID)
	if a.err != nil {
		return nil, a.err
	}
	return a.refs[userID], nil
}

type pkEnv struct {
	fake   *discordtest.Fake
	access *fakeAvailable
	st     *store.Store
	s      *Sources
}

func newPKEnv(t *testing.T, refs ...access.ChannelRef) *pkEnv {
	t.Helper()
	f := discordtest.NewFake()
	st := store.New(f, "storage")
	if err := st.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := &fakeAvailable{refs: map[string][]access.ChannelRef{pkUser: refs}}
	return &pkEnv{fake: f, access: a, st: st, s: &Sources{API: f, Access: a, Store: st}}
}

// ref 回傳伺服器 g 的頻道 ch（名稱由 ID 產生）。
func ref(g, ch string) access.ChannelRef {
	return access.ChannelRef{GuildID: g, GuildName: "伺服器" + g, ChannelID: ch, ChannelName: "頻道" + ch}
}

// manyRefs 回傳 n 個頻道 c0..c(n-1)。
func manyRefs(n int) []access.ChannelRef {
	var out []access.ChannelRef
	for i := 0; i < n; i++ {
		out = append(out, ref("g", fmt.Sprint("c", i)))
	}
	return out
}

func (e *pkEnv) choose(t *testing.T, ids ...string) {
	t.Helper()
	err := e.st.Update(context.Background(), func(d *store.Data) error { d.SetUserChannels(pkUser, ids); return nil })
	if err != nil {
		t.Fatal(err)
	}
	e.fake.Sent = nil
}

func sourcesCommand(userID string, dm bool) *discordgo.InteractionCreate {
	i := &discordgo.Interaction{
		ID: "ip", AppID: "app", Token: "tok", Type: discordgo.InteractionApplicationCommand,
		Data: discordgo.ApplicationCommandInteractionData{Name: CmdSources},
	}
	if dm {
		i.User = &discordgo.User{ID: userID}
	} else {
		i.GuildID = "other-guild"
		i.Member = &discordgo.Member{User: &discordgo.User{ID: userID}}
	}
	return &discordgo.InteractionCreate{Interaction: i}
}

// sourcesSelect 是使用者在 msg（/sources 顯示的訊息）的選單 customID 中選了 values。
func sourcesSelect(userID, customID string, values []string, msg *discordgo.Message) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "ic", AppID: "app", Token: "tok2", Type: discordgo.InteractionMessageComponent,
		User:    &discordgo.User{ID: userID},
		Message: msg,
		Data: discordgo.MessageComponentInteractionData{
			CustomID: customID, ComponentType: discordgo.SelectMenuComponent, Values: values,
		},
	}}
}

// shown 執行 /sources，回傳顯示的訊息（內容與元件），並清除呼叫紀錄。
func (e *pkEnv) shown(t *testing.T) *discordgo.Message {
	t.Helper()
	e.s.Command(context.Background(), sourcesCommand(pkUser, false))
	ed := lastSourcesEdit(t, e.fake)
	msg := &discordgo.Message{ID: "m", Content: *ed.Content, Flags: discordgo.MessageFlagsEphemeral}
	if ed.Components != nil {
		msg.Components = *ed.Components
	}
	e.fake.Responses, e.fake.ResponseEdits, e.fake.Sent = nil, nil, nil
	return msg
}

func lastSourcesEdit(t *testing.T, f *discordtest.Fake) *discordgo.WebhookEdit {
	t.Helper()
	if len(f.ResponseEdits) == 0 {
		t.Fatal("沒有補上回應")
	}
	ed := f.ResponseEdits[len(f.ResponseEdits)-1].Edit
	if ed.Content == nil {
		t.Fatal("回應沒有文字")
	}
	return ed
}

// menus 取出元件中的所有選單（依順序）。
func menus(t *testing.T, comps []discordgo.MessageComponent) []discordgo.SelectMenu {
	t.Helper()
	var out []discordgo.SelectMenu
	for _, c := range comps {
		row, ok := c.(discordgo.ActionsRow)
		if !ok {
			t.Fatalf("元件應為 ActionsRow：%T", c)
		}
		for _, rc := range row.Components {
			m, ok := rc.(discordgo.SelectMenu)
			if !ok {
				t.Fatalf("列中應為 SelectMenu：%T", rc)
			}
			out = append(out, m)
		}
	}
	return out
}

func editMenus(t *testing.T, ed *discordgo.WebhookEdit) []discordgo.SelectMenu {
	t.Helper()
	if ed.Components == nil {
		return nil
	}
	return menus(t, *ed.Components)
}

// defaults 回傳選單中 Default 的選項值。
func defaults(ms []discordgo.SelectMenu) []string {
	var out []string
	for _, m := range ms {
		for _, o := range m.Options {
			if o.Default {
				out = append(out, o.Value)
			}
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func chosen(t *testing.T, st *store.Store) ([]string, bool) {
	t.Helper()
	return st.Snapshot().UserChannels(pkUser)
}

// ---- 指令 ----

// 先回 deferred ephemeral（查可用頻道可能接近 2 秒）。
func TestSources_DeferredEphemeral(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"))
	e.s.Command(context.Background(), sourcesCommand(pkUser, false))
	if len(e.fake.Responses) != 1 {
		t.Fatalf("應有 1 個回應：%+v", e.fake.Responses)
	}
	r := e.fake.Responses[0].Response
	if r.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource || r.Data == nil || r.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Errorf("應為 deferred ephemeral：%+v", r)
	}
}

// 沒選過 → 全部 Default（Q46、Q47）；選項文字「伺服器名 / #頻道名」（Q44）；可一個都不選（MinValues 0）。
func TestPick_ShowsAvailableWithDefaults(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"), ref("1", "c2"), ref("2", "c3"))
	e.s.Command(context.Background(), sourcesCommand(pkUser, false))
	ed := lastSourcesEdit(t, e.fake)
	if *ed.Content != msgSourcesPrompt {
		t.Errorf("文字 %q", *ed.Content)
	}
	ms := editMenus(t, ed)
	if len(ms) != 1 {
		t.Fatalf("應有 1 個選單：%d", len(ms))
	}
	m := ms[0]
	if m.MenuType != discordgo.StringSelectMenu || m.CustomID != SourcesMenuPrefix+":0" {
		t.Errorf("選單 %+v", m)
	}
	if m.MinValues == nil || *m.MinValues != 0 || m.MaxValues != 3 {
		t.Errorf("MinValues %v MaxValues %d", m.MinValues, m.MaxValues)
	}
	if len(m.Options) != 3 || m.Options[0].Label != "伺服器1 / #頻道c1" || m.Options[0].Value != "c1" || m.Options[2].Label != "伺服器2 / #頻道c3" {
		t.Errorf("選項 %+v", m.Options)
	}
	if !sameSet(defaults(ms), []string{"c1", "c2", "c3"}) {
		t.Errorf("沒選過應全部預設勾選：%v", defaults(ms))
	}
}

// 選過 → 只有勾選的預設勾選。
func TestPick_DefaultsFollowSelection(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"), ref("1", "c2"))
	e.choose(t, "c2")
	e.s.Command(context.Background(), sourcesCommand(pkUser, false))
	if got := defaults(editMenus(t, lastSourcesEdit(t, e.fake))); !slices.Equal(got, []string{"c2"}) {
		t.Errorf("預設勾選 %v", got)
	}
}

// 選了 0 個 → 一個都不預設勾選。
func TestPick_DefaultsWhenChoseNone(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"))
	e.choose(t)
	e.s.Command(context.Background(), sourcesCommand(pkUser, false))
	if got := defaults(editMenus(t, lastSourcesEdit(t, e.fake))); len(got) != 0 {
		t.Errorf("預設勾選 %v", got)
	}
}

func TestPick_NoChannels(t *testing.T) {
	e := newPKEnv(t)
	e.s.Command(context.Background(), sourcesCommand(pkUser, false))
	ed := lastSourcesEdit(t, e.fake)
	if *ed.Content != msgSourcesNone {
		t.Errorf("文字 %q", *ed.Content)
	}
	if len(editMenus(t, ed)) != 0 {
		t.Error("不應有選單")
	}
}

// 私訊（含與他人的私訊）使用 i.User 的 ID。
func TestPick_DMUsesUserID(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"))
	e.s.Command(context.Background(), sourcesCommand(pkUser, true))
	if !slices.Equal(e.access.users, []string{pkUser}) {
		t.Errorf("查詢的使用者 %v", e.access.users)
	}
}

// 查詢可用頻道失敗 → 提示稍後再試（不顯示選單）。
func TestPick_AccessError(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"))
	e.access.err = context.Canceled
	e.s.Command(context.Background(), sourcesCommand(pkUser, false))
	ed := lastSourcesEdit(t, e.fake)
	if *ed.Content != msgPermissionUnknown || len(editMenus(t, ed)) != 0 {
		t.Errorf("回應 %q", *ed.Content)
	}
}

// 超過 25 個：每 25 個一個選單（Q43）。
func TestPick_MultipleMenus(t *testing.T) {
	e := newPKEnv(t, manyRefs(60)...)
	e.s.Command(context.Background(), sourcesCommand(pkUser, false))
	ed := lastSourcesEdit(t, e.fake)
	ms := editMenus(t, ed)
	if len(ms) != 3 || len(ms[0].Options) != 25 || len(ms[1].Options) != 25 || len(ms[2].Options) != 10 {
		t.Fatalf("選單數與選項數不符：%d", len(ms))
	}
	for n, m := range ms {
		if m.CustomID != fmt.Sprintf("%s:%d", SourcesMenuPrefix, n) || m.MaxValues != len(m.Options) {
			t.Errorf("選單 %d：%s %d", n, m.CustomID, m.MaxValues)
		}
	}
	if ms[1].Options[0].Value != "c25" {
		t.Errorf("第二個選單從 c25 開始：%s", ms[1].Options[0].Value)
	}
	if *ed.Content != msgSourcesPrompt {
		t.Errorf("文字 %q", *ed.Content)
	}
}

// 超過 125 個：只列前 125 個並附說明（Q43）。
func TestPick_Max125(t *testing.T) {
	e := newPKEnv(t, manyRefs(130)...)
	e.s.Command(context.Background(), sourcesCommand(pkUser, false))
	ed := lastSourcesEdit(t, e.fake)
	ms := editMenus(t, ed)
	total := 0
	for _, m := range ms {
		total += len(m.Options)
	}
	if len(ms) != 5 || total != 125 {
		t.Errorf("應有 5 個選單共 125 個選項：%d %d", len(ms), total)
	}
	if *ed.Content != msgSourcesPrompt+"\n"+msgSourcesTooMany {
		t.Errorf("文字 %q", *ed.Content)
	}
}

// 選項文字超過 Discord 上限 100 字時截斷並加「…」。
func TestPick_LongLabelTruncated(t *testing.T) {
	r := access.ChannelRef{GuildID: "1", GuildName: strings.Repeat("伺", 90), ChannelID: "c1", ChannelName: strings.Repeat("頻", 90)}
	e := newPKEnv(t, r)
	e.s.Command(context.Background(), sourcesCommand(pkUser, false))
	l := editMenus(t, lastSourcesEdit(t, e.fake))[0].Options[0].Label
	if n := utf16Units(l); n > 100 || !strings.HasSuffix(l, "…") || !strings.HasPrefix(l, "伺") {
		t.Errorf("長度 %d：%q", n, l)
	}
}

// ---- 選單送出 ----

// 先回 deferred update（寫入儲存頻道需要時間），變更即儲存（Q45），回覆「已更新搜尋頻道」並更新預設勾選。
func TestPick_SaveSelection(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"), ref("1", "c2"), ref("2", "c3"))
	msg := e.shown(t)
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":0", []string{"c1", "c3"}, msg))
	if len(e.fake.Responses) != 1 || e.fake.Responses[0].Response.Type != discordgo.InteractionResponseDeferredMessageUpdate {
		t.Fatalf("應先回 deferred update：%+v", e.fake.Responses)
	}
	if ids, ok := chosen(t, e.st); !ok || !sameSet(ids, []string{"c1", "c3"}) {
		t.Errorf("勾選 %v %v", ids, ok)
	}
	if len(e.fake.Sent) == 0 || e.fake.Sent[0].ChannelID != "storage" {
		t.Error("應寫入儲存頻道")
	}
	ed := lastSourcesEdit(t, e.fake)
	if *ed.Content != msgSourcesUpdated {
		t.Errorf("文字 %q", *ed.Content)
	}
	if got := defaults(editMenus(t, ed)); !sameSet(got, []string{"c1", "c3"}) {
		t.Errorf("更新後的預設勾選 %v", got)
	}
}

// 可以一個都不選（Q46）：存成「選了 0 個」，不是「沒選過」。
func TestPick_SaveNone(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"), ref("1", "c2"))
	msg := e.shown(t)
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":0", nil, msg))
	if ids, ok := chosen(t, e.st); !ok || len(ids) != 0 {
		t.Errorf("應為選了 0 個：%v %v", ids, ok)
	}
}

// 偽造的值（不在目前可用頻道內）不儲存。
func TestPick_FiltersForgedValues(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"), ref("1", "c2"))
	msg := e.shown(t)
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":0", []string{"c1", "hidden", "c9", ""}, msg))
	if ids, _ := chosen(t, e.st); !sameSet(ids, []string{"c1"}) {
		t.Errorf("勾選 %v", ids)
	}
}

// 偽造的選單內容（訊息中的選項被改成看不到的頻道）也只接受目前可用的頻道。
func TestPick_ForgedMessageOptions(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"), ref("1", "c2"))
	e.choose(t, "c1", "c2")
	msg := &discordgo.Message{Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: SourcesMenuPrefix + ":0", Options: []discordgo.SelectMenuOption{{Label: "x", Value: "hidden"}, {Label: "y", Value: "c1"}}},
	}}}}
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":0", []string{"hidden"}, msg))
	if ids, _ := chosen(t, e.st); !sameSet(ids, []string{"c2"}) {
		t.Errorf("勾選 %v（c1 在這頁被取消、hidden 不可加入、c2 不在這頁應保留）", ids)
	}
}

// 看不到但以前選過的頻道保留（Q47）。
func TestPick_KeepsHiddenPreviouslyChosen(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"), ref("1", "c2"))
	e.choose(t, "c1", "hidden")
	msg := e.shown(t)
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":0", []string{"c2"}, msg))
	if ids, _ := chosen(t, e.st); !sameSet(ids, []string{"hidden", "c2"}) {
		t.Errorf("勾選 %v", ids)
	}
}

// 顯示之後才失去權限的頻道：選單裡還有，但沒勾也不刪掉（看不到但以前選過 → 保留）。
func TestPick_ChannelHiddenAfterShownKept(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"), ref("1", "c2"))
	e.choose(t, "c1", "c2")
	msg := e.shown(t)
	e.access.refs[pkUser] = []access.ChannelRef{ref("1", "c1")}
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":0", []string{"c1"}, msg))
	if ids, _ := chosen(t, e.st); !sameSet(ids, []string{"c1", "c2"}) {
		t.Errorf("勾選 %v", ids)
	}
}

// 多個選單：變更一個選單不能清掉其他頁的勾選。
func TestPick_OtherMenusUntouched(t *testing.T) {
	e := newPKEnv(t, manyRefs(30)...)
	e.choose(t, "c0", "c3", "c26")
	msg := e.shown(t)
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":1", []string{"c27", "c29"}, msg))
	if ids, _ := chosen(t, e.st); !sameSet(ids, []string{"c0", "c3", "c27", "c29"}) {
		t.Errorf("勾選 %v", ids)
	}
	if got := defaults(editMenus(t, lastSourcesEdit(t, e.fake))); !sameSet(got, []string{"c0", "c3", "c27", "c29"}) {
		t.Errorf("更新後的預設勾選 %v", got)
	}
}

// 沒選過時變更其中一頁：其他頁維持全部勾選（畫面上看到的狀態）。
func TestPick_FirstChangeKeepsOtherMenusAll(t *testing.T) {
	e := newPKEnv(t, manyRefs(30)...)
	msg := e.shown(t)
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":1", []string{"c25"}, msg))
	want := []string{"c25"}
	for i := 0; i < 25; i++ {
		want = append(want, fmt.Sprint("c", i))
	}
	if ids, ok := chosen(t, e.st); !ok || !sameSet(ids, want) {
		t.Errorf("勾選 %v", ids)
	}
}

// 寫入儲存頻道失敗：回 Q5 文字，設定不變，選單恢復成原本的勾選。
func TestPick_SaveFailure(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"), ref("1", "c2"))
	e.choose(t, "c1")
	msg := e.shown(t)
	e.fake.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":0", []string{"c2"}, msg))
	ed := lastSourcesEdit(t, e.fake)
	if *ed.Content != msgStorageFailed {
		t.Errorf("文字 %q", *ed.Content)
	}
	if ids, _ := chosen(t, e.st); !slices.Equal(ids, []string{"c1"}) {
		t.Errorf("設定不應改變：%v", ids)
	}
	if got := defaults(editMenus(t, ed)); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("選單應恢復原本勾選：%v", got)
	}
}

// 送出時查詢可用頻道失敗：不儲存，提示稍後再試。
func TestPick_SelectAccessError(t *testing.T) {
	e := newPKEnv(t, ref("1", "c1"))
	msg := e.shown(t)
	e.access.err = context.Canceled
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":0", []string{"c1"}, msg))
	if _, ok := chosen(t, e.st); ok {
		t.Error("不應儲存")
	}
	if got := *lastSourcesEdit(t, e.fake).Content; got != msgPermissionUnknown {
		t.Errorf("文字 %q", got)
	}
}

// 訊息中找不到對應的選單（沒有訊息內容）：依目前可用頻道的同一頁計算。
func TestPick_NoMessageFallsBackToPage(t *testing.T) {
	e := newPKEnv(t, manyRefs(30)...)
	e.choose(t, "c0", "c26")
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":1", []string{"c27"}, nil))
	if ids, _ := chosen(t, e.st); !sameSet(ids, []string{"c0", "c27"}) {
		t.Errorf("勾選 %v", ids)
	}
}

func TestSourcesTexts(t *testing.T) {
	cases := map[string]string{
		msgSourcesPrompt:  "勾選要搜尋的圖庫頻道（未選過時預設全部）",
		msgSourcesNone:    "目前沒有可用的圖庫頻道",
		msgSourcesUpdated: "已更新搜尋頻道",
		msgSourcesTooMany: "（可用的圖庫頻道超過 125 個，只列出前 125 個）",
		msgStorageFailed:  "儲存設定失敗，請稍後再試",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("文字 %q，預期 %q", got, want)
		}
	}
}

// 實際的 interaction 中，訊息元件是從 JSON 解出來的（*ActionsRow、*SelectMenu）。
func TestPick_MessageFromJSON(t *testing.T) {
	e := newPKEnv(t, manyRefs(30)...)
	e.choose(t, "c0", "c26")
	shown := e.shown(t)
	comps, err := json.Marshal(shown.Components) // Message 的 MarshalJSON 不輸出元件，另外組
	if err != nil {
		t.Fatal(err)
	}
	b := []byte(`{"id":"m","flags":64,"components":` + string(comps) + `}`)
	if err != nil {
		t.Fatal(err)
	}
	var msg discordgo.Message
	if err := json.Unmarshal(b, &msg); err != nil {
		t.Fatal(err)
	}
	e.access.refs[pkUser] = manyRefs(30)[1:] // 頁面邊界改變：仍以使用者看到的選單為準
	e.s.Component(context.Background(), sourcesSelect(pkUser, SourcesMenuPrefix+":1", []string{"c27"}, &msg))
	if ids, _ := chosen(t, e.st); !sameSet(ids, []string{"c0", "c27"}) {
		t.Errorf("勾選 %v", ids)
	}
	if ms := editMenus(t, lastSourcesEdit(t, e.fake)); len(ms) != 2 || ms[1].Options[0].Value != "c25" {
		t.Errorf("選單應維持使用者看到的頁面：%d %+v", len(ms), ms)
	}
}
