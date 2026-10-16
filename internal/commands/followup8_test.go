package commands

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"discordbot/internal/access"
	"discordbot/internal/discordtest"
	"discordbot/internal/imagemsg"
	"discordbot/internal/pipeline"

	"github.com/bwmarrin/discordgo"
)

// 階段 8 收尾（S1～S5 與排隊期間頻道被移除）的測試。

// 重新處理排隊期間頻道被移除（ErrNotSource）→ 回「這個頻道已不是圖庫頻道」。
func TestRerecognize_NotSourceText(t *testing.T) {
	if msgNotSource != "這個頻道已不是圖庫頻道" {
		t.Errorf("文字 %q", msgNotSource)
	}
	e := newCMEnv(t)
	m := failedOriginal(e, cmUser, true)
	e.pipe.reprocErr = pipeline.ErrNotSource
	e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
	deferred(t, e.fake, msgNotSource)
	if got := e.fake.ReactionsOn(m.ChannelID, m.ID); len(got) != 0 {
		t.Errorf("原訊息不補回 ❌：%v", got)
	}
}

// lockCheckingAPI 在補上回應時記錄訊息鎖是否仍被持有。
type lockCheckingAPI struct {
	*discordtest.Fake
	c          *ContextMenu
	heldOnEdit []int
}

func (a *lockCheckingAPI) InteractionResponseEdit(i *discordgo.Interaction, e *discordgo.WebhookEdit, o ...discordgo.RequestOption) (*discordgo.Message, error) {
	a.heldOnEdit = append(a.heldOnEdit, a.c.locks.lockCount())
	return a.Fake.InteractionResponseEdit(i, e, o...)
}

// S1：寫回訊息與更新索引後先釋放訊息鎖，再補上回應（回應慢時不擋住同一則訊息的其他操作）。
func TestS1_LockReleasedBeforeResponse(t *testing.T) {
	cases := map[string]func(e *cmEnv, m *discordgo.Message){
		"編輯送出": func(e *cmEnv, m *discordgo.Message) {
			e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, formValues(imagemsg.Fields{Work: "改"})))
		},
		"重新辨識": func(e *cmEnv, m *discordgo.Message) {
			e.c.Rerecognize(context.Background(), menu(CmdRerecognize, cmUser, m))
		},
		"訊息已刪除": func(e *cmEnv, m *discordgo.Message) {
			if err := e.fake.ChannelMessageDelete(m.ChannelID, m.ID); err != nil {
				t.Fatal(err)
			}
			e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, formValues(imagemsg.Fields{Work: "改"})))
		},
		"超過上限": func(e *cmEnv, m *discordgo.Message) {
			v := formValues(imagemsg.Fields{Work: "改"})
			v["text"] = strings.Repeat("字", 2000)
			e.c.EditSubmit(context.Background(), submit(editID(m), cmUser, v))
		},
	}
	for name, run := range cases {
		e := newCMEnv(t)
		api := &lockCheckingAPI{Fake: e.fake, c: e.c}
		e.c.API = api
		m := e.imageMessage(imagemsg.Fields{Work: imagemsg.FailedMarker}, "up")
		run(e, m)
		if len(api.heldOnEdit) == 0 {
			t.Errorf("%s：沒有補上回應", name)
		}
		for _, n := range api.heldOnEdit {
			if n != 0 {
				t.Errorf("%s：補上回應時訊息鎖仍被持有（%d）", name, n)
			}
		}
	}
}

// formHas 回傳是否保存了 customID 的表單記錄。
func (c *ContextMenu) formHas(customID string) bool {
	c.formMu.Lock()
	defer c.formMu.Unlock()
	_, ok := c.forms[customID]
	return ok
}

// S2：表單記錄最多 200 筆，超過時丟掉最舊的。
func TestS2_FormRecordsCapped(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	var ids []string
	for n := 0; n < maxFormRecords+1; n++ {
		ids = append(ids, openForm(t, e, m, fmt.Sprintf("i%d", n)))
	}
	if maxFormRecords != 200 {
		t.Errorf("上限 %d，預期 200", maxFormRecords)
	}
	if n := e.c.formCount(); n != maxFormRecords {
		t.Errorf("記錄數 %d，預期 %d", n, maxFormRecords)
	}
	if e.c.formHas(ids[0]) {
		t.Error("最舊的記錄應被丟掉")
	}
	if !e.c.formHas(ids[1]) || !e.c.formHas(ids[len(ids)-1]) {
		t.Error("其他記錄應保留")
	}
}

