package commands

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/imagemsg"
	"discordbot/internal/pipeline"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

const (
	cmGuild  = "g1"
	cmSource = "src"
	cmUser   = "u1"
	cmBot    = "bot"
)

// fakeAccess：allowed[userID] 中的頻道可用。
type fakeAccess struct {
	mu          sync.Mutex
	allowed     map[string][]string
	calls       int
	memberCalls []memberCall
	err         error // CheckView 回傳的錯誤（無法確認權限）
	memberErr   error // CanViewMember 回傳的錯誤（快速路徑無法確認）
}

type memberCall struct {
	guildID, userID, channelID string
	roles                      []string
}

// CanViewMember 是快速路徑（interaction 自帶的成員身分組）：判定規則同 CanView，另外記錄呼叫。
func (a *fakeAccess) CanViewMember(guildID, userID, channelID string, roles []string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.memberCalls = append(a.memberCalls, memberCall{guildID, userID, channelID, roles})
	if a.memberErr != nil {
		return false, a.memberErr
	}
	return guildID == cmGuild && slices.Contains(a.allowed[userID], channelID), nil
}

// CheckView：err 不為 nil 時回 (false, err)（無法確認），否則依 allowed。
func (a *fakeAccess) CheckView(ctx context.Context, userID, channelID string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return false, a.err
	}
	return slices.Contains(a.allowed[userID], channelID), nil
}

type cmIndex struct {
	mu   sync.Mutex
	gids []string
	msgs []*discordgo.Message
}

func (x *cmIndex) PutImageMessage(guildID string, m *discordgo.Message) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.gids = append(x.gids, guildID)
	x.msgs = append(x.msgs, m)
}

// fakeReprocessor 是假 pipeline。
type fakeReprocessor struct {
	mu         sync.Mutex
	recognize  func(a *discordgo.MessageAttachment, fields []imagemsg.Field) (imagemsg.Fields, error)
	asked      [][]imagemsg.Field
	lastCtx    context.Context // 最後一次 RecognizeAttachment 收到的 ctx
	reprocErr  error
	reprocHook func() // Reprocess 時呼叫（模擬處理途中關機）
	reproc     []*discordgo.Message
	reprocGIDs []string
}

func (p *fakeReprocessor) RecognizeAttachment(ctx context.Context, a *discordgo.MessageAttachment, fields []imagemsg.Field) (imagemsg.Fields, error) {
	p.mu.Lock()
	p.asked = append(p.asked, slices.Clone(fields))
	p.lastCtx = ctx
	fn := p.recognize
	p.mu.Unlock()
	return fn(a, fields)
}

func (p *fakeReprocessor) Reprocess(ctx context.Context, guildID string, m *discordgo.Message) <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reproc = append(p.reproc, m)
	p.reprocGIDs = append(p.reprocGIDs, guildID)
	if p.reprocHook != nil {
		p.reprocHook()
	}
	ch := make(chan error, 1)
	ch <- p.reprocErr
	return ch
}

type cmEnv struct {
	fake   *discordtest.Fake
	access *fakeAccess
	index  *cmIndex
	pipe   *fakeReprocessor
	c      *ContextMenu
}

func newCMEnv(t *testing.T) *cmEnv {
	t.Helper()
	f := discordtest.NewFake()
	f.BotID = cmBot
	st := store.New(f, "storage")
	st.UseEmpty()
	if err := st.Update(context.Background(), func(d *store.Data) error { d.AddChannel(cmGuild, cmSource, "1"); return nil }); err != nil {
		t.Fatal(err)
	}
	f.Sent = nil
	e := &cmEnv{
		fake:   f,
		access: &fakeAccess{allowed: map[string][]string{cmUser: {cmSource}}},
		index:  &cmIndex{},
		pipe: &fakeReprocessor{recognize: func(a *discordgo.MessageAttachment, fields []imagemsg.Field) (imagemsg.Fields, error) {
			var out imagemsg.Fields
			for _, fd := range fields {
				out.Set(fd, "新的"+imagemsg.Label(fd))
			}
			return out, nil
		}},
	}
	e.c = &ContextMenu{API: f, Access: e.access, Store: st, Index: e.index, Pipeline: e.pipe, BotID: func() string { return cmBot }}
	return e
}

// imageMessage 在來源頻道放一則 bot 的圖片訊息（上傳者 uploader）。
func (e *cmEnv) imageMessage(f imagemsg.Fields, uploader string) *discordgo.Message {
	return e.fake.AddMessage(&discordgo.Message{
		ChannelID: cmSource, GuildID: cmGuild,
		Author:      &discordgo.User{ID: cmBot, Bot: true},
		Content:     imagemsg.Format(f, uploader),
		Attachments: []*discordgo.MessageAttachment{{ID: "a", Filename: "a.png", URL: "https://cdn.test/a.png", Size: 10}},
	})
}

