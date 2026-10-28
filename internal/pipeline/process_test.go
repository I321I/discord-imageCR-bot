package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/imagemsg"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// fakeFetcher 依 URL 回傳內容或錯誤，並記錄下載過的 URL。
type fakeFetcher struct {
	mu   sync.Mutex
	data map[string][]byte
	errs map[string]error
	got  []string
}

func (f *fakeFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, url)
	if err := f.errs[url]; err != nil {
		return nil, err
	}
	d, ok := f.data[url]
	if !ok {
		return nil, fmt.Errorf("沒有 %s", url)
	}
	return d, nil
}

func (f *fakeFetcher) fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

// fakeIndex 記錄收到的圖片訊息。
type fakeIndex struct {
	mu   sync.Mutex
	msgs []*discordgo.Message
	gids []string
}

func (x *fakeIndex) PutImageMessage(guildID string, m *discordgo.Message) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.gids = append(x.gids, guildID)
	x.msgs = append(x.msgs, m)
}

func (x *fakeIndex) count() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.msgs)
}

// failNthSend 在第 n 次（從 1 起算）ChannelMessageSendComplex 回錯誤，其餘交給 Fake。
type failNthSend struct {
	*discordtest.Fake
	mu    sync.Mutex
	calls int
	n     int
}

func (f *failNthSend) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	f.calls++
	fail := f.calls == f.n
	f.mu.Unlock()
	if fail {
		return nil, discordtest.ErrInjected
	}
	return f.Fake.ChannelMessageSendComplex(channelID, data, options...)
}

const (
	testGuild   = "g1"
	testSource  = "src"
	testUser    = "u1"
	testContent = "芙莉蓮,費倫"
)

type env struct {
	fake  *discordtest.Fake
	fetch *fakeFetcher
	index *fakeIndex
	p     *Pipeline
}

