package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"discordbot/internal/access"
	"discordbot/internal/discordtest"
	"discordbot/internal/imagemsg"
	"discordbot/internal/index"
	"discordbot/internal/pipeline"

	"github.com/bwmarrin/discordgo"
)

const (
	srGuild   = "10"
	srChannel = "100" // 使用者可用的來源頻道
	srHidden  = "200" // 使用者看不到的來源頻道
	srUser    = "u1"
)

// fakeSelected 是假的可用頻道判定：refs[userID] 為使用者要搜尋的頻道。
type fakeSelected struct {
	mu    sync.Mutex
	refs  map[string][]access.ChannelRef
	err   error
	users []string
}

func (a *fakeSelected) Selected(ctx context.Context, userID string) ([]access.ChannelRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.users = append(a.users, userID)
	if a.err != nil {
		return nil, a.err
	}
	return a.refs[userID], nil
}

// fakeFetcher 依網址回傳 bytes；block 不為 nil 時每次下載先等它（用來觀察並行數）。
type fakeFetcher struct {
	mu       sync.Mutex
	data     map[string][]byte
	err      error
	urls     []string
	inFlight int
	maxIn    int
	block    chan struct{}
	started  chan struct{}
}

func (f *fakeFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	f.mu.Lock()
	f.urls = append(f.urls, url)
	f.inFlight++
	f.maxIn = max(f.maxIn, f.inFlight)
	block, started := f.block, f.started
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.data[url]
	if !ok {
		return nil, errors.New("not found")
	}
	return b, nil
}

func (f *fakeFetcher) fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.urls...)
}

type srEnv struct {
	fake   *discordtest.Fake
	access *fakeSelected
	x      *index.Index
	fetch  *fakeFetcher
	s      *Search
}

func newSREnv(t *testing.T) *srEnv {
	t.Helper()
	f := discordtest.NewFake()
	f.BotID = cmBot
	e := &srEnv{
		fake: f,
		access: &fakeSelected{refs: map[string][]access.ChannelRef{
			srUser: {{GuildID: srGuild, ChannelID: srChannel}},
		}},
		x:     index.New(),
		fetch: &fakeFetcher{data: map[string][]byte{}},
	}
	e.s = &Search{API: f, Access: e.access, Index: e.x, Fetch: e.fetch}
	return e
}

// image 在頻道 ch 放一則 bot 的圖片訊息（附件 bytes 為 data）並收錄進索引。
func (e *srEnv) image(ch string, f imagemsg.Fields, data string) *discordgo.Message {
	m := e.fake.AddMessage(&discordgo.Message{
		ChannelID: ch, Author: &discordgo.User{ID: cmBot, Bot: true}, Content: imagemsg.Format(f, "up"),
	})
	url := fmt.Sprintf("https://cdn.test/%s/%s/a.png", ch, m.ID)
	m.Attachments = []*discordgo.MessageAttachment{{ID: "a" + m.ID, Filename: "a.png", ContentType: "image/png", URL: url, Size: len(data)}}
	e.fetch.data[url] = []byte(data)
	e.x.PutImageMessage(srGuild, m)
	return m
}

// legacy 在頻道 ch 放一則使用者的舊圖片訊息（每張附件 bytes 依 datas）並收錄進索引。
func (e *srEnv) legacy(ch, text string, datas ...string) *discordgo.Message {
	m := e.fake.AddMessage(&discordgo.Message{ChannelID: ch, Author: &discordgo.User{ID: "someone"}, Content: text})
	for n, d := range datas {
		url := fmt.Sprintf("https://cdn.test/%s/%s/%d.png", ch, m.ID, n)
		m.Attachments = append(m.Attachments, &discordgo.MessageAttachment{
			ID: fmt.Sprintf("a%d", n), Filename: fmt.Sprintf("%d.png", n), ContentType: "image/png", URL: url, Size: len(d),
		})
		e.fetch.data[url] = []byte(d)
	}
	e.x.PutLegacy(srGuild, m)
	return m
}