func (e *cmEnv) content(t *testing.T, m *discordgo.Message) string {
	t.Helper()
	got, err := e.fake.ChannelMessage(m.ChannelID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got.Content
}

func menu(name, userID string, m *discordgo.Message) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "i1", Type: discordgo.InteractionApplicationCommand, GuildID: cmGuild, ChannelID: m.ChannelID,
		Member: &discordgo.Member{User: &discordgo.User{ID: userID}},
		Data: discordgo.ApplicationCommandInteractionData{
			Name: name, CommandType: discordgo.MessageApplicationCommand, TargetID: m.ID,
			Resolved: &discordgo.ApplicationCommandInteractionDataResolved{Messages: map[string]*discordgo.Message{m.ID: m}},
		},
	}}
}

// submit 建立編輯表單送出的 interaction；values 為 TextInput CustomID → 值。
func submit(customID, userID string, values map[string]string) *discordgo.InteractionCreate {
	var rows []discordgo.MessageComponent
	for _, id := range []string{"work", "text", "characters", "keywords"} {
		v, ok := values[id]
		if !ok {
			continue
		}
		rows = append(rows, &discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: id, Value: v}}})
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "i2", Type: discordgo.InteractionModalSubmit, GuildID: cmGuild, ChannelID: cmSource,
		Member: &discordgo.Member{User: &discordgo.User{ID: userID}},
		Data:   discordgo.ModalSubmitInteractionData{CustomID: customID, Components: rows},
	}}
}

func editID(m *discordgo.Message) string { return "editimg:" + m.ChannelID + ":" + m.ID }

// formValues 回傳表單的四個欄位值（全部照 f）。
func formValues(f imagemsg.Fields) map[string]string {
	return map[string]string{"work": f.Work, "text": f.Text, "characters": f.Characters, "keywords": f.Keywords}
}

// immediate 斷言唯一的回應是 ephemeral 訊息（type 4）且內容為 want。
func immediate(t *testing.T, f *discordtest.Fake, want string) {
	t.Helper()
	if len(f.Responses) != 1 {
		t.Fatalf("應有 1 個回應，得到 %d", len(f.Responses))
	}
	r := f.Responses[0].Response
	if r.Type != discordgo.InteractionResponseChannelMessageWithSource || r.Data == nil || r.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Fatalf("應為 ephemeral 訊息：%+v", r)
	}
	if r.Data.Content != want {
		t.Errorf("回覆 %q，預期 %q", r.Data.Content, want)
	}
}

// deferred 斷言第一個回應是 deferred ephemeral（type 5），且最後一次補上的內容為 want。
func deferred(t *testing.T, f *discordtest.Fake, want string) {
	t.Helper()
	if len(f.Responses) != 1 {
		t.Fatalf("應有 1 個回應，得到 %d", len(f.Responses))
	}
	r := f.Responses[0].Response
	if r.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource || r.Data == nil || r.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Fatalf("第一個回應應為 deferred ephemeral：%+v", r)
	}
	if len(f.ResponseEdits) == 0 {
		t.Fatal("沒有補上回應內容")
	}
	if got := *f.ResponseEdits[len(f.ResponseEdits)-1].Edit.Content; got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
}

// ---- 編輯圖片訊息：右鍵 ----

func TestEdit_NotBotMessage_Ephemeral(t *testing.T) {
	e := newCMEnv(t)
	user := e.fake.AddMessage(&discordgo.Message{ChannelID: cmSource, Author: &discordgo.User{ID: cmUser}, Content: "hi"})
	e.c.Edit(context.Background(), menu(CmdEditImage, cmUser, user))
	immediate(t, e.fake, msgNotImageMessage)

	e2 := newCMEnv(t)
	other := e2.fake.AddMessage(&discordgo.Message{ChannelID: cmSource, Author: &discordgo.User{ID: cmBot, Bot: true}, Content: "不是圖片訊息"})
	e2.c.Edit(context.Background(), menu(CmdEditImage, cmUser, other))
	immediate(t, e2.fake, msgNotImageMessage)
}

func TestEdit_NoAccess_Ephemeral(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	e.c.Edit(context.Background(), menu(CmdEditImage, "stranger", m))
	immediate(t, e.fake, msgNoPermission)
}

