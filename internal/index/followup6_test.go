package index

import (
	"bytes"
	"context"
	"log"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/imagemsg"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// 階段 6 後決定 2：設定時間點之前就帶 bot ❌ 的訊息（頻道移除後重新加入）照一般舊圖片訊息收錄，不補處理。
func TestScan_FailedOriginalBeforeSetAtIndexedAsLegacy(t *testing.T) {
	f := discordtest.NewFake()
	m := addUserImage(f, "c", "100", "失敗過的舊圖")
	m.Reactions = failedReaction()
	x, bf := New(), &fakeBackfill{}
	if err := ScanChannel(context.Background(), f, x, bf, botID, "g", "c", "200"); err != nil {
		t.Fatal(err)
	}
	e, ok := x.Get(Key{ChannelID: "c", MessageID: "100"})
	if !ok || e.Kind != KindLegacy || e.LegacyText != "失敗過的舊圖" {
		t.Errorf("應收錄為舊圖片訊息：%+v（%v）", e, ok)
	}
	if got := bf.got(); len(got) != 0 {
		t.Errorf("不應補處理：%v", got)
	}
}

// 階段 6 後決定 3：搜尋不比對「⚠️ 辨識失敗」（任一部分、各欄位皆然），其他欄位照常比對。
func TestSearch_FailedMarkerNeverMatchedAnyPart(t *testing.T) {
	x := New()
	x.PutImageMessage("g", imageMsg("c", "1", imagemsg.Fields{Work: imagemsg.FailedMarker, Text: imagemsg.FailedMarker, Characters: "芙莉蓮"}, "u"))
	for _, q := range []string{"⚠️", "辨識", "失敗", imagemsg.FailedMarker, "芙莉蓮 失敗"} {
		if got := x.Search(q, allow("c"), 25); len(got) != 0 {
			t.Errorf("查詢 %q 不應比對到失敗標記：%v", q, keys(got))
		}
	}
	if got := x.Search("芙莉蓮", allow("c"), 25); len(got) != 1 {
		t.Errorf("其他欄位應照常比對：%v", keys(got))
	}
}

// 階段 6 後決定 4：多詞搜尋的排序取任一詞命中欄位中等級最高者。
// 甲：「芙莉蓮」命中角色（主欄位）、「魔法」只命中圖片文字 → 主欄位等級；
// 乙：兩詞都只命中圖片文字 → 圖片文字等級。甲較舊仍排在乙之前。
func TestSearch_MultiTermRankTakesBestField(t *testing.T) {
	x := New()
	x.PutImageMessage("g", imageMsg("c", "10", imagemsg.Fields{Characters: "芙莉蓮", Text: "魔法"}, "u"))
	x.PutImageMessage("g", imageMsg("c", "20", imagemsg.Fields{Text: "芙莉蓮的魔法"}, "u"))
	x.PutLegacy("g", legacyMsg("c", "30", "芙莉蓮 魔法", 1))
	want := []string{"c:10:0", "c:20:0", "c:30:0"}
	if got := keys(x.Search("魔法 芙莉蓮", allow("c"), 25)); !slices.Equal(got, want) {
		t.Errorf("得到 %v，預期 %v", got, want)
	}
}

// captureLog 把標準 log 導到 buffer，測試結束時還原。
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	b := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(prev) })
	return b
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// 階段 6 後決定 7：setAt 不合法（空字串、非數字）→ 該頻道使用者圖片一律當舊圖片訊息收錄，不補處理，記 log。
func TestScan_InvalidSetAtAllLegacy(t *testing.T) {
	for _, setAt := range []string{"", "abc", "1000x"} {
		t.Run(setAt, func(t *testing.T) {
			logs := captureLog(t)
			f := discordtest.NewFake()
			addUserImage(f, "c", "999", "甲")
			addUserImage(f, "c", "5000", "乙")
			failed := addUserImage(f, "c", "6000", "丙")
			failed.Reactions = failedReaction()
			addBotImage(f, "c", "7000", imagemsg.Fields{Work: "丁"})
			x, bf := New(), &fakeBackfill{}
			if err := ScanChannel(context.Background(), f, x, bf, botID, "g", "c", setAt); err != nil {
				t.Fatal(err)
			}
			if got := bf.got(); len(got) != 0 {
				t.Errorf("不應補處理：%v", got)
			}
			for _, id := range []string{"999", "5000", "6000"} {
				if e, ok := x.Get(Key{ChannelID: "c", MessageID: id}); !ok || e.Kind != KindLegacy {
					t.Errorf("%s 應收錄為舊圖片訊息：%+v（%v）", id, e, ok)
				}
			}
			if _, ok := x.Get(Key{ChannelID: "c", MessageID: "7000"}); !ok {
				t.Error("bot 的圖片訊息照常收錄")
			}
			if !strings.Contains(logs.String(), "c") || !strings.Contains(logs.String(), "設定時間點") {
				t.Errorf("應記 log，得到 %q", logs.String())
			}
		})
	}
}