// newEnv 建立以假物件組成的 Pipeline：來源頻道 testSource（伺服器 testGuild），辨識結果固定。
func newEnv(t *testing.T) *env {
	t.Helper()
	fake := discordtest.NewFake()
	st := store.New(fake, "storage")
	st.UseEmpty()
	if err := st.Update(context.Background(), func(d *store.Data) error {
		d.AddChannel(testGuild, testSource, "1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 清掉寫入儲存頻道的紀錄，之後的 Sent 只有圖片訊息。
	fake.Sent = nil
	fake.Calls = map[string]int{}
	e := &env{fake: fake, fetch: &fakeFetcher{data: map[string][]byte{}, errs: map[string]error{}}, index: &fakeIndex{}}
	e.p = &Pipeline{API: fake, Fetch: e.fetch, Work: okWork(""), Text: okText(), Store: st, Index: e.index}
	return e
}

// imageMessage 在來源頻道放一則使用者訊息，附件為 names 的 PNG（每張內容不同）。
func (e *env) imageMessage(t *testing.T, names ...string) *discordgo.Message {
	t.Helper()
	m := &discordgo.Message{ChannelID: testSource, GuildID: testGuild, Author: &discordgo.User{ID: testUser}, Content: testContent}
	for i, name := range names {
		url := "https://cdn.test/" + name
		data := pngBytes(t, 64+i, 36)
		e.fetch.data[url] = data
		m.Attachments = append(m.Attachments, &discordgo.MessageAttachment{
			ID: fmt.Sprintf("a%d", i), Filename: name, ContentType: "image/png", URL: url, Size: len(data),
		})
	}
	return e.fake.AddMessage(m)
}

func (e *env) originalExists(m *discordgo.Message) bool {
	_, err := e.fake.ChannelMessage(m.ChannelID, m.ID)
	return err == nil
}

func TestProcess_TwoImages(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png", "b.webp")
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatalf("Process 回錯誤：%v", err)
	}
	if len(e.fake.Sent) != 2 {
		t.Fatalf("應發出 2 則，得到 %d", len(e.fake.Sent))
	}
	want := imagemsg.Format(imagemsg.Fields{Work: "葬送のフリーレン", Text: "這就是人類啊", Keywords: "芙莉蓮、費倫"}, testUser)
	for i, s := range e.fake.Sent {
		a := m.Attachments[i]
		if s.ChannelID != testSource {
			t.Errorf("第 %d 則發到 %s，預期來源頻道", i+1, s.ChannelID)
		}
		if s.Data.Content != want {
			t.Errorf("第 %d 則內容：\n%s\n預期：\n%s", i+1, s.Data.Content, want)
		}
		if len(s.Data.Files) != 1 || s.Data.Files[0].Name != a.Filename || s.Data.Files[0].ContentType != a.ContentType {
			t.Errorf("第 %d 則附件應為原檔名與類型 %s：%+v", i+1, a.Filename, s.Data.Files)
		}
		if !bytes.Equal(s.Files[0], e.fetch.data[a.URL]) {
			t.Errorf("第 %d 則附件 bytes 與原檔不同", i+1)
		}
		// Q20：上傳者 mention 不通知。
		if am := s.Data.AllowedMentions; am == nil || am.Parse == nil || len(am.Parse) != 0 || len(am.Users) != 0 || len(am.Roles) != 0 || am.RepliedUser {
			t.Errorf("第 %d 則 AllowedMentions 應不允許任何通知：%+v", i+1, am)
		}
	}
	if e.originalExists(m) || len(e.fake.Deleted) != 1 {
		t.Errorf("全部發完應刪除原訊息，Deleted %+v", e.fake.Deleted)
	}
	if e.index.count() != 2 || e.index.gids[0] != testGuild {
		t.Errorf("索引應收到 2 則（伺服器 %s），得到 %d %v", testGuild, e.index.count(), e.index.gids)
	}
	// Q19：處理中加 ⏳。
	if len(e.fake.ReactionCalls) == 0 || e.fake.ReactionCalls[0] != (discordtest.ReactionCall{Op: "add", ChannelID: testSource, MessageID: m.ID, Emoji: "⏳"}) {
		t.Errorf("處理開始時應在原訊息加 ⏳：%+v", e.fake.ReactionCalls)
	}
	if slices.Contains(e.fake.ReactionsOn(testSource, m.ID), "❌") {
		t.Error("成功時不應加 ❌")
	}
}

// Q21：換行與分隔符號轉「、」；沒有文字時關鍵字為空。
func TestProcess_KeywordsFromContent(t *testing.T) {
	cases := []struct{ content, want string }{
		{"第一行\n第二行，第三", "第一行、第二行、第三"},
		{"", ""},
		{"<@123> 看這個", "<@123> 看這個"},
	}
	for _, c := range cases {
		e := newEnv(t)
		m := e.imageMessage(t, "a.png")
		m.Content = c.content
		if err := e.p.Process(context.Background(), testGuild, m); err != nil {
			t.Fatal(err)
		}
		f, _, ok := imagemsg.Parse(e.fake.Sent[0].Data.Content)
		if !ok || f.Keywords != c.want {
			t.Errorf("原文 %q：關鍵字 %q，預期 %q", c.content, f.Keywords, c.want)
		}
	}
}

// 全部辨識完成才發：第二張辨識被阻塞時沒有任何 Send。
func TestProcess_AllRecognizedBeforeSend(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png", "b.png")
	var mu sync.Mutex
	calls := 0
	block, entered := make(chan struct{}), make(chan struct{})
	e.p.Work = workFunc(func(context.Context, []byte) (string, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 2 {
			close(entered)
			<-block
		}
		return "作品", nil
	})
	done := make(chan error, 1)
	go func() { done <- e.p.Process(context.Background(), testGuild, m) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("第二張沒有開始辨識")
	}
	time.Sleep(50 * time.Millisecond)
	if n := e.fake.CallCount("ChannelMessageSendComplex"); n != 0 {
		t.Errorf("第二張辨識完成前不應發出任何圖片訊息，已發 %d", n)
	}
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 2 {
		t.Errorf("辨識完成後應發出 2 則，得到 %d", len(e.fake.Sent))
	}
}

// 辨識失敗不算處理失敗：欄位標失敗，圖片照常發出並刪原訊息。
func TestProcess_RecognitionFailureStillSends(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	e.p.Work = workFunc(func(context.Context, []byte) (string, error) { return "", errors.New("quota") })
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	f, _, _ := imagemsg.Parse(e.fake.Sent[0].Data.Content)
	if f.Work != imagemsg.FailedMarker {
		t.Errorf("作品應為失敗標記，得到 %q", f.Work)
	}
	if e.originalExists(m) {
		t.Error("辨識失敗仍應刪除原訊息")
	}
}

// Q24：第 2 張發送失敗 → 已發出的保留、其餘照發、原訊息不刪、加 ❌（移除 ⏳）。
func TestProcess_SendFailure_KeepsOriginal(t *testing.T) {
	e := newEnv(t)
	api := &failNthSend{Fake: e.fake, n: 2}
	e.p.API = api
	m := e.imageMessage(t, "a.png", "b.png", "c.png")
	if err := e.p.Process(context.Background(), testGuild, m); err == nil {
		t.Error("有圖片發送失敗時 Process 應回錯誤")
	}
	if !e.originalExists(m) || len(e.fake.Deleted) != 0 {
		t.Errorf("發送失敗時不可刪除任何訊息，Deleted %+v", e.fake.Deleted)
	}
	if len(e.fake.Sent) != 2 || e.fake.Sent[0].Data.Files[0].Name != "a.png" || e.fake.Sent[1].Data.Files[0].Name != "c.png" {
		t.Errorf("第 1、3 張應照常發出並保留：%d 則", len(e.fake.Sent))
	}
	if e.index.count() != 2 {
		t.Errorf("成功發出的 2 則應進索引，得到 %d", e.index.count())
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) {
		t.Errorf("原訊息反應應只剩 ❌，得到 %q", got)
	}
}

// 任一張下載失敗：不發出任何圖片訊息、原訊息不刪、加 ❌。
func TestProcess_DownloadFailure_KeepsOriginal(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png", "b.png")
	e.fetch.errs[m.Attachments[1].URL] = errors.New("cdn 掛了")
	if err := e.p.Process(context.Background(), testGuild, m); err == nil {
		t.Error("下載失敗時 Process 應回錯誤")
	}
	if len(e.fake.Sent) != 0 {
		t.Errorf("下載失敗時不應發出圖片訊息，得到 %d 則", len(e.fake.Sent))
	}
	if !e.originalExists(m) {
		t.Error("下載失敗時原訊息不可刪除")
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) {
		t.Errorf("原訊息反應應只剩 ❌，得到 %q", got)
	}
}

// 附件 > 25MB：不下載，直接視為失敗（不發、不刪、加 ❌）。
func TestProcess_AttachmentTooLarge(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png", "big.png")
	m.Attachments[1].Size = MaxAttachmentSize + 1
	if err := e.p.Process(context.Background(), testGuild, m); err == nil {
		t.Error("附件過大時 Process 應回錯誤")
	}
	if slices.Contains(e.fetch.fetched(), m.Attachments[1].URL) {
		t.Error("超過 25MB 的附件不應下載")
	}
	if len(e.fake.Sent) != 0 || !e.originalExists(m) {
		t.Errorf("附件過大時不發不刪：Sent %d", len(e.fake.Sent))
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) {
		t.Errorf("原訊息反應應只剩 ❌，得到 %q", got)
	}
}

// 下載不完整（10/28 Task 1 起由 Fetcher 以 Content-Length 判斷，回 ErrSizeMismatch）：視為下載失敗，不發、不刪、加 ❌。
func TestProcess_DownloadSizeMismatch(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png", "b.png")
	url := m.Attachments[1].URL
	e.fetch.errs[url] = ErrSizeMismatch
	if err := e.p.Process(context.Background(), testGuild, m); !errors.Is(err, ErrSizeMismatch) {
		t.Errorf("下載不完整時 Process 應回 ErrSizeMismatch：%v", err)
	}
	if len(e.fake.Sent) != 0 || !e.originalExists(m) {
		t.Errorf("大小不符時不發不刪：Sent %d", len(e.fake.Sent))
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) {
		t.Errorf("原訊息反應應只剩 ❌，得到 %q", got)
	}
}

// 10/28 Task 1：附件 Size 與實際下載長度不符（Pi 實測 219770 vs 315747）不再視為失敗，照常處理。
// （原 TestProcess_DownloadSizeMismatch 以截短資料模擬 Size 不符，依新決定改為上方的 Fetcher 錯誤。）
func TestProcess_AttachmentSizeDiffersStillProcessed(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png", "b.png")
	m.Attachments[1].Size = 219770
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatalf("Size 與實際不符仍應成功：%v", err)
	}
	if len(e.fake.Sent) != 2 || e.originalExists(m) {
		t.Errorf("應發出 2 則並刪除原訊息：Sent %d", len(e.fake.Sent))
	}
}

// 剛好 25MB 仍處理（PNG 後面補到 25MiB，解碼器忽略 IEND 之後的資料）。
func TestProcess_AttachmentExactly25MB(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	url := m.Attachments[0].URL
	padded := make([]byte, MaxAttachmentSize)
	copy(padded, e.fetch.data[url])
	e.fetch.data[url] = padded
	m.Attachments[0].Size = MaxAttachmentSize
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 1 {
		t.Errorf("剛好 25MB 應處理，Sent %d", len(e.fake.Sent))
	}
}

// Q25：刪除原訊息失敗 → 只記 log，原訊息加 ❌；已發出的圖片訊息保留。
func TestProcess_DeleteFails(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	e.fake.FailOn["ChannelMessageDelete"] = discordtest.ErrInjected
	if err := e.p.Process(context.Background(), testGuild, m); err == nil {
		t.Error("刪除失敗時 Process 應回錯誤")
	}
	if len(e.fake.Sent) != 1 || e.index.count() != 1 {
		t.Errorf("圖片訊息應已發出並進索引：Sent %d、索引 %d", len(e.fake.Sent), e.index.count())
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) {
		t.Errorf("原訊息反應應只剩 ❌，得到 %q", got)
	}
}

// unknownMessage 是 Discord 回的 Unknown Message（404／10008），同 discordtest 的 notFound。
func unknownMessage() error {
	return &discordgo.RESTError{
		Response: &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found"},
		Message:  &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage, Message: "Unknown Message"},
	}
}