func TestEdit_ModalPrefilled(t *testing.T) {
	e := newCMEnv(t)
	f := imagemsg.Fields{Work: imagemsg.FailedMarker, Text: "第一句\n第二句", Characters: "芙莉蓮、費倫"}
	m := e.imageMessage(f, "up")
	e.c.Edit(context.Background(), menu(CmdEditImage, cmUser, m))
	if len(e.fake.Responses) != 1 {
		t.Fatalf("應有 1 個回應，得到 %d", len(e.fake.Responses))
	}
	r := e.fake.Responses[0].Response
	if r.Type != discordgo.InteractionResponseModal {
		t.Fatalf("應回 modal：%v", r.Type)
	}
	if !strings.HasPrefix(r.Data.CustomID, editID(m)+":") || r.Data.Title != "編輯圖片訊息" {
		t.Errorf("CustomID %q、標題 %q", r.Data.CustomID, r.Data.Title)
	}
	type input struct {
		id, label, value string
		style            discordgo.TextInputStyle
	}
	var got []input
	for _, row := range r.Data.Components {
		ar := row.(discordgo.ActionsRow)
		ti := ar.Components[0].(discordgo.TextInput)
		if ti.Required {
			t.Errorf("%s 不應必填（送出空白即清空）", ti.CustomID)
		}
		got = append(got, input{ti.CustomID, ti.Label, ti.Value, ti.Style})
	}
	want := []input{
		{"work", "作品", imagemsg.FailedMarker, discordgo.TextInputShort}, // Q28：失敗欄位預填失敗標記
		{"text", "圖片文字", "第一句\n第二句", discordgo.TextInputParagraph},
		{"characters", "角色", "芙莉蓮、費倫", discordgo.TextInputShort},
		{"keywords", "關鍵字", "", discordgo.TextInputShort},
	}
	if !slices.Equal(got, want) {
		t.Errorf("表單欄位\n得到 %+v\n預期 %+v", got, want)
	}
}

// ---- 編輯圖片訊息：表單送出 ----

func TestEditSubmit_NormalizesAndKeepsUploader(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮", Text: "這就是人類啊"}, "up")
	e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, map[string]string{
		"work": "葬送的芙莉蓮, Frieren", "text": "甲，乙\n丙", "characters": "芙莉蓮,費倫", "keywords": " 魔法；;旅行 ",
	}))
	want := "作品：葬送的芙莉蓮、Frieren\n圖片文字：甲，乙\n丙\n角色：芙莉蓮、費倫\n關鍵字：魔法、旅行\n上傳者：<@up>"
	if got := e.content(t, m); got != want {
		t.Errorf("內容\n得到 %q\n預期 %q", got, want)
	}
	ed := e.fake.Edited[0]
	if ed.AllowedMentions == nil || ed.AllowedMentions.Parse == nil || len(ed.AllowedMentions.Parse) != 0 {
		t.Errorf("mention 應不通知：%+v", ed.AllowedMentions)
	}
	deferred(t, e.fake, msgUpdated)
}

// Q29：送出空白 → 清空；送出內容仍是失敗標記 → 保留失敗標記。
func TestEditSubmit_BlankClearsMarkerKeeps(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker, Text: imagemsg.FailedMarker, Characters: "芙莉蓮"}, "up")
	e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, map[string]string{
		"work": imagemsg.FailedMarker, "text": "  ", "characters": "", "keywords": "",
	}))
	got, _, ok := imagemsg.Parse(e.content(t, m))
	if !ok {
		t.Fatal("編輯後應仍是圖片訊息")
	}
	if got != (imagemsg.Fields{Work: imagemsg.FailedMarker}) {
		t.Errorf("得到 %+v", got)
	}
}

// 表單送出失敗標記，但期間「重新辨識」已補上該欄：保留最新的值，不把辨識結果蓋回失敗標記。
func TestEditSubmit_MarkerKeepsLatestValue(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")
	latest := imagemsg.Format(imagemsg.Fields{Work: "重新辨識的作品"}, "up")
	if _, err := e.fake.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: m.ID, Channel: m.ChannelID, Content: &latest}); err != nil {
		t.Fatal(err)
	}
	e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, map[string]string{
		"work": imagemsg.FailedMarker, "text": "", "characters": "", "keywords": "新關鍵字",
	}))
	got, _, _ := imagemsg.Parse(e.content(t, m))
	if got.Work != "重新辨識的作品" || got.Keywords != "新關鍵字" {
		t.Errorf("得到 %+v", got)
	}
}

func TestEditSubmit_UpdatesIndex(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, formValues(imagemsg.Fields{Work: "芙莉蓮", Keywords: "魔法"})))
	if len(e.index.msgs) != 1 || e.index.gids[0] != cmGuild || e.index.msgs[0].ID != m.ID || !strings.Contains(e.index.msgs[0].Content, "關鍵字：魔法") {
		t.Errorf("索引應收到編輯後的訊息：%v %+v", e.index.gids, e.index.msgs)
	}
}

func TestEditSubmit_NoAccess(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	e.c.EditSubmit(context.Background(), submit(editID(m), "stranger", formValues(imagemsg.Fields{Work: "改"})))
	if len(e.fake.Edited) != 0 {
		t.Error("沒有權限不應修改")
	}
	if n := e.fake.CallCount("ChannelMessage"); n != 0 {
		t.Errorf("沒有權限不應讀取訊息，讀了 %d 次", n)
	}
	deferred(t, e.fake, msgNoPermission)
}