// S3：別的使用者（也有權限）送出 A 的 CustomID：不取走、不刪除 A 的記錄，自己的送出視為沒有記錄（全部套用）；
// 之後 A 送出仍只套用 A 改過的欄位。
func TestS3_FormRecordBoundToUser(t *testing.T) {
	e := newCMEnv(t)
	e.access.allowed["u2"] = []string{cmSource}
	orig := imagemsg.Fields{Work: "芙莉蓮", Characters: "舊角色", Keywords: "舊關鍵字"}
	m := e.imageMessage(orig, "up")
	idA := openForm(t, e, m, "ia")

	// 期間別人改了角色。
	latest := imagemsg.Format(imagemsg.Fields{Work: "芙莉蓮", Characters: "別人改的", Keywords: "舊關鍵字"}, "up")
	if _, err := e.fake.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: m.ID, Channel: m.ChannelID, Content: &latest}); err != nil {
		t.Fatal(err)
	}
	// u2 拿 A 的 CustomID 送出（沒有自己的記錄 → 全部套用）。
	b := formValues(orig)
	b["keywords"] = "u2的關鍵字"
	e.c.EditSubmit(context.Background(), submit(idA, "u2", b))
	if !e.c.formHas(idA) {
		t.Fatal("不同使用者不應取走記錄")
	}
	got, _, _ := imagemsg.Parse(e.content(t, m))
	if got.Characters != "舊角色" || got.Keywords != "u2的關鍵字" {
		t.Errorf("不同使用者送出應全部套用：%+v", got)
	}

	// 別人改回角色後，A 只改作品送出 → 只套用作品。
	latest = imagemsg.Format(imagemsg.Fields{Work: "芙莉蓮", Characters: "又被改", Keywords: "u2的關鍵字"}, "up")
	if _, err := e.fake.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: m.ID, Channel: m.ChannelID, Content: &latest}); err != nil {
		t.Fatal(err)
	}
	a := formValues(orig)
	a["work"] = "A的作品"
	e.c.EditSubmit(context.Background(), submit(idA, cmUser, a))
	got, _, _ = imagemsg.Parse(e.content(t, m))
	want := imagemsg.Fields{Work: "A的作品", Characters: "又被改", Keywords: "u2的關鍵字"}
	if got != want {
		t.Errorf("得到 %+v，預期 %+v", got, want)
	}
	if e.c.formHas(idA) {
		t.Error("同一使用者送出後應取走記錄")
	}
}

// S3：沒有通過權限檢查的送出不取走、不刪除記錄。
func TestS3_FormRecordKeptWhenPermissionDenied(t *testing.T) {
	for _, name := range []string{"沒權限", "無法確認"} {
		e := newCMEnv(t)
		m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
		id := openForm(t, e, m, "ia")
		user := "stranger"
		if name == "無法確認" {
			user = cmUser
			e.access.err = discordtest.ErrInjected
		}
		e.c.EditSubmit(context.Background(), submit(id, user, formValues(imagemsg.Fields{Work: "改"})))
		if !e.c.formHas(id) {
			t.Errorf("%s：未通過權限檢查不應取走記錄", name)
		}
	}
}

// S4：快速路徑無法確認（gateway state 尚未載入伺服器或頻道）→「目前無法確認權限」。
func TestS4_FastPathUnknown(t *testing.T) {
	e := newCMEnv(t)
	m := e.imageMessage(imagemsg.Fields{Work: "芙莉蓮"}, "up")
	e.access.memberErr = discordtest.ErrInjected
	e.c.Edit(context.Background(), menu(CmdEditImage, cmUser, m))
	immediate(t, e.fake, msgPermissionUnknown)
	if e.c.formCount() != 0 {
		t.Error("不應開表單")
	}
}

// S5：判斷欄位是否改過時兩邊先正規化（修剪前後空白、統一換行為 \n）：只差空白或換行格式視為沒改。
func TestS5_ChangeDetectionNormalizes(t *testing.T) {
	e := newCMEnv(t)
	orig := imagemsg.Fields{Work: "芙莉蓮", Text: "第一行\n第二行", Characters: "舊角色"}
	m := e.imageMessage(orig, "up")
	id := openForm(t, e, m, "ia")
	latest := imagemsg.Format(imagemsg.Fields{Work: "別人的作品", Text: "別人的文字", Characters: "舊角色"}, "up")
	if _, err := e.fake.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: m.ID, Channel: m.ChannelID, Content: &latest}); err != nil {
		t.Fatal(err)
	}
	v := formValues(orig)
	v["work"] = " 芙莉蓮 "
	v["text"] = "第一行\r\n第二行\n"
	v["characters"] = "新角色"
	e.c.EditSubmit(context.Background(), submit(id, cmUser, v))
	got, _, _ := imagemsg.Parse(e.content(t, m))
	want := imagemsg.Fields{Work: "別人的作品", Text: "別人的文字", Characters: "新角色"}
	if got != want {
		t.Errorf("得到 %+v，預期 %+v", got, want)
	}
}

// *access.Resolver 必須滿足快速路徑的介面（簽名不符時型別斷言會靜默失敗、退回 REST）。
var _ memberAccess = (*access.Resolver)(nil)
