package commands

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"discordbot/internal/discordtest"
	"discordbot/internal/imagemsg"
	"discordbot/internal/index"
	"discordbot/internal/pipeline"

	"github.com/bwmarrin/discordgo"
)

// 階段 9 收尾：使用者決定與 PM 決定（把關建議 1～6）。

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// PM 決定 6：提示文字以 UTF-16 長度 ≤ 100 為上限（emoji 等 BMP 以外的字算 2）。
func TestChoiceName_TruncatesByUTF16(t *testing.T) {
	long := strings.Repeat("😀", 80)
	for _, e := range []index.Entry{
		{Fields: imagemsg.Fields{Work: long}, AttachmentCount: 1},
		{Kind: index.KindLegacy, LegacyText: long, Number: 3, AttachmentCount: 3, Key: index.Key{Attachment: 2}},
	} {
		got := ChoiceName(e)
		if n := utf16Len(got); n > 100 || n < 95 {
			t.Errorf("UTF-16 長度 %d：%q", n, got)
		}
		if !strings.Contains(got, "…") {
			t.Errorf("截斷應加「…」：%q", got)
		}
	}
	// 剛好 100 個 UTF-16 單位不截斷。
	exact := strings.Repeat("😀", 50)
	if got := ChoiceName(index.Entry{Kind: index.KindLegacy, LegacyText: exact, AttachmentCount: 1}); got != exact {
		t.Errorf("剛好 100 不截斷：%q", got)
	}
}

// PM 決定 5：舊圖片訊息的編號只計圖片附件（用 Entry.Number）。
func TestChoiceName_UsesImageNumber(t *testing.T) {
	e := index.Entry{Kind: index.KindLegacy, LegacyText: "舊的", Key: index.Key{Attachment: 2}, Number: 2, AttachmentCount: 2}
	if got := ChoiceName(e); got != "舊的（第 2 張）" {
		t.Errorf("得到 %q", got)
	}
}

// 使用者決定：只打空白、沒選或值不是字串 → ephemeral「請輸入關鍵字，或從提示中選擇一張圖片」，不貼圖。
func TestSearchSubmit_EmptyQueryAsksForKeyword(t *testing.T) {
	for name, mk := range map[string]func() *discordgo.InteractionCreate{
		"空字串":  func() *discordgo.InteractionCreate { return searchSubmit(srUser, "") },
		"只有空白": func() *discordgo.InteractionCreate { return searchSubmit(srUser, " 　\t ") },
		"非字串": func() *discordgo.InteractionCreate {
			i := searchSubmit(srUser, "")
			data := i.Data.(discordgo.ApplicationCommandInteractionData)
			data.Options[0].Value = float64(1)
			i.Data = data
			return i
		},
		"沒有參數": func() *discordgo.InteractionCreate {
			i := searchSubmit(srUser, "")
			data := i.Data.(discordgo.ApplicationCommandInteractionData)
			data.Options = nil
			i.Data = data
			return i
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newSREnv(t)
			e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "x")
			e.s.Submit(context.Background(), mk())
			immediate(t, e.fake, msgSearchEmptyQuery)
			if e.fake.CallCount("ChannelMessage") != 0 || len(e.fetch.fetched()) != 0 {
				t.Error("不應讀訊息或下載")
			}
		})
	}
}

// PM 決定 1：附件序號越界 → 「找不到」，只移除索引中該筆附件（同訊息其他圖保留）。
func TestSearchSubmit_AttachmentOutOfRangeRemovesOnlyThatEntry(t *testing.T) {
	e := newSREnv(t)
	m := e.legacy(srChannel, "舊的", "A", "B")
	m.Attachments = m.Attachments[:1]
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":1"))
	failedAfterDefer(t, e.fake, msgSearchImageGone)
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Attachment: 1}); ok {
		t.Error("越界的附件應從索引移除")
	}
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Attachment: 0}); !ok {
		t.Error("同訊息的其他圖應保留")
	}
}

// PM 決定 2：附件 ID 與索引不符（附件被換掉）→ 「找不到」、不貼圖、移除該筆。
func TestSearchSubmit_AttachmentIDChanged(t *testing.T) {
	e := newSREnv(t)
	m := e.legacy(srChannel, "舊的", "A", "B")
	m.Attachments[0] = m.Attachments[1] // 第 0 個位置變成另一個附件
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	failedAfterDefer(t, e.fake, msgSearchImageGone)
	if len(e.fetch.fetched()) != 0 {
		t.Error("不應下載")
	}
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Attachment: 0}); ok {
		t.Error("應從索引移除該筆")
	}
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID, Attachment: 1}); !ok {
		t.Error("其他附件應保留")
	}
}