// 偽造的 CustomID 指向非 bot 訊息、格式錯誤 → 拒絕、不修改。
func TestEditSubmit_ForgedCustomID(t *testing.T) {
	e := newCMEnv(t)
	user := e.fake.AddMessage(&discordgo.Message{ChannelID: cmSource, Author: &discordgo.User{ID: cmUser}, Content: imagemsg.Format(imagemsg.Fields{}, cmUser)})
	e.c.EditSubmit(context.Background(), submit(editID(user), cmUser, formValues(imagemsg.Fields{Work: "改"})))
	if len(e.fake.Edited) != 0 {
		t.Error("非 bot 訊息不應被修改")
	}
	deferred(t, e.fake, msgNotImageMessage)

	for _, id := range []string{"editimg", "editimg:" + cmSource, "editimg::", "editimg:a:b:c:d", "editimg:" + cmSource + "::t"} {
		e2 := newCMEnv(t)
		e2.c.EditSubmit(context.Background(), submit(id, cmUser, formValues(imagemsg.Fields{Work: "改"})))
		if len(e2.fake.Edited) != 0 {
			t.Errorf("%q 不應修改", id)
		}
		deferred(t, e2.fake, msgNotImageMessage)
	}
}

// Q23：表單送出內容超過 2000 字 → 回錯誤、不修改（不截斷）。
func TestEditSubmit_TooLong(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	before := e.content(t, m)
	e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, formValues(imagemsg.Fields{Work: "芙莉蓮", Text: strings.Repeat("字", 2000)})))
	if len(e.fake.Edited) != 0 || e.content(t, m) != before {
		t.Error("超過 2000 字不應修改")
	}
	deferred(t, e.fake, msgTooLong)
}

func TestEditSubmit_MessageDeleted(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	if err := e.fake.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
		t.Fatal(err)
	}
	e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, formValues(imagemsg.Fields{Work: "改"})))
	deferred(t, e.fake, msgMessageGone)
}

func TestEditSubmit_EditFails(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	e.fake.FailOn["ChannelMessageEditComplex"] = discordtest.ErrInjected
	e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, formValues(imagemsg.Fields{Work: "改"})))
	if len(e.index.msgs) != 0 {
		t.Error("修改失敗不應更新索引")
	}
	deferred(t, e.fake, msgUpdateFailed)
}

// ---- 重新辨識：bot 的圖片訊息 ----

func TestRerecognize_NotImageMessage(t *testing.T) {
	e := newCMEnv(t)
	other := e.fake.AddMessage(&discordgo.Message{ChannelID: cmSource, Author: &discordgo.User{ID: "someone"}, Content: "hi",
		Attachments: []*discordgo.MessageAttachment{{Filename: "a.png", ContentType: "image/png", URL: "u", Size: 1}}})
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, other))
	immediate(t, e.fake, msgNotImageMessage)
}

func TestRerecognize_NoFailedFields(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮", Text: "字幕"}, "up")
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	immediate(t, e.fake, msgNoFailedFields)
	if len(e.pipe.asked) != 0 {
		t.Error("沒有失敗欄位不應辨識")
	}
}

// 第一個回應為 deferred ephemeral（type 5）。
func TestRerecognize_Deferred(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	deferred(t, e.fake, msgRerecognized)
}

// 只重跑失敗欄位：作品失敗、圖片文字成功、角色有使用者的值 → 只辨識作品，角色不變。
func TestRerecognize_OnlyFailedFields(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker, Text: "字幕", Characters: "芙莉蓮"}, "up")
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	if len(e.pipe.asked) != 1 || !slices.Equal(e.pipe.asked[0], []imagemsg.Field{imagemsg.FieldWork}) {
		t.Errorf("只應辨識作品：%v", e.pipe.asked)
	}
	want := imagemsg.Format(imagemsg.Fields{Work: "新的作品", Text: "字幕", Characters: "芙莉蓮"}, "up")
	if got := e.content(t, m); got != want {
		t.Errorf("內容\n得到 %q\n預期 %q", got, want)
	}
	ed := e.fake.Edited[0]
	if ed.AllowedMentions == nil || ed.AllowedMentions.Parse == nil || len(ed.AllowedMentions.Parse) != 0 {
		t.Errorf("mention 應不通知：%+v", ed.AllowedMentions)
	}
	if len(e.index.msgs) != 1 || e.index.gids[0] != cmGuild {
		t.Errorf("應更新索引：%v", e.index.gids)
	}
	deferred(t, e.fake, msgRerecognized)
}

// 角色、關鍵字的失敗標記（使用者自己填的）不重跑：只有作品、圖片文字能辨識。
func TestRerecognize_OnlyRecognizableFields(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "作品", Text: "字幕", Characters: imagemsg.FailedMarker}, "up")
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	immediate(t, e.fake, msgNoFailedFields)
}