// 決定 7：setAt 不合法時，使用者訊息的編輯也依舊圖片訊息更新。
func TestOnMessageUpdate_InvalidSetAtTreatedAsLegacy(t *testing.T) {
	_, st, x, _, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "")
	h.MessageUpdated(legacyMsg("c", "5000", "新文字", 1))
	if e, ok := x.Get(Key{ChannelID: "c", MessageID: "5000"}); !ok || e.LegacyText != "新文字" {
		t.Errorf("應依舊圖片訊息收錄：%+v（%v）", e, ok)
	}
}

// ctxRecordingAPI 套用每次 ChannelMessages 收到的 RequestOption，記錄請求最後帶的 context。
type ctxRecordingAPI struct {
	*discordtest.Fake
	mu   sync.Mutex
	ctxs []context.Context
}

func (a *ctxRecordingAPI) ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	cfg := &discordgo.RequestConfig{Request: httptest.NewRequest("GET", "/", nil)}
	for _, o := range options {
		o(cfg)
	}
	a.mu.Lock()
	a.ctxs = append(a.ctxs, cfg.Request.Context())
	a.mu.Unlock()
	return a.Fake.ChannelMessages(channelID, limit, beforeID, afterID, aroundID)
}

// PM 決定 (c)：掃描讀取歷史時傳入 discordgo.WithContext(ctx)，掃描取消時進行中的請求也會中止。
func TestScan_ChannelMessagesUsesContext(t *testing.T) {
	f := discordtest.NewFake()
	for i := 1; i <= 150; i++ {
		addUserImage(f, "c", strconv.Itoa(1000+i), "文字")
	}
	api := &ctxRecordingAPI{Fake: f}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := ScanChannel(ctx, api, New(), &fakeBackfill{}, botID, "g", "c", "1"); err != nil {
		t.Fatal(err)
	}
	if len(api.ctxs) < 2 {
		t.Fatalf("應分頁讀取，得到 %d 次", len(api.ctxs))
	}
	cancel()
	for i, c := range api.ctxs {
		if c.Err() == nil {
			t.Errorf("第 %d 次請求沒有帶掃描的 ctx", i+1)
		}
	}
}

// PM 決定 (a)：遲到的 ChannelRemoved（頻道已被重新加入、仍是來源）不清除索引、不停止重新加入後的掃描。
func TestHooks_LateChannelRemovedKeepsReaddedChannel(t *testing.T) {
	f, st, x, bf, h := newHooksEnv(t)
	for i := 1; i <= 150; i++ {
		addUserImage(f, "c", strconv.Itoa(1000+i), "")
	}
	addSource(t, st, "g", "c", "1")
	x.PutLegacy("g", legacyMsg("c", "5", "舊文字", 1))
	api := &blockSecondPage{Fake: f, entered: make(chan struct{}), release: make(chan struct{})}
	h.api = api
	h.ChannelAdded("g", "c")
	<-api.entered

	removed := make(chan struct{})
	go func() {
		h.ChannelRemoved("c")
		close(removed)
	}()
	select {
	case <-removed:
	case <-time.After(time.Second):
		t.Fatal("頻道仍是來源時 ChannelRemoved 不應等待（停止）進行中的掃描")
	}
	close(api.release)
	h.Wait()
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "5"}); !ok {
		t.Error("頻道仍是來源，索引不應被清除")
	}
	if n := len(bf.got()); n != 150 {
		t.Errorf("重新加入後的掃描不應被取消，補處理 %d 則", n)
	}
}