// 10/28 Task 2：刪除原訊息遇 Unknown Message（10008，原訊息已不在）→ 視為刪除成功：不回錯誤、不加 ❌。
func TestProcess_OriginalAlreadyDeleted_IsSuccess(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	e.fake.FailOn["ChannelMessageDelete"] = unknownMessage()
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Errorf("原訊息已刪除應視為成功：%v", err)
	}
	if len(e.fake.Sent) != 1 || e.index.count() != 1 {
		t.Errorf("圖片訊息應已發出並進索引：Sent %d、索引 %d", len(e.fake.Sent), e.index.count())
	}
	// 成功路徑本來就不移除 ⏳（原訊息已不在）；fake 以 FailOn 模擬時訊息仍在，所以只剩 ⏳、沒有 ❌。
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"⏳"}) {
		t.Errorf("不應加 ❌，得到 %q", got)
	}
	// PM 決定：記一行 log。
	if want := "原訊息 " + m.ChannelID + "/" + m.ID + " 已不存在（10008），視為已刪除"; !strings.Contains(buf.String(), want) {
		t.Errorf("log 應含 %q：%s", want, buf.String())
	}
}

// 10/28 Task 2：只放寬 10008；其他刪除錯誤（403／50013、404 但非 10008）仍是失敗、加 ❌。
func TestProcess_DeleteForbiddenStillFails(t *testing.T) {
	cases := map[string]error{
		"403 50013": &discordgo.RESTError{
			Response: &http.Response{StatusCode: http.StatusForbidden, Status: "403 Forbidden"},
			Message:  &discordgo.APIErrorMessage{Code: discordgo.ErrCodeMissingPermissions, Message: "Missing Permissions"},
		},
		"404 10003": &discordgo.RESTError{
			Response: &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found"},
			Message:  &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownChannel, Message: "Unknown Channel"},
		},
	}
	for name, injected := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			m := e.imageMessage(t, "a.png")
			e.fake.FailOn["ChannelMessageDelete"] = injected
			if err := e.p.Process(context.Background(), testGuild, m); err == nil {
				t.Error("刪除失敗時 Process 應回錯誤")
			}
			if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) {
				t.Errorf("原訊息反應應只剩 ❌，得到 %q", got)
			}
		})
	}
}