func TestRerecognize_StillFails_KeepsMarker(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker, Text: imagemsg.FailedMarker}, "up")
	e.pipe.recognize = func(a *discordgo.MessageAttachment, fields []imagemsg.Field) (imagemsg.Fields, error) {
		return imagemsg.Fields{Work: imagemsg.FailedMarker, Text: "讀到了"}, nil
	}
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	got, _, _ := imagemsg.Parse(e.content(t, m))
	if got.Work != imagemsg.FailedMarker || got.Text != "讀到了" {
		t.Errorf("得到 %+v", got)
	}
	deferred(t, e.fake, msgRerecognizedStillFailed)
}

// 辨識進行中使用者修改了訊息（關鍵字改成「新」、作品自己填了）→ 只補仍是失敗標記的欄位，保留使用者的值。
func TestRerecognize_MergesIntoLatestContent(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker, Text: imagemsg.FailedMarker}, "up")
	e.pipe.recognize = func(a *discordgo.MessageAttachment, fields []imagemsg.Field) (imagemsg.Fields, error) {
		latest := imagemsg.Format(imagemsg.Fields{Work: "使用者填的", Text: imagemsg.FailedMarker, Keywords: "新"}, "up")
		if _, err := e.fake.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: m.ID, Channel: m.ChannelID, Content: &latest}); err != nil {
			t.Error(err)
		}
		return imagemsg.Fields{Work: "辨識的作品", Text: "辨識的字幕"}, nil
	}
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	got, _, _ := imagemsg.Parse(e.content(t, m))
	want := imagemsg.Fields{Work: "使用者填的", Text: "辨識的字幕", Keywords: "新"}
	if got != want {
		t.Errorf("得到 %+v，預期 %+v", got, want)
	}
}

func TestRerecognize_NoAccess(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, "stranger", m))
	if len(e.pipe.asked) != 0 || len(e.fake.Edited) != 0 {
		t.Error("沒有權限不應辨識或修改")
	}
	deferred(t, e.fake, msgNoPermission)
}

func TestRerecognize_ImageMissing(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")
	e.pipe.recognize = func(*discordgo.MessageAttachment, []imagemsg.Field) (imagemsg.Fields, error) {
		return imagemsg.Fields{}, discordtest.ErrInjected
	}
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	if len(e.fake.Edited) != 0 {
		t.Error("下載失敗不應修改")
	}
	deferred(t, e.fake, msgImageNotFound)

	e2 := newCMEnv(t)
	noAtt := e2.fake.AddMessage(&discordgo.Message{ChannelID: cmSource, Author: &discordgo.User{ID: cmBot, Bot: true},
		Content: imagemsg.Format(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")})
	e2.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, noAtt))
	if len(e2.pipe.asked) != 0 {
		t.Error("沒有附件不應辨識")
	}
	deferred(t, e2.fake, msgImageNotFound)
}

func TestRerecognize_MessageDeletedDuringRecognition(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")
	e.pipe.recognize = func(*discordgo.MessageAttachment, []imagemsg.Field) (imagemsg.Fields, error) {
		if err := e.fake.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
			t.Error(err)
		}
		return imagemsg.Fields{Work: "作品"}, nil
	}
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	deferred(t, e.fake, msgMessageGone)
}

// interaction token 過期（InteractionResponseEdit 失敗）：只記 log，圖片訊息仍更新。
func TestRerecognize_TokenExpiredOnlyLogs(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")
	e.fake.FailOn["InteractionResponseEdit"] = discordtest.ErrInjected
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	got, _, _ := imagemsg.Parse(e.content(t, m))
	if got.Work != "新的作品" {
		t.Errorf("token 過期仍應更新圖片訊息：%+v", got)
	}
}

// ---- 重新辨識：自己發的、帶 bot ❌ 的原訊息 ----

func failedOriginal(e *cmEnv, author string, withMark bool) *discordgo.Message {
	m := &discordgo.Message{ChannelID: cmSource, GuildID: cmGuild, Author: &discordgo.User{ID: author}, Content: "原文",
		Attachments: []*discordgo.MessageAttachment{{Filename: "a.png", ContentType: "image/png", URL: "https://cdn.test/a.png", Size: 10}}}
	if withMark {
		m.Reactions = []*discordgo.MessageReactions{{Count: 1, Me: true, Emoji: &discordgo.Emoji{Name: "❌"}}}
	}
	return e.fake.AddMessage(m)
}

func TestRerecognize_FailedOriginalReprocesses(t *testing.T) {
	e := newCMEnv(t)
	m := failedOriginal(e, cmUser, true)
	resolved := *m
	resolved.Reactions = nil // 不依賴 resolved 訊息帶反應：以重新取得的最新訊息判斷
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, &resolved))
	if len(e.pipe.reproc) != 1 || e.pipe.reproc[0].ID != m.ID || e.pipe.reprocGIDs[0] != cmGuild {
		t.Fatalf("應重新處理整則：%v %v", e.pipe.reproc, e.pipe.reprocGIDs)
	}
	if len(e.pipe.asked) != 0 {
		t.Error("不應走圖片訊息的重新辨識")
	}
	deferred(t, e.fake, msgReprocessed)
}