func searchInteraction(typ discordgo.InteractionType, userID, value string, dm bool) *discordgo.InteractionCreate {
	i := &discordgo.Interaction{
		ID: "is", AppID: "app", Token: "tok", Type: typ,
		Data: discordgo.ApplicationCommandInteractionData{
			Name: CmdSearch,
			Options: []*discordgo.ApplicationCommandInteractionDataOption{{
				Name: searchOption, Type: discordgo.ApplicationCommandOptionString, Value: value, Focused: typ == discordgo.InteractionApplicationCommandAutocomplete,
			}},
		},
	}
	if dm {
		i.User = &discordgo.User{ID: userID}
	} else {
		i.GuildID = "other-guild"
		i.Member = &discordgo.Member{User: &discordgo.User{ID: userID}}
	}
	return &discordgo.InteractionCreate{Interaction: i}
}

func autocomplete(userID, value string) *discordgo.InteractionCreate {
	return searchInteraction(discordgo.InteractionApplicationCommandAutocomplete, userID, value, false)
}

func searchSubmit(userID, value string) *discordgo.InteractionCreate {
	return searchInteraction(discordgo.InteractionApplicationCommand, userID, value, false)
}

// choices 回傳唯一的 autocomplete 回應中的選項。
func choices(t *testing.T, f *discordtest.Fake) []*discordgo.ApplicationCommandOptionChoice {
	t.Helper()
	if len(f.Responses) != 1 {
		t.Fatalf("應有 1 個回應，得到 %d", len(f.Responses))
	}
	r := f.Responses[0].Response
	if r.Type != discordgo.InteractionApplicationCommandAutocompleteResult || r.Data == nil {
		t.Fatalf("應為 autocomplete 結果：%+v", r)
	}
	return r.Data.Choices
}

// ---- 提示文字（Q38）----

func TestChoiceName_Format(t *testing.T) {
	cases := []struct {
		name string
		e    index.Entry
		want string
	}{
		{"作品｜圖片文字", index.Entry{Fields: imagemsg.Fields{Work: "芙莉蓮", Text: "這就是人類啊", Characters: "芙莉蓮"}, AttachmentCount: 1}, "芙莉蓮｜這就是人類啊"},
		{"作品空", index.Entry{Fields: imagemsg.Fields{Text: "字幕"}, AttachmentCount: 1}, "字幕"},
		{"圖片文字空", index.Entry{Fields: imagemsg.Fields{Work: "芙莉蓮"}, AttachmentCount: 1}, "芙莉蓮"},
		{"失敗顯示 ⚠️", index.Entry{Fields: imagemsg.Fields{Work: imagemsg.FailedMarker, Text: "字幕"}, AttachmentCount: 1}, "⚠️｜字幕"},
		{"兩欄都失敗", index.Entry{Fields: imagemsg.Fields{Work: imagemsg.FailedMarker, Text: imagemsg.FailedMarker}, AttachmentCount: 1}, "⚠️｜⚠️"},
		{"多行換成空白", index.Entry{Fields: imagemsg.Fields{Work: "芙莉蓮", Text: "第一行\n第二行"}, AttachmentCount: 1}, "芙莉蓮｜第一行 第二行"},
		{"舊訊息", index.Entry{Kind: index.KindLegacy, LegacyText: "舊的\n說明", AttachmentCount: 1}, "舊的 說明"},
		{"舊訊息多圖", index.Entry{Kind: index.KindLegacy, LegacyText: "舊的", Key: index.Key{Attachment: 1}, AttachmentCount: 3}, "舊的（第 2 張）"},
		{"舊訊息多圖第 1 張", index.Entry{Kind: index.KindLegacy, LegacyText: "舊的", AttachmentCount: 2}, "舊的（第 1 張）"},
	}
	for _, c := range cases {
		if got := ChoiceName(c.e); got != c.want {
			t.Errorf("%s：得到 %q，預期 %q", c.name, got, c.want)
		}
	}
}

// 作品與圖片文字都是空的（暫定）：改顯示角色、關鍵字；全部空白顯示「（沒有文字）」。
func TestChoiceName_EmptyFallback(t *testing.T) {
	if got := ChoiceName(index.Entry{Fields: imagemsg.Fields{Characters: "芙莉蓮、費倫", Keywords: "魔法"}, AttachmentCount: 1}); got != "芙莉蓮、費倫｜魔法" {
		t.Errorf("得到 %q", got)
	}
	if got := ChoiceName(index.Entry{AttachmentCount: 1}); got != choiceNameEmpty || choiceNameEmpty != "（沒有文字）" {
		t.Errorf("得到 %q", got)
	}
}

