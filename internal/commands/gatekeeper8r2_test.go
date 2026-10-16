package commands

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"discordbot/internal/imagemsg"

	"github.com/bwmarrin/discordgo"
)

// lockCount 回傳目前 messageLocks 中的 key 數（用來確認引用計數不洩漏）。
func (l *messageLocks) lockCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.locks)
}

// 開表單時作品是失敗標記 → 期間被重新辨識補上 → 送出時作品仍是失敗標記（沒改）、只改角色：
// 作品保留辨識結果，角色為使用者的值。
func TestGK8R2_OpenedMarker_FilledMeanwhile_KeepsLatest(t *testing.T) {
	e := newCMEnv(t)
	orig := imagemsg.Fields{Work: imagemsg.FailedMarker, Text: imagemsg.FailedMarker, Characters: "舊角色"}
	m := e.imageMessage(orig, "up")
	id := openForm(t, e, m, "ia")
	e.pipe.recognize = func(*discordgo.MessageAttachment, []imagemsg.Field) (imagemsg.Fields, error) {
		return imagemsg.Fields{Work: "辨識的作品", Text: imagemsg.FailedMarker}, nil
	}
	ri := menu(CmdRerecognize, cmUser, m)
	ri.ID = "ir"
	e.c.Rerecognize(context.Background(), ri)

	v := formValues(orig)
	v["characters"] = "新角色"
	e.c.EditSubmit(context.Background(), submit(id, cmUser, v))
	got, _, _ := imagemsg.Parse(e.content(t, m))
	want := imagemsg.Fields{Work: "辨識的作品", Text: imagemsg.FailedMarker, Characters: "新角色"}
	if got != want {
		t.Errorf("得到 %+v，預期 %+v", got, want)
	}
}

// 開表單時是失敗標記，使用者把它改成自己的值；期間被重新辨識補上 → 使用者實際改過，套用使用者的值。
func TestGK8R2_OpenedMarker_UserChanged_UserWins(t *testing.T) {
	e := newCMEnv(t)
	orig := imagemsg.Fields{Work: imagemsg.FailedMarker}
	m := e.imageMessage(orig, "up")
	id := openForm(t, e, m, "ia")
	e.pipe.recognize = func(*discordgo.MessageAttachment, []imagemsg.Field) (imagemsg.Fields, error) {
		return imagemsg.Fields{Work: "辨識的作品"}, nil
	}
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	v := formValues(orig)
	v["work"] = "使用者的作品"
	e.c.EditSubmit(context.Background(), submit(id, cmUser, v))
	got, _, _ := imagemsg.Parse(e.content(t, m))
	if got.Work != "使用者的作品" {
		t.Errorf("使用者改過的欄位應套用：%+v", got)
	}
}

// 同一則訊息大量並發的編輯送出與重新辨識：結束後鎖的 map 必須清空（引用計數不洩漏），且不死結。
// 也涵蓋各種提早 return 的路徑（沒權限、訊息不存在）。
func TestGK8R2_ConcurrentOps_NoLockLeak(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker, Characters: "舊"}, "up")
	snap := *m // 實際上 Resolved 的訊息是 Discord 另外送來的物件，不與 Fake 儲存的共用
	var wg sync.WaitGroup
	for n := 0; n < 40; n++ {
		wg.Add(3)
		go func(n int) {
			defer wg.Done()
			v := formValues(imagemsg.Fields{Work: imagemsg.FailedMarker})
			v["characters"] = fmt.Sprintf("角色%d", n)
			e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, v))
		}(n)
		go func() {
			defer wg.Done()
			cp := snap
			e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, &cp))
		}()
		go func() {
			defer wg.Done()
			// 不存在的訊息：鎖內 ChannelMessage 失敗提早 return
			e.c.EditSubmit(context.Background(), submit("editimg:"+cmSource+":999999", cmUser, formValues(imagemsg.Fields{})))
		}()
	}
	wg.Wait()
	if n := e.c.locks.lockCount(); n != 0 {
		t.Errorf("所有操作結束後鎖應全部釋放，剩 %d 個", n)
	}
	if n := e.c.formCount(); n != 0 {
		t.Errorf("沒有開表單不應有記錄，得到 %d", n)
	}
}

// 偽造 token：別人（也有權限）送出 A 的 CustomID。
// 原本只記錄行為（記錄未綁定使用者，B 會取走 A 的記錄）；階段 8 收尾依 S3 決定改為斷言：
// 記錄綁定開表單的使用者，不同使用者送出時不取走、也不刪除，A 之後送出仍用得到。
func TestGK8R2_FormRecordNotBoundToUser_Documented(t *testing.T) {
	e := newCMEnv(t)
	e.access.allowed["u2"] = []string{cmSource}
	orig := imagemsg.Fields{Work: "芙莉蓮", Characters: "舊角色"}
	m := e.imageMessage(orig, "up")
	idA := openForm(t, e, m, "ia")
	e.c.EditSubmit(context.Background(), submit(idA, "u2", formValues(orig)))
	if n := e.c.formCount(); n != 1 {
		t.Errorf("他人送出不應取走 A 的記錄，剩 %d 筆", n)
	}
	e.c.EditSubmit(context.Background(), submit(idA, cmUser, formValues(orig)))
	if n := e.c.formCount(); n != 0 {
		t.Errorf("A 送出後應取走記錄，剩 %d 筆", n)
	}
}