// PM 決定 2：附件已不是圖片 → 「找不到」、不貼圖、移除該筆。
func TestSearchSubmit_AttachmentNoLongerImage(t *testing.T) {
	e := newSREnv(t)
	m := e.legacy(srChannel, "舊的", "A")
	m.Attachments[0].ContentType = "application/zip"
	m.Attachments[0].Filename = "a.zip"
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	failedAfterDefer(t, e.fake, msgSearchImageGone)
	if len(e.fetch.fetched()) != 0 {
		t.Error("不應下載")
	}
	if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID}); ok {
		t.Error("應從索引移除該筆")
	}
}

// 使用者決定：下載／上傳失敗區分四種原因。
func TestSearchSubmit_PostFailureReasons(t *testing.T) {
	restErr := func(status, code int) error {
		e := &discordgo.RESTError{Response: &http.Response{StatusCode: status, Status: http.StatusText(status)}}
		if code != 0 {
			e.Message = &discordgo.APIErrorMessage{Code: code}
		}
		return e
	}
	cases := []struct {
		name  string
		setup func(e *srEnv, m *discordgo.Message)
		want  string
	}{
		{"超過 25MiB", func(e *srEnv, m *discordgo.Message) { m.Attachments[0].Size = pipeline.MaxAttachmentSize + 1 }, msgSearchTooLarge},
		{"下載失敗", func(e *srEnv, m *discordgo.Message) { e.fetch.err = discordtest.ErrInjected }, msgSearchDownloadFailed},
		// 10/28 Task 1：「長度不符」不再是失敗原因（改為照常貼出，見 TestSearchSubmit_SizeMismatchStillPosts）。
		{"上傳 413", func(e *srEnv, m *discordgo.Message) {
			e.fake.FailOn["InteractionResponseEdit"] = restErr(http.StatusRequestEntityTooLarge, 0)
		}, msgSearchUploadTooLarge},
		{"上傳 40005", func(e *srEnv, m *discordgo.Message) {
			e.fake.FailOn["InteractionResponseEdit"] = restErr(http.StatusBadRequest, discordgo.ErrCodeRequestEntityTooLarge)
		}, msgSearchUploadTooLarge},
		{"上傳其他 REST 錯誤", func(e *srEnv, m *discordgo.Message) {
			e.fake.FailOn["InteractionResponseEdit"] = restErr(http.StatusBadRequest, 50035)
		}, msgSearchPostFailed},
		{"上傳網路錯誤", func(e *srEnv, m *discordgo.Message) {
			e.fake.FailOn["InteractionResponseEdit"] = discordtest.ErrInjected
		}, msgSearchPostFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSREnv(t)
			m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "PNG")
			c.setup(e, m)
			e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
			failedAfterDefer(t, e.fake, c.want)
			if _, ok := e.x.Get(index.Key{ChannelID: srChannel, MessageID: m.ID}); !ok {
				t.Error("不應從索引移除")
			}
		})
	}
}

// PM 決定 3：排隊超過上限（約 14 分鐘，deferred token 15 分鐘）就放棄：刪除回應、ephemeral followup，不讀訊息。
func TestSearchSubmit_QueueTimeout(t *testing.T) {
	if defaultSearchMaxWait != 14*time.Minute {
		t.Errorf("預設排隊上限 %v", defaultSearchMaxWait)
	}
	e := newSREnv(t)
	m := e.image(srChannel, imagemsg.Fields{Work: "芙莉蓮"}, "PNG")
	e.s.MaxWait = 30 * time.Millisecond
	release := e.s.acquire(context.Background())
	defer release()
	e.s.Submit(context.Background(), searchSubmit(srUser, srChannel+":"+m.ID+":0"))
	failedAfterDefer(t, e.fake, msgSearchQueueTimeout)
	if e.fake.CallCount("ChannelMessage") != 0 {
		t.Error("逾時不應讀訊息")
	}
}

func TestSearchTexts_Stage9Followup(t *testing.T) {
	cases := map[string]string{
		msgSearchEmptyQuery:     "請輸入關鍵字，或從提示中選擇一張圖片",
		msgSearchTooLarge:       "圖片超過 25MB，無法貼出",
		msgSearchUploadTooLarge: "圖片太大，超過這裡的上傳上限，無法貼出",
		msgSearchDownloadFailed: "下載圖片失敗，請稍後再試",
		msgSearchPostFailed:     "貼圖失敗，請稍後再試",
		msgSearchQueueTimeout:   "目前貼圖的人太多，請稍後再試",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("文字 %q，預期 %q", got, want)
		}
	}
}