// 超過 100 字（rune）截斷，結尾加「…」；多圖編號保留。
func TestChoiceName_Truncate100Runes(t *testing.T) {
	long := strings.Repeat("字", 150)
	got := ChoiceName(index.Entry{Fields: imagemsg.Fields{Work: "作品", Text: long}, AttachmentCount: 1})
	if n := utf8.RuneCountInString(got); n != 100 || !strings.HasPrefix(got, "作品｜字") || !strings.HasSuffix(got, "字…") {
		t.Errorf("得到 %d 字：%q", n, got)
	}
	got = ChoiceName(index.Entry{Kind: index.KindLegacy, LegacyText: long, Key: index.Key{Attachment: 2}, AttachmentCount: 3})
	if n := utf8.RuneCountInString(got); n != 100 || !strings.HasSuffix(got, "字…（第 3 張）") {
		t.Errorf("得到 %d 字：%q", n, got)
	}
	exact := strings.Repeat("字", 100)
	if got := ChoiceName(index.Entry{Kind: index.KindLegacy, LegacyText: exact, AttachmentCount: 1}); got != exact {
		t.Errorf("剛好 100 字不截斷：%q", got)
	}
}

// ---- autocomplete ----

func TestAutocomplete_Max25(t *testing.T) {
	e := newSREnv(t)
	var newest *discordgo.Message
	for n := 0; n < 30; n++ {
		newest = e.image(srChannel, imagemsg.Fields{Work: fmt.Sprintf("作品%d", n)}, "x")
	}
	e.s.Autocomplete(context.Background(), autocomplete(srUser, ""))
	got := choices(t, e.fake)
	if len(got) != 25 {
		t.Fatalf("應有 25 個提示，得到 %d", len(got))
	}
	// 空輸入顯示最新的（Q33）；Value 為 channelID:messageID:附件序號。
	if got[0].Value != srChannel+":"+newest.ID+":0" || got[0].Name != "作品29" {
		t.Errorf("第一個提示 %+v", got[0])
	}
}

func TestAutocomplete_MatchesQuery(t *testing.T) {
	e := newSREnv(t)
	e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮", Text: "魔法"}, "x")
	e.image(srChannel, imagemsg.Fields{Work: "孤獨搖滾"}, "x")
	e.s.Autocomplete(context.Background(), autocomplete(srUser, "芙莉"))
	got := choices(t, e.fake)
	if len(got) != 1 || got[0].Name != "芙莉蓮｜魔法" {
		t.Errorf("提示 %+v", got)
	}
}

func TestAutocomplete_OnlySelectedChannels(t *testing.T) {
	e := newSREnv(t)
	e.image(srChannel, imagemsg.Fields{Work: "看得到"}, "x")
	e.image(srHidden, imagemsg.Fields{Work: "看不到"}, "x")
	e.s.Autocomplete(context.Background(), autocomplete(srUser, ""))
	got := choices(t, e.fake)
	if len(got) != 1 || got[0].Name != "看得到" {
		t.Errorf("只應有可用頻道的圖：%+v", got)
	}
}

// 私訊（含與他人的私訊、群組）中沒有 Member，使用 i.User 的 ID（Q41）。
func TestAutocomplete_DMUsesUserID(t *testing.T) {
	e := newSREnv(t)
	e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "x")
	e.s.Autocomplete(context.Background(), searchInteraction(discordgo.InteractionApplicationCommandAutocomplete, srUser, "", true))
	if len(e.access.users) != 1 || e.access.users[0] != srUser {
		t.Errorf("應以 i.User 查可用頻道：%v", e.access.users)
	}
	if got := choices(t, e.fake); len(got) != 1 {
		t.Errorf("提示 %+v", got)
	}
}