func TestRerecognize_FailedOriginalReprocessFails(t *testing.T) {
	e := newCMEnv(t)
	m := failedOriginal(e, cmUser, true)
	e.pipe.reprocErr = discordtest.ErrInjected
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	deferred(t, e.fake, msgReprocessFailed)
}

// 別人發的原訊息：不可重新處理。
func TestRerecognize_OthersOriginalRejected(t *testing.T) {
	e := newCMEnv(t)
	e.access.allowed["other"] = []string{cmSource}
	m := failedOriginal(e, "other", true)
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	immediate(t, e.fake, msgNotImageMessage)
	if len(e.pipe.reproc) != 0 {
		t.Error("不應重新處理別人的訊息")
	}
}

// 自己的原訊息但沒有 bot 的 ❌（還在排隊、或不是處理失敗）→ 不重新處理。
func TestRerecognize_OwnOriginalWithoutMark(t *testing.T) {
	e := newCMEnv(t)
	m := failedOriginal(e, cmUser, false)
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	if len(e.pipe.reproc) != 0 {
		t.Error("沒有 ❌ 不應重新處理")
	}
	deferred(t, e.fake, msgNotFailedOriginal)
}

func TestRerecognize_FailedOriginalNoAccess(t *testing.T) {
	e := newCMEnv(t)
	e.access.allowed[cmUser] = nil
	m := failedOriginal(e, cmUser, true)
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	if len(e.pipe.reproc) != 0 {
		t.Error("沒有權限不應重新處理")
	}
	deferred(t, e.fake, msgNoPermission)
}

// 原訊息在處理前已被刪除 → 不重新處理。
func TestRerecognize_FailedOriginalDeleted(t *testing.T) {
	e := newCMEnv(t)
	m := failedOriginal(e, cmUser, true)
	if err := e.fake.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
		t.Fatal(err)
	}
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	if len(e.pipe.reproc) != 0 {
		t.Error("已刪除不應重新處理")
	}
	deferred(t, e.fake, msgImageNotFound)
}

// S2：開表單時有 interaction 自帶的成員資料 → 用它判定權限（快速路徑），不走需要 REST 的 CanView。
func TestEdit_UsesInteractionMemberFastPath(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	i := menu(CmdEditImage, cmUser, m)
	i.Member.Roles = []string{"r1"}
	e.c.Edit(context.Background(), i)
	if e.access.calls != 0 {
		t.Errorf("有成員資料時不應呼叫 CanView，呼叫 %d 次", e.access.calls)
	}
	want := []memberCall{{cmGuild, cmUser, cmSource, []string{"r1"}}}
	if len(e.access.memberCalls) != 1 || e.access.memberCalls[0].guildID != want[0].guildID ||
		e.access.memberCalls[0].channelID != cmSource || !slices.Equal(e.access.memberCalls[0].roles, want[0].roles) {
		t.Errorf("快速路徑呼叫 %+v", e.access.memberCalls)
	}
	if e.fake.Responses[0].Response.Type != discordgo.InteractionResponseModal {
		t.Error("有權限應開表單")
	}

	e2 := newCMEnv(t)
	m2 := e2.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	e2.c.Edit(context.Background(), menu(CmdEditImage, "stranger", m2))
	immediate(t, e2.fake, msgNoPermission)
}

// 沒有成員資料（例如私訊，不應發生）時退回 CanView。
func TestEdit_NoMemberFallsBackToCanView(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	i := menu(CmdEditImage, cmUser, m)
	i.Member = nil
	i.User = &discordgo.User{ID: cmUser}
	e.c.Edit(context.Background(), i)
	if e.access.calls != 1 || len(e.access.memberCalls) != 0 {
		t.Errorf("應退回 CanView：calls=%d member=%d", e.access.calls, len(e.access.memberCalls))
	}
}

// S6：重新辨識在 bot 關機（c.Ctx 取消）時取消；interaction 的 ctx 取消也一樣。
func TestRerecognize_CanceledOnShutdown(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")
	shutdown, stop := context.WithCancel(context.Background())
	e.c.Ctx = shutdown
	var errDuring error
	e.pipe.recognize = func(*discordgo.MessageAttachment, []imagemsg.Field) (imagemsg.Fields, error) {
		stop()
		select { // 取消是非同步傳遞的
		case <-e.pipe.lastCtx.Done():
		case <-time.After(time.Second):
		}
		errDuring = e.pipe.lastCtx.Err()
		return imagemsg.Fields{}, errDuring
	}
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	if errDuring == nil {
		t.Error("關機時重新辨識的 ctx 應被取消")
	}

	e2 := newCMEnv(t)
	m2 := e2.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")
	e2.c.Ctx = context.Background()
	ictx, icancel := context.WithCancel(context.Background())
	icancel()
	e2.c.Rerecognize(ictx, menu(CmdRerecognize, cmUser, m2))
	if e2.pipe.lastCtx == nil || e2.pipe.lastCtx.Err() == nil {
		t.Error("interaction 的 ctx 取消也應取消重新辨識")
	}
}