// 10/28 Task 2：原訊息在處理中被刪除，移除 ⏳／加 ❌ 遇 10008 只記一行短 log（不含完整錯誤）。
func TestRun_ReactOnDeletedMessage_NoError(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	e.fetch.errs[m.Attachments[0].URL] = discordtest.ErrInjected
	if err := e.fake.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Process(context.Background(), testGuild, m); !errors.Is(err, discordtest.ErrInjected) {
		t.Errorf("應回下載錯誤：%v", err)
	}
	for _, c := range e.fake.ReactionCalls {
		if c.MessageID == m.ID {
			t.Errorf("不存在的訊息不應有成功的反應：%+v", c)
		}
	}
	out := buf.String()
	if strings.Contains(out, "Unknown Message") || strings.Contains(out, "10008") {
		t.Errorf("10008 不應記完整錯誤：%s", out)
	}
	for _, want := range []string{"訊息 " + m.ChannelID + "/" + m.ID + " 已不存在，略過加 ❌", "訊息 " + m.ChannelID + "/" + m.ID + " 已不存在，略過移除 ⏳"} {
		if !strings.Contains(out, want) {
			t.Errorf("log 應含 %q：%s", want, out)
		}
	}
}

// 加 ⏳ 失敗（例如缺新增反應權限）不影響處理。
func TestProcess_ReactionFailureDoesNotStop(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	e.fake.FailOn["MessageReactionAdd"] = discordtest.ErrInjected
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 1 || e.originalExists(m) {
		t.Errorf("加反應失敗仍應完成處理：Sent %d", len(e.fake.Sent))
	}
}