// 沒有符合結果：不顯示提示（Q34）。
func TestAutocomplete_NoResults(t *testing.T) {
	e := newSREnv(t)
	e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "x")
	e.s.Autocomplete(context.Background(), autocomplete(srUser, "不存在"))
	if got := choices(t, e.fake); len(got) != 0 {
		t.Errorf("不應有提示：%+v", got)
	}
}

// 查詢可用頻道失敗：回空提示，不 panic。
func TestAutocomplete_AccessErrorEmpty(t *testing.T) {
	e := newSREnv(t)
	e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "x")
	e.access.err = context.DeadlineExceeded
	e.s.Autocomplete(context.Background(), autocomplete(srUser, ""))
	if got := choices(t, e.fake); len(got) != 0 {
		t.Errorf("不應有提示：%+v", got)
	}
}

// ---- 送出 ----

// posted 斷言第一個回應為 deferred 公開（非 ephemeral），並回傳最後一次補上的回應。
func posted(t *testing.T, f *discordtest.Fake) discordtest.ResponseEdit {
	t.Helper()
	if len(f.Responses) != 1 {
		t.Fatalf("應有 1 個回應，得到 %d", len(f.Responses))
	}
	r := f.Responses[0].Response
	if r.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource || (r.Data != nil && r.Data.Flags&discordgo.MessageFlagsEphemeral != 0) {
		t.Fatalf("第一個回應應為 deferred 公開：%+v", r)
	}
	if len(f.ResponseEdits) != 1 {
		t.Fatalf("應補上 1 次回應，得到 %d", len(f.ResponseEdits))
	}
	return f.ResponseEdits[0]
}

// failedAfterDefer 斷言 deferred 公開後失敗：刪除原回應並改發 ephemeral followup（內容 want），沒有貼圖。
func failedAfterDefer(t *testing.T, f *discordtest.Fake, want string) {
	t.Helper()
	if len(f.Responses) != 1 || f.Responses[0].Response.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
		t.Fatalf("應先 deferred：%+v", f.Responses)
	}
	if len(f.ResponseDeletes) != 1 {
		t.Errorf("應刪除 deferred 回應，刪了 %d 次", len(f.ResponseDeletes))
	}
	if len(f.Followups) != 1 {
		t.Fatalf("應有 1 個 followup，得到 %d", len(f.Followups))
	}
	p := f.Followups[0].Params
	if p.Content != want || p.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Errorf("followup %+v，預期 ephemeral %q", p, want)
	}
	for _, ed := range f.ResponseEdits {
		if len(ed.Edit.Files) > 0 {
			t.Error("失敗時不應貼圖")
		}
	}
}

func TestSearchSubmit_PostsImageOnly(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮", Text: "這就是人類啊"}, "PNGDATA")
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	ed := posted(t, e.fake)
	if ed.Edit.Content != nil && *ed.Edit.Content != "" {
		t.Errorf("不應有文字：%q", *ed.Edit.Content)
	}
	if ed.Edit.Embeds != nil || ed.Edit.Components != nil {
		t.Error("只發圖片")
	}
	if len(ed.Files) != 1 || string(ed.Files[0]) != "PNGDATA" {
		t.Fatalf("附件 %q", ed.Files)
	}
	if f := ed.Edit.Files[0]; f.Name != "a.png" || f.ContentType != "image/png" {
		t.Errorf("檔名或類型 %+v", f)
	}
}

// 貼圖前重新抓訊息，用最新的附件網址下載（索引不存網址）。
func TestSearchSubmit_FetchesFreshURL(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "OLD")
	m.Attachments[0].URL = "https://cdn.test/fresh.png" // Fake 中的訊息與 m 共用
	e.fetch.data["https://cdn.test/fresh.png"] = []byte("NEW")
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	if ed := posted(t, e.fake); len(ed.Files) != 1 || string(ed.Files[0]) != "NEW" {
		t.Errorf("應用最新網址：%q", ed.Files)
	}
}