// 使用者看得到的文字（階段 8 的使用者決定）。
func TestContextMenuTexts(t *testing.T) {
	cases := map[string]string{
		msgNotImageMessage:         "只能對圖庫 bot 發的圖片訊息，或處理失敗（❌）的原訊息使用",
		msgNoPermission:            "你沒有權限使用此功能",
		msgUpdated:                 "已更新",
		msgTooLong:                 "內容超過 Discord 2000 字上限，未修改",
		msgUpdateFailed:            "更新失敗，請稍後再試",
		msgMessageGone:             "找不到這則圖片訊息，可能已被刪除",
		msgNoFailedFields:          "沒有辨識失敗的欄位，不需要重新辨識",
		msgRerecognized:            "重新辨識完成",
		msgRerecognizedStillFailed: "重新辨識完成，仍有欄位辨識失敗",
		msgImageNotFound:           "找不到原圖，無法重新辨識",
		msgNotFailedOriginal:       "這則訊息沒有處理失敗的標記（❌），不需要重新處理",
		msgReprocessed:             "重新處理完成",
		msgReprocessFailed:         "重新處理失敗，已在原訊息加上 ❌",
		msgAlreadyQueued:           "這則訊息已在處理佇列中，請稍候",
		msgPermissionUnknown:       "目前無法確認權限，請稍後再試",
		msgRestarting:              "機器人正在重新啟動，請稍後再試",
		msgNotSource:               "這個頻道已不是圖庫頻道",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("文字 %q，預期 %q", got, want)
		}
	}
	if editModalTitle != "編輯圖片訊息" {
		t.Errorf("表單標題 %q", editModalTitle)
	}
}

// ---- 無法確認權限 ----

func TestEdit_PermissionUnknown(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	e.access.err = context.DeadlineExceeded
	i := menu(CmdEditImage, cmUser, m)
	i.Member = nil
	i.User = &discordgo.User{ID: cmUser}
	e.c.Edit(context.Background(), i)
	immediate(t, e.fake, msgPermissionUnknown)
}

func TestEditSubmit_PermissionUnknown(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	e.access.err = errors.New("lookup failed")
	e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, formValues(imagemsg.Fields{Work: "改"})))
	if len(e.fake.Edited) != 0 {
		t.Error("無法確認權限不應修改")
	}
	deferred(t, e.fake, msgPermissionUnknown)
}

func TestRerecognize_PermissionUnknown(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")
	e.access.err = errors.New("lookup failed")
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	deferred(t, e.fake, msgPermissionUnknown)

	e2 := newCMEnv(t)
	o := failedOriginal(e2, cmUser, true)
	e2.access.err = errors.New("lookup failed")
	e2.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, o))
	if len(e2.pipe.reproc) != 0 {
		t.Error("無法確認權限不應重新處理")
	}
	deferred(t, e2.fake, msgPermissionUnknown)
}

// ---- S4：重新處理被略過 ----

func TestRerecognize_FailedOriginalAlreadyQueued(t *testing.T) {
	e := newCMEnv(t)
	m := failedOriginal(e, cmUser, true)
	e.pipe.reprocErr = pipeline.ErrAlreadyQueued
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	deferred(t, e.fake, msgAlreadyQueued)
}

// 輪到時頻道已非來源 → 回「這個頻道已不是圖庫頻道」（不是「重新處理完成」）。
// 階段 8 收尾依使用者決定改文字（原為「你沒有權限使用此功能」）。
func TestRerecognize_FailedOriginalNoLongerSource(t *testing.T) {
	e := newCMEnv(t)
	m := failedOriginal(e, cmUser, true)
	e.pipe.reprocErr = pipeline.ErrNotSource
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	deferred(t, e.fake, msgNotSource)
}

// ---- S3：關機 ----

func TestRerecognize_FailedOriginalShutdown(t *testing.T) {
	e := newCMEnv(t)
	m := failedOriginal(e, cmUser, true)
	shutdown, stop := context.WithCancel(context.Background())
	e.c.Ctx = shutdown
	e.pipe.reprocHook = stop
	e.pipe.reprocErr = context.Canceled
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	deferred(t, e.fake, msgRestarting)
}

func TestRerecognize_ImageShutdownDoesNotWrite(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker, Text: imagemsg.FailedMarker}, "up")
	shutdown, stop := context.WithCancel(context.Background())
	e.c.Ctx = shutdown
	e.pipe.recognize = func(*discordgo.MessageAttachment, []imagemsg.Field) (imagemsg.Fields, error) {
		stop()
		<-e.pipe.lastCtx.Done()
		// 關機中斷：pipeline 已完成的欄位有值，被中斷的欄位為失敗標記，不回錯誤
		return imagemsg.Fields{Work: "作品", Text: imagemsg.FailedMarker}, nil
	}
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	if len(e.fake.Edited) != 0 {
		t.Error("關機中斷不應寫回")
	}
	deferred(t, e.fake, msgRestarting)
}

