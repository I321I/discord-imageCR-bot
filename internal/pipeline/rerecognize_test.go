package pipeline

import (
	"context"
	"errors"
	"image"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/imagemsg"
	"discordbot/internal/recognize"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// 只辨識要求的欄位：只要作品時不呼叫 OCR，圖片文字留空。
func TestRecognizeAttachment_OnlyRequestedFields(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	var textCalled atomic.Bool
	e.p.Text = textFunc(func(context.Context, image.Image) (string, error) { textCalled.Store(true); return "x", nil })
	got, err := e.p.RecognizeAttachment(context.Background(), m.Attachments[0], []imagemsg.Field{imagemsg.FieldWork})
	if err != nil {
		t.Fatal(err)
	}
	if got != (imagemsg.Fields{Work: "葬送のフリーレン"}) {
		t.Errorf("得到 %+v", got)
	}
	if textCalled.Load() {
		t.Error("沒要求圖片文字時不應呼叫 OCR")
	}

	var workCalled atomic.Bool
	e.p.Work = workFunc(func(context.Context, []byte) (string, error) { workCalled.Store(true); return "x", nil })
	e.p.Text = okText()
	got, err = e.p.RecognizeAttachment(context.Background(), m.Attachments[0], []imagemsg.Field{imagemsg.FieldText})
	if err != nil {
		t.Fatal(err)
	}
	if got != (imagemsg.Fields{Text: "這就是人類啊"}) {
		t.Errorf("得到 %+v", got)
	}
	if workCalled.Load() {
		t.Error("沒要求作品時不應呼叫 trace.moe")
	}
}

// 仍然失敗的欄位為失敗標記。
func TestRecognizeAttachment_StillFails(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	e.p.Work = workFunc(func(context.Context, []byte) (string, error) { return "", recognize.ErrNotFound })
	got, err := e.p.RecognizeAttachment(context.Background(), m.Attachments[0], []imagemsg.Field{imagemsg.FieldWork, imagemsg.FieldText})
	if err != nil {
		t.Fatal(err)
	}
	if got.Work != imagemsg.FailedMarker || got.Text != "這就是人類啊" {
		t.Errorf("得到 %+v", got)
	}
}

// 重新辨識排隊優先（Q62）：辨識器收到的 ctx 帶 WithPriority 標記。
func TestRecognizeAttachment_Priority(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	var workPri, textPri atomic.Bool
	e.p.Work = workFunc(func(ctx context.Context, _ []byte) (string, error) {
		workPri.Store(recognize.IsPriority(ctx))
		return "w", nil
	})
	e.p.Text = textFunc(func(ctx context.Context, _ image.Image) (string, error) {
		textPri.Store(recognize.IsPriority(ctx))
		return "t", nil
	})
	if _, err := e.p.RecognizeAttachment(context.Background(), m.Attachments[0], []imagemsg.Field{imagemsg.FieldWork, imagemsg.FieldText}); err != nil {
		t.Fatal(err)
	}
	if !workPri.Load() || !textPri.Load() {
		t.Errorf("應以優先排隊：work=%v text=%v", workPri.Load(), textPri.Load())
	}
}

// 下載失敗、超過 25MB、長度不符 → 回錯誤（找不到原圖），不辨識。
func TestRecognizeAttachment_DownloadErrors(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	a := m.Attachments[0]
	var called atomic.Bool
	e.p.Work = workFunc(func(context.Context, []byte) (string, error) { called.Store(true); return "x", nil })
	want := []imagemsg.Field{imagemsg.FieldWork}

	e.fetch.errs[a.URL] = discordtest.ErrInjected
	if _, err := e.p.RecognizeAttachment(context.Background(), a, want); !errors.Is(err, discordtest.ErrInjected) {
		t.Errorf("下載失敗應回錯誤：%v", err)
	}
	delete(e.fetch.errs, a.URL)

	big := *a
	big.Size = MaxAttachmentSize + 1
	if _, err := e.p.RecognizeAttachment(context.Background(), &big, want); !errors.Is(err, ErrTooLarge) {
		t.Errorf("超過 25MB 應回 ErrTooLarge：%v", err)
	}
	wrong := *a
	wrong.Size = a.Size + 1
	if _, err := e.p.RecognizeAttachment(context.Background(), &wrong, want); !errors.Is(err, ErrSizeMismatch) {
		t.Errorf("長度不符應回 ErrSizeMismatch：%v", err)
	}
	if called.Load() {
		t.Error("下載失敗時不應辨識")
	}
}

func failedReaction() []*discordgo.MessageReactions {
	return []*discordgo.MessageReactions{{Count: 1, Me: true, Emoji: &discordgo.Emoji{Name: "❌"}}}
}

// 帶 ❌ 的原訊息重新處理：先移除 ❌，再走自動處理流程（⏳、發出、刪原訊息）。
func TestReprocess_RemovesFailedMarkThenProcesses(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png", "b.png")
	m.Reactions = failedReaction()
	if err := <-e.p.Reprocess(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 2 {
		t.Errorf("應發出 2 則，得到 %d", len(e.fake.Sent))
	}
	if e.originalExists(m) {
		t.Error("全部成功應刪除原訊息")
	}
	calls := e.fake.ReactionCalls
	if len(calls) == 0 || calls[0] != (discordtest.ReactionCall{Op: "remove", ChannelID: testSource, MessageID: m.ID, Emoji: "❌", UserID: "@me"}) {
		t.Errorf("第一個反應操作應為移除 ❌：%+v", calls)
	}
}

// 重新處理仍失敗：原訊息保留並重新加上 ❌。
func TestReprocess_FailureAddsMarkAgain(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	m.Reactions = failedReaction()
	e.fetch.errs[m.Attachments[0].URL] = discordtest.ErrInjected
	if err := <-e.p.Reprocess(context.Background(), testGuild, m); err == nil {
		t.Fatal("下載失敗應回錯誤")
	}
	if !e.originalExists(m) {
		t.Error("失敗不應刪除原訊息")
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) {
		t.Errorf("失敗後應只有 ❌，得到 %q", got)
	}
}

// 重新處理以優先排隊（Q62）。
func TestReprocess_Priority(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	var pri atomic.Bool
	e.p.Work = workFunc(func(ctx context.Context, _ []byte) (string, error) {
		pri.Store(recognize.IsPriority(ctx))
		return "w", nil
	})
	if err := <-e.p.Reprocess(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if !pri.Load() {
		t.Error("重新處理應以優先排隊")
	}
}

// S5：重新辨識同時只進行一個，排到才下載（不先下載再排隊）。
func TestRecognizeAttachment_OneAtATimeDownloadAfterTurn(t *testing.T) {
	e := newEnv(t)
	m1 := e.imageMessage(t, "a.png")
	m2 := e.imageMessage(t, "b.png")
	started, release := make(chan struct{}), make(chan struct{})
	var n atomic.Int32
	e.p.Work = workFunc(func(context.Context, []byte) (string, error) {
		if n.Add(1) == 1 {
			close(started)
			<-release
		}
		return "w", nil
	})
	want := []imagemsg.Field{imagemsg.FieldWork}
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		if _, err := e.p.RecognizeAttachment(context.Background(), m1.Attachments[0], want); err != nil {
			t.Error(err)
		}
	}()
	<-started
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		if _, err := e.p.RecognizeAttachment(context.Background(), m2.Attachments[0], want); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-done2:
		t.Fatal("第一個進行中時第二個不應完成")
	case <-time.After(100 * time.Millisecond):
	}
	if slices.Contains(e.fetch.fetched(), m2.Attachments[0].URL) {
		t.Error("排到之前不應下載")
	}
	close(release)
	<-done1
	<-done2
	if !slices.Contains(e.fetch.fetched(), m2.Attachments[0].URL) {
		t.Error("排到後應下載")
	}
}

// 排隊中 ctx 結束 → 回 ctx 錯誤，不下載。
func TestRecognizeAttachment_CanceledWhileWaiting(t *testing.T) {
	e := newEnv(t)
	m1 := e.imageMessage(t, "a.png")
	m2 := e.imageMessage(t, "b.png")
	started, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	e.p.Work = workFunc(func(context.Context, []byte) (string, error) {
		if once.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
		return "w", nil
	})
	want := []imagemsg.Field{imagemsg.FieldWork}
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		_, _ = e.p.RecognizeAttachment(context.Background(), m1.Attachments[0], want)
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := e.p.RecognizeAttachment(ctx, m2.Attachments[0], want); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("排隊中逾時應回 ctx 錯誤：%v", err)
	}
	if slices.Contains(e.fetch.fetched(), m2.Attachments[0].URL) {
		t.Error("沒排到不應下載")
	}
	close(release)
	<-done1
}