// 偽造 value 指向使用者看不到的頻道 → ephemeral「找不到」、不讀訊息、不下載。
func TestSearchSubmit_RejectsUnavailableChannel(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srHidden, imagemsg.Fields{Work: "機密"}, "x")
	e.s.Submit(context.Background(), searchSubmit(srUser, srHidden+":"+m.ID+":0"))
	immediate(t, e.fake, msgSearchImageGone)
	if n := e.fake.CallCount("ChannelMessage"); n != 0 || len(e.fetch.fetched()) != 0 {
		t.Errorf("不應讀訊息或下載：%d %v", n, e.fetch.fetched())
	}
	if _, ok := e.x.Get(index.Key{ChannelID: srHidden, MessageID: m.ID}); !ok {
		t.Error("沒權限不應從索引移除")
	}
}

// 項目的伺服器與可用頻道的伺服器不一致 → 拒絕。
func TestSearchSubmit_RejectsGuildMismatch(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "x")
	e.access.refs[srUser] = []access.ChannelRef{{GuildID: "99", ChannelID: srChannel}}
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	immediate(t, e.fake, msgSearchImageGone)
}

// 不在索引中的 key、附件序號超出範圍 → 「找不到」。
func TestSearchSubmit_UnknownKey(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "x")
	for _, v := range []string{srChannel + ":999:0", srChannel + ":" + m.ID + ":1"} {
		e.fake.Responses = nil
		e.s.Submit(context.Background(), searchSubmit(srUser, v))
		immediate(t, e.fake, msgSearchImageGone)
	}
}

// 訊息已被刪除 → 刪除 deferred 回應、ephemeral「找不到」，並從索引移除（Q40）。
func TestSearchSubmit_MessageDeleted(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "x")
	if err := e.fake.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	failedAfterDefer(t, e.fake, msgSearchImageGone)
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID}); ok {
		t.Error("找不到的圖應從索引移除")
	}
}

// 讀訊息失敗但不是 404（例如網路錯誤）→ 同樣回「找不到」，但不從索引移除（暫定）。
func TestSearchSubmit_MessageFetchErrorKeepsIndex(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "x")
	e.fake.FailOn["ChannelMessage"] = discordtest.ErrInjected
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	failedAfterDefer(t, e.fake, msgSearchImageGone)
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID}); !ok {
		t.Error("非 404 錯誤不應從索引移除")
	}
}

// 抓到的訊息已沒有該附件 → 「找不到」。
func TestSearchSubmit_AttachmentGone(t *testing.T) {
	e := newSREnv(t)
	m := e.legacy(srChannel, "舊的", "A", "B")
	m.Attachments = m.Attachments[:1]
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":1"))
	failedAfterDefer(t, e.fake, msgSearchImageGone)
}

// 沒從提示選、直接送出自己打的字 → 貼出第一個符合的結果（Q35）；沒有符合 → ephemeral「找不到符合的圖片」。
func TestSearchSubmit_FreeText(t *testing.T) {
	e := newSREnv(t)
	e.image(srChannel, imagemsg.Fields{Work: "孤獨搖滾"}, "BOCCHI")
	e.image(srChannel, imagemsg.Fields{Text: "芙莉蓮說的話"}, "TEXT")
	e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "WORK")
	e.image(srHidden, imagemsg.Fields{Work: "芙莉蓮"}, "HIDDEN")
	e.s.Submit(context.Background(), searchSubmit(srUser, "芙莉蓮"))
	if ed := posted(t, e.fake); len(ed.Files) != 1 || string(ed.Files[0]) != "WORK" {
		t.Errorf("應貼出排序第一的結果：%q", ed.Files)
	}

	e2 := newSREnv(t)
	e2.image(srHidden, imagemsg.Fields{Work: "芙莉蓮"}, "HIDDEN")
	e2.s.Submit(context.Background(), searchSubmit(srUser, "芙莉蓮"))
	immediate(t, e2.fake, msgSearchNoMatch)
}

func TestSearchSubmit_LegacySecondAttachment(t *testing.T) {
	e := newSREnv(t)
	m := e.legacy(srChannel, "舊的說明", "FIRST", "SECOND")
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":1"))
	ed := posted(t, e.fake)
	if len(ed.Files) != 1 || string(ed.Files[0]) != "SECOND" || ed.Edit.Files[0].Name != "1.png" {
		t.Errorf("應貼出第二張：%q", ed.Files)
	}
}