// 辨識期間 ctx 結束（bot 關機）：不發出、不刪除、不加 ❌（移除 ⏳），下次啟動補處理。
func TestProcess_ContextCanceledDoesNotSend(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	ctx, cancel := context.WithCancel(context.Background())
	e.p.Text = textFunc(func(ctx context.Context, _ image.Image) (string, error) {
		cancel()
		return "", ctx.Err()
	})
	if err := e.p.Process(ctx, testGuild, m); !errors.Is(err, context.Canceled) {
		t.Errorf("應回 context.Canceled，得到 %v", err)
	}
	if len(e.fake.Sent) != 0 || !e.originalExists(m) {
		t.Errorf("關機中不應發出或刪除：Sent %d", len(e.fake.Sent))
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); len(got) != 0 {
		t.Errorf("關機中斷時應移除 ⏳ 且不加 ❌，得到 %q", got)
	}
}

func TestHandleMessage_ProcessesSourceImage(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.gif")
	m.Attachments[0].ContentType = "image/gif"
	e.p.HandleMessage(context.Background(), m)
	if len(e.fake.Sent) != 1 || e.originalExists(m) {
		t.Errorf("來源頻道的圖片訊息應被處理：Sent %d", len(e.fake.Sent))
	}
	if e.index.gids[0] != testGuild {
		t.Errorf("伺服器 ID 應取自設定：%v", e.index.gids)
	}
}

func TestHandleMessage_Ignores(t *testing.T) {
	cases := []struct {
		name   string
		modify func(m *discordgo.Message)
	}{
		{"非來源頻道", func(m *discordgo.Message) { m.ChannelID = "other" }},
		{"純文字", func(m *discordgo.Message) { m.Attachments = nil }},
		{"含影片附件", func(m *discordgo.Message) {
			m.Attachments = append(m.Attachments, &discordgo.MessageAttachment{Filename: "v.mp4", ContentType: "video/mp4", URL: "https://cdn.test/v.mp4"})
		}},
		{"bot 自己", func(m *discordgo.Message) { m.Author = &discordgo.User{ID: "bot", Bot: true} }},
		{"其他 bot", func(m *discordgo.Message) { m.Author = &discordgo.User{ID: "otherbot", Bot: true} }},
		{"webhook", func(m *discordgo.Message) { m.WebhookID = "wh" }},
		{"只有 embed", func(m *discordgo.Message) {
			m.Attachments = nil
			m.Embeds = []*discordgo.MessageEmbed{{Type: discordgo.EmbedTypeImage, URL: "https://example.com/a.png"}}
		}},
		{"沒有作者", func(m *discordgo.Message) { m.Author = nil }},
	}
	for _, c := range cases {
		e := newEnv(t)
		m := e.imageMessage(t, "a.png")
		c.modify(m)
		e.p.HandleMessage(context.Background(), m)
		if len(e.fake.Sent) != 0 || len(e.fake.Deleted) != 0 || len(e.fake.ReactionCalls) != 0 || len(e.fetch.fetched()) != 0 {
			t.Errorf("%s：應忽略，Sent %d、Deleted %d、反應 %d、下載 %d", c.name, len(e.fake.Sent), len(e.fake.Deleted), len(e.fake.ReactionCalls), len(e.fetch.fetched()))
		}
	}
}

// 多則訊息同時處理（-race）：每則各自完成。
func TestHandleMessage_Concurrent(t *testing.T) {
	e := newEnv(t)
	var msgs []*discordgo.Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs, e.imageMessage(t, fmt.Sprintf("m%d.png", i)))
	}
	var wg sync.WaitGroup
	for _, m := range msgs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.p.HandleMessage(context.Background(), m)
		}()
	}
	wg.Wait()
	if len(e.fake.Sent) != 5 || len(e.fake.Deleted) != 5 {
		t.Errorf("Sent %d、Deleted %d，預期各 5", len(e.fake.Sent), len(e.fake.Deleted))
	}
}