// S4：已在佇列中（或處理中）→ Reprocess 回 ErrAlreadyQueued，不重複處理。
func TestReprocess_AlreadyQueued(t *testing.T) {
	e := newEnv(t)
	text, started, release := blockingText()
	e.p.Text = text
	m := e.imageMessage(t, "a.png")
	first := e.p.Enqueue(context.Background(), testGuild, m)
	<-started
	if err := <-e.p.Reprocess(context.Background(), testGuild, m); !errors.Is(err, ErrAlreadyQueued) {
		t.Errorf("已在佇列中應回 ErrAlreadyQueued：%v", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 1 {
		t.Errorf("只應處理一次，Sent %d", len(e.fake.Sent))
	}
}

// S4：輪到時頻道已非來源 → Reprocess 回 ErrNotSource（一般 Enqueue 仍回 nil）。
func TestReprocess_ChannelNoLongerSource(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	if err := e.p.Store.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel(testGuild, testSource); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := <-e.p.Reprocess(context.Background(), testGuild, m); !errors.Is(err, ErrNotSource) {
		t.Errorf("頻道已非來源應回 ErrNotSource：%v", err)
	}
	for _, sent := range e.fake.Sent {
		if sent.ChannelID == testSource {
			t.Error("不應處理")
		}
	}
}