// 私訊中也能使用（Q41）。
func TestSearchSubmit_DM(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "PNG")
	e.s.Submit(context.Background(), searchInteraction(discordgo.InteractionApplicationCommand, srUser, srChannel+":"+m.ID+":0", true))
	if ed := posted(t, e.fake); len(ed.Files) != 1 {
		t.Errorf("私訊應可貼圖：%+v", ed)
	}
}

// 下載失敗、長度不符、超過 25MiB、上傳失敗 → 刪除 deferred 回應、ephemeral「貼圖失敗」。
func TestSearchSubmit_PostFailures(t *testing.T) {
	cases := map[string]func(e *srEnv, m *discordgo.Message){
		"下載失敗": func(e *srEnv, m *discordgo.Message) { e.fetch.err = discordtest.ErrInjected },
		"長度不符": func(e *srEnv, m *discordgo.Message) { m.Attachments[0].Size = 999 },
		"超過上限": func(e *srEnv, m *discordgo.Message) { m.Attachments[0].Size = pipeline.MaxAttachmentSize + 1 },
		"上傳失敗": func(e *srEnv, m *discordgo.Message) {
			e.fake.FailOn["InteractionResponseEdit"] = discordtest.ErrInjected
		},
	}
	for name, setup := range cases {
		e := newSREnv(t)
		m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "PNG")
		setup(e, m)
		e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
		t.Run(name, func(t *testing.T) { failedAfterDefer(t, e.fake, msgSearchPostFailed) })
		if name == "超過上限" && len(e.fetch.fetched()) != 0 {
			t.Errorf("超過上限不應下載")
		}
		if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID}); !ok {
			t.Errorf("%s：不應從索引移除", name)
		}
	}
}

// 同時貼圖的數量有上限（Pi 記憶體）：排到才讀訊息與下載。
func TestSearchSubmit_ConcurrencyLimited(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "PNG")
	e.fetch.block = make(chan struct{})
	e.fetch.started = make(chan struct{}, 10)
	var wg sync.WaitGroup
	for n := 0; n < 3; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
		}()
	}
	<-e.fetch.started
	for deadline := time.Now().Add(2 * time.Second); e.fake.CallCount("InteractionRespond") < 3 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // 給排隊中的 goroutine 機會（錯誤地）先讀訊息
	if n := e.fake.CallCount("InteractionRespond"); n != 3 {
		t.Errorf("排隊中也應先 deferred（3 秒內回應），得到 %d", n)
	}
	if n := e.fake.CallCount("ChannelMessage"); n != searchPostSlots {
		t.Errorf("排隊中不應先讀訊息：%d", n)
	}
	close(e.fetch.block)
	wg.Wait()
	if e.fetch.maxIn != searchPostSlots {
		t.Errorf("同時下載數 %d，預期 %d", e.fetch.maxIn, searchPostSlots)
	}
	if len(e.fake.ResponseEdits) != 3 {
		t.Errorf("三個都應貼出：%d", len(e.fake.ResponseEdits))
	}
}

// 排隊中 bot 關機 → 不下載，回「機器人正在重新啟動」。
func TestSearchSubmit_ShutdownWhileQueued(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "PNG")
	shutdown, stop := context.WithCancel(context.Background())
	stop()
	e.s.Ctx = shutdown
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	failedAfterDefer(t, e.fake, msgRestarting)
	if len(e.fetch.fetched()) != 0 {
		t.Error("不應下載")
	}
}

// 送出時查詢可用頻道失敗 → ephemeral「找不到」（不洩漏）。
func TestSearchSubmit_AccessError(t *testing.T) {
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "PNG")
	e.access.err = context.DeadlineExceeded
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	immediate(t, e.fake, msgSearchImageGone)
}

func TestSearchTexts(t *testing.T) {
	cases := map[string]string{
		msgSearchImageGone:  "找不到這張圖片，可能已被刪除",
		msgSearchNoMatch:    "找不到符合的圖片",
		msgSearchPostFailed: "貼圖失敗，請稍後再試",
		searchOption:        "關鍵字",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("文字 %q，預期 %q", got, want)
		}
	}
}