// PM 決定 (a)：ChannelRemoved 在取得 putMu 之後才確認頻道已非來源；
// 等待 putMu 期間頻道被重新加入時不清除。
func TestHooks_ChannelRemovedRechecksAfterPutMu(t *testing.T) {
	_, st, x, _, h := newHooksEnv(t)
	x.PutLegacy("g", legacyMsg("c", "5", "舊文字", 1))
	h.putMu.Lock()
	done := make(chan struct{})
	go func() {
		h.ChannelRemoved("c") // 此時 c 不是來源
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	addSource(t, st, "g", "c", "1") // 等待 putMu 期間重新加入
	h.putMu.Unlock()
	<-done
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "5"}); !ok {
		t.Error("取得 putMu 後頻道已是來源，不應清除")
	}
}

// PM 決定 (b)：掃描（ChannelAdded）的索引寫入經過來源確認：掃描中頻道已從設定移除、
// ChannelRemoved 還沒到時，之後讀到的頁不寫入索引。
func TestHooks_ScanWritesSkipRemovedChannel(t *testing.T) {
	f, st, x, _, h := newHooksEnv(t)
	for i := 1; i <= 150; i++ {
		addUserImage(f, "c", strconv.Itoa(1000+i), "文字")
	}
	addSource(t, st, "g", "c", "9999")
	api := &blockSecondPage{Fake: f, entered: make(chan struct{}), release: make(chan struct{})}
	h.api = api
	h.ChannelAdded("g", "c")
	<-api.entered
	if err := st.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel("g", "c"); return nil }); err != nil {
		t.Fatal(err)
	}
	close(api.release)
	h.Wait()
	if n := len(x.Search("", allow("c"), 0)); n != 100 {
		t.Errorf("頻道移除後讀到的頁不應寫入，索引 %d 筆（預期只有第一頁 100 筆）", n)
	}
}

// PM 決定 (b)：MessageUpdated 的索引寫入在 putMu 下確認來源（同 PutImageMessage）。
func TestHooks_MessageUpdatedWritesUnderPutMu(t *testing.T) {
	_, st, x, _, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "200")
	h.putMu.Lock()
	done := make(chan struct{})
	go func() {
		h.MessageUpdated(legacyMsg("c", "100", "新文字", 1))
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "100"}); ok {
		t.Error("持有 putMu 期間不應寫入")
	}
	if err := st.Update(context.Background(), func(d *store.Data) error { d.RemoveChannel("g", "c"); return nil }); err != nil {
		t.Fatal(err)
	}
	h.putMu.Unlock()
	<-done
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "100"}); ok {
		t.Error("取得 putMu 後頻道已非來源，不應寫入")
	}
}

// PM 決定 (d)：MESSAGE_UPDATE 帶文字但 Attachments 為 nil → 保留既有附件，只更新舊圖片訊息的文字。
func TestOnMessageUpdate_NilAttachmentsUpdatesLegacyText(t *testing.T) {
	_, st, x, _, h := newHooksEnv(t)
	addSource(t, st, "g", "c", "200")
	x.PutLegacy("g", legacyMsg("c", "100", "舊文字", 2))
	h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: "新文字", Author: &discordgo.User{ID: "u1"}})
	for i := 0; i < 2; i++ {
		e, ok := x.Get(Key{ChannelID: "c", MessageID: "100", Attachment: i})
		if !ok || e.LegacyText != "新文字" || e.AttachmentCount != 2 || e.Kind != KindLegacy {
			t.Errorf("第 %d 張應保留並更新文字：%+v（%v）", i, e, ok)
		}
	}
	// 沒有文字也沒有附件資訊：無法判斷，不動。
	h.MessageUpdated(&discordgo.Message{ID: "100", ChannelID: "c", Content: "", Author: &discordgo.User{ID: "u1"}})
	if e, ok := x.Get(Key{ChannelID: "c", MessageID: "100", Attachment: 1}); !ok || e.LegacyText != "新文字" {
		t.Errorf("沒有文字也沒有附件資訊時不應變動：%+v（%v）", e, ok)
	}
	// 設定時間點之後（不是舊圖片訊息）的訊息不受影響。
	h.MessageUpdated(&discordgo.Message{ID: "300", ChannelID: "c", Content: "新訊息", Author: &discordgo.User{ID: "u1"}})
	if _, ok := x.Get(Key{ChannelID: "c", MessageID: "300"}); ok {
		t.Error("設定時間點之後的訊息不應收錄")
	}
}