// ---- 只套用表單中實際改過的欄位 ----

// openForm 以 interaction ID id 開表單，回傳 modal 的 CustomID。
func openForm(t *testing.T, e *cmEnv, m *discordgo.Message, id string) string {
	t.Helper()
	i := menu(CmdEditImage, cmUser, m)
	i.ID = id
	n := len(e.fake.Responses)
	e.c.Edit(context.Background(), i)
	if len(e.fake.Responses) != n+1 || e.fake.Responses[n].Response.Type != discordgo.InteractionResponseModal {
		t.Fatal("應開出表單")
	}
	return e.fake.Responses[n].Response.Data.CustomID
}

// A、B 先後開表單；A 改角色送出；B 只改關鍵字送出 → 角色保留 A 的值。
func TestEditSubmit_OnlyChangedFieldsApplied(t *testing.T) {
	e := newCMEnv(t)
	orig := imagemsg.Fields{Work: "芙莉蓮", Characters: "舊角色", Keywords: "舊關鍵字"}
	m := e.imageMessage(orig, "up")
	idA := openForm(t, e, m, "ia")
	idB := openForm(t, e, m, "ib")
	if idA == idB {
		t.Fatalf("兩個表單的 CustomID 應不同：%q", idA)
	}
	a := formValues(orig)
	a["characters"] = "A的角色"
	e.c.EditSubmit(context.Background(), submit(idA, cmUser, a))
	b := formValues(orig)
	b["keywords"] = "B的關鍵字"
	e.c.EditSubmit(context.Background(), submit(idB, cmUser, b))
	got, _, _ := imagemsg.Parse(e.content(t, m))
	want := imagemsg.Fields{Work: "芙莉蓮", Characters: "A的角色", Keywords: "B的關鍵字"}
	if got != want {
		t.Errorf("得到 %+v，預期 %+v", got, want)
	}
}

// 記錄超過 15 分鐘（或遺失）→ 退回全部欄位套用；過期記錄會被清除。
func TestEditSubmit_ExpiredRecordAppliesAll(t *testing.T) {
	e := newCMEnv(t)
	now := time.Unix(1_000_000, 0)
	e.c.Now = func() time.Time { return now }
	orig := imagemsg.Fields{Work: "芙莉蓮", Characters: "舊角色"}
	m := e.imageMessage(orig, "up")
	idA := openForm(t, e, m, "ia")
	idB := openForm(t, e, m, "ib")
	latest := imagemsg.Format(imagemsg.Fields{Work: "芙莉蓮", Characters: "別人改的"}, "up")
	if _, err := e.fake.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: m.ID, Channel: m.ChannelID, Content: &latest}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(15*time.Minute + time.Second)
	openForm(t, e, m, "ic") // 開新表單時清除過期記錄
	if n := e.c.formCount(); n != 1 {
		t.Errorf("過期記錄應被清除，剩 %d 筆", n)
	}
	e.c.EditSubmit(context.Background(), submit(idA, cmUser, formValues(orig)))
	got, _, _ := imagemsg.Parse(e.content(t, m))
	if got.Characters != "舊角色" {
		t.Errorf("記錄過期應全部套用：%+v", got)
	}
	_ = idB
}

// 記錄過期但尚未被清除時，送出也視為記錄遺失（全部套用）。
func TestEditSubmit_ExpiredRecordNotUsed(t *testing.T) {
	e := newCMEnv(t)
	now := time.Unix(1_000_000, 0)
	e.c.Now = func() time.Time { return now }
	orig := imagemsg.Fields{Work: "芙莉蓮", Characters: "舊角色"}
	m := e.imageMessage(orig, "up")
	id := openForm(t, e, m, "ia")
	latest := imagemsg.Format(imagemsg.Fields{Work: "芙莉蓮", Characters: "別人改的"}, "up")
	if _, err := e.fake.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: m.ID, Channel: m.ChannelID, Content: &latest}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(editFormTTL)
	e.c.EditSubmit(context.Background(), submit(id, cmUser, formValues(orig)))
	got, _, _ := imagemsg.Parse(e.content(t, m))
	if got.Characters != "舊角色" {
		t.Errorf("記錄過期應全部套用：%+v", got)
	}
}

// 送出後記錄即刪除（不累積）。
func TestEditSubmit_RecordRemovedAfterSubmit(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	id := openForm(t, e, m, "ia")
	e.c.EditSubmit(context.Background(), submit(id, cmUser, formValues(imagemsg.Fields{Work: "芙莉蓮"})))
	if n := e.c.formCount(); n != 0 {
		t.Errorf("送出後應刪除記錄，剩 %d 筆", n)
	}
}

// formCount 回傳目前保存的表單記錄數。
func (c *ContextMenu) formCount() int {
	c.formMu.Lock()
	defer c.formMu.Unlock()
	return len(c.forms)
}
