package commands

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/imagemsg"

	"github.com/bwmarrin/discordgo"
)

// hookAPI 在第一次 ChannelMessage（重新取得最新內容）回傳前執行 hook，用來在 read-modify-write 的時間窗內插入另一個操作。
type hookAPI struct {
	*discordtest.Fake
	fired atomic.Bool
	hook  func()
}

func (h *hookAPI) ChannelMessage(channelID, messageID string, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	m, err := h.Fake.ChannelMessage(channelID, messageID, options...)
	if h.hook != nil && h.fired.CompareAndSwap(false, true) {
		h.hook()
	}
	return m, err
}

// runInWindow 在背景執行 fn，最多等 300ms（若實作加了 per-message 鎖，fn 會被擋住，等時間窗結束後才繼續）。
func runInWindow(wg *sync.WaitGroup, fn func()) {
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
	}
}

// 把關重點 2：重新辨識取得最新內容後、寫回前，使用者送出了編輯表單（改角色）。
// 預期兩邊的修改都保留：作品 = 辨識結果、角色 = 使用者的值。
func TestGK8_EditSubmitDuringRerecognizeWindow_KeepsBoth(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker, Characters: "舊角色"}, "up")
	e.pipe.recognize = func(*discordgo.MessageAttachment, []imagemsg.Field) (imagemsg.Fields, error) {
		return imagemsg.Fields{Work: "辨識的作品"}, nil
	}
	api := &hookAPI{Fake: e.fake}
	e.c.API = api
	var wg sync.WaitGroup
	api.hook = func() {
		runInWindow(&wg, func() {
			e.c.EditSubmit(context.Background(), submit(editID(m), cmUser,
				map[string]string{"work": imagemsg.FailedMarker, "text": "", "characters": "新角色", "keywords": ""}))
		})
	}
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	wg.Wait()
	got, _, _ := imagemsg.Parse(e.content(t, m))
	if got.Work != "辨識的作品" || got.Characters != "新角色" {
		t.Errorf("兩邊的修改應都保留，得到 %+v", got)
	}
}

// 把關重點 2：編輯表單取得最新內容後、寫回前，重新辨識完成並寫入作品。
// 使用者沒改作品（仍是失敗標記）→ 預期保留辨識結果，角色為使用者的值。
func TestGK8_RerecognizeDuringEditSubmitWindow_KeepsBoth(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker, Characters: "舊角色"}, "up")
	e.pipe.recognize = func(*discordgo.MessageAttachment, []imagemsg.Field) (imagemsg.Fields, error) {
		return imagemsg.Fields{Work: "辨識的作品"}, nil
	}
	api := &hookAPI{Fake: e.fake}
	e.c.API = api
	var wg sync.WaitGroup
	api.hook = func() {
		runInWindow(&wg, func() {
			e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
		})
	}
	e.c.EditSubmit(context.Background(), submit(editID(m), cmUser,
		map[string]string{"work": imagemsg.FailedMarker, "text": "", "characters": "新角色", "keywords": ""}))
	wg.Wait()
	got, _, _ := imagemsg.Parse(e.content(t, m))
	if got.Work != "辨識的作品" || got.Characters != "新角色" {
		t.Errorf("兩邊的修改應都保留，得到 %+v", got)
	}
}

// 偽造 CustomID：頻道寫使用者可用的來源頻道，訊息 ID 指向另一個（使用者不可用）頻道的 bot 圖片訊息 → 不修改。
func TestGK8_ForgedCustomID_MessageFromOtherChannel(t *testing.T) {
	e := newCMEnv(t)
	other := e.fake.AddMessage(&discordgo.Message{
		ChannelID: "secret", Author: &discordgo.User{ID: cmBot, Bot: true},
		Content: imagemsg.Format(imagemsg.Fields{Work: "原本"}, "up"),
	})
	e.c.EditSubmit(context.Background(), submit("editimg:"+cmSource+":"+other.ID, cmUser, formValues(imagemsg.Fields{Work: "改"})))
	if len(e.fake.Edited) != 0 {
		t.Fatal("訊息不屬於 CustomID 的頻道，不應修改")
	}
	deferred(t, e.fake, msgMessageGone)

	e2 := newCMEnv(t)
	other2 := e2.fake.AddMessage(&discordgo.Message{
		ChannelID: "secret", Author: &discordgo.User{ID: cmBot, Bot: true},
		Content: imagemsg.Format(imagemsg.Fields{Work: "原本"}, "up"),
	})
	e2.c.EditSubmit(context.Background(), submit("editimg:secret:"+other2.ID, cmUser, formValues(imagemsg.Fields{Work: "改"})))
	if len(e2.fake.Edited) != 0 {
		t.Fatal("使用者不可用的頻道，不應修改")
	}
	deferred(t, e2.fake, msgNoPermission)
}

// 開表單的權限檢查有時間上限（modal 不能 deferred）：檢查卡住時 ≤ editOpenTimeout 內回覆「目前無法確認權限」。
// （沒有成員資料的快速路徑：blockingAccess 不支援 CanViewMember。）
type blockingAccess struct{ deadline atomic.Value }

func (b *blockingAccess) CheckView(ctx context.Context, userID, channelID string) (bool, error) {
	if d, ok := ctx.Deadline(); ok {
		b.deadline.Store(d)
	}
	<-ctx.Done()
	return false, ctx.Err()
}

func TestGK8_EditOpen_PermissionCheckBounded(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "作品"}, "up")
	ba := &blockingAccess{}
	e.c.Access = ba
	start := time.Now()
	e.c.Edit(context.Background(), menu(CmdEditImage, cmUser, m))
	if el := time.Since(start); el > 2500*time.Millisecond {
		t.Errorf("權限檢查卡住時應在 2 秒左右回覆，花了 %v", el)
	}
	if _, ok := ba.deadline.Load().(time.Time); !ok {
		t.Error("權限檢查的 ctx 應帶期限")
	}
	immediate(t, e.fake, msgPermissionUnknown) // 階段 8 的使用者決定：逾時回「目前無法確認權限」
}

// 別人的 bot 圖片訊息以外的訊息（例如別人發的、bot 發的非圖片訊息）右鍵「編輯」→ 提示，不開表單。
func TestGK8_Edit_BotNonImageMessage(t *testing.T) {
	e := newCMEnv(t)
	m := e.fake.AddMessage(&discordgo.Message{ChannelID: cmSource, Author: &discordgo.User{ID: cmBot, Bot: true}, Content: "一般文字"})
	e.c.Edit(context.Background(), menu(CmdEditImage, cmUser, m))
	immediate(t, e.fake, msgNotImageMessage)
}

// BotID 尚未取得（空字串）時，作者 ID 為空的訊息不能被當成 bot 的圖片訊息。
func TestGK8_EmptyBotID_NotTreatedAsBot(t *testing.T) {
	e := newCMEnv(t)
	e.c.BotID = func() string { return "" }
	m := e.fake.AddMessage(&discordgo.Message{ChannelID: cmSource, Author: &discordgo.User{ID: ""},
		Content: imagemsg.Format(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")})
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	immediate(t, e.fake, msgNotImageMessage)
}
